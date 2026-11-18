package management

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

type usageQueueRecord []byte

var usageImportMaxBytes int64 = 20 << 20

type usageExportPayload struct {
	Version    int                      `json:"version"`
	ExportedAt time.Time                `json:"exported_at"`
	Usage      usage.StatisticsSnapshot `json:"usage"`
}

type usageImportPayload struct {
	Version int             `json:"version"`
	Usage   json.RawMessage `json:"usage"`
}

func (r usageQueueRecord) MarshalJSON() ([]byte, error) {
	if json.Valid(r) {
		return append([]byte(nil), r...), nil
	}
	return json.Marshal(string(r))
}

// GetUsageStatistics returns the in-memory request statistics snapshot.
func (h *Handler) GetUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, gin.H{
		"usage":           snapshot,
		"failed_requests": snapshot.FailureCount,
	})
}

// ExportUsageStatistics returns a complete usage snapshot for backup/migration.
func (h *Handler) ExportUsageStatistics(c *gin.Context) {
	var snapshot usage.StatisticsSnapshot
	if h != nil && h.usageStats != nil {
		snapshot = h.usageStats.Snapshot()
	}
	c.JSON(http.StatusOK, usageExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC(),
		Usage:      snapshot,
	})
}

// ImportUsageStatistics restores a previously exported usage snapshot into memory.
func (h *Handler) ImportUsageStatistics(c *gin.Context) {
	if h == nil || h.usageStats == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "usage statistics unavailable"})
		return
	}

	data, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, usageImportMaxBytes))
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "usage import payload too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read request body"})
		return
	}

	var payload usageImportPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json"})
		return
	}
	if payload.Version != 0 && payload.Version != 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported version"})
		return
	}

	var restored usage.StatisticsSnapshot
	v8 := c.GetBool(ConfigV8ContextKey)
	if v8 {
		var totals struct {
			TotalRequests *int64 `json:"total_requests"`
			TotalTokens   *int64 `json:"total_tokens"`
		}
		if errTotals := json.Unmarshal(payload.Usage, &totals); errTotals != nil || totals.TotalRequests == nil || totals.TotalTokens == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage snapshot"})
			return
		}
	}
	if len(payload.Usage) != 0 {
		if errSnapshot := json.Unmarshal(payload.Usage, &restored); errSnapshot != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage snapshot"})
			return
		}
	}
	if v8 && !usageSnapshotCountsValid(restored) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage snapshot"})
		return
	}
	result := h.usageStats.RestoreSnapshot(restored)
	snapshot := h.usageStats.Snapshot()
	c.JSON(http.StatusOK, gin.H{
		"added":           result.Added,
		"skipped":         result.Skipped,
		"total_requests":  snapshot.TotalRequests,
		"failed_requests": snapshot.FailureCount,
	})
}

func usageSnapshotCountsValid(snapshot usage.StatisticsSnapshot) bool {
	if !nonnegativeUsageCounts(snapshot.TotalRequests, snapshot.SuccessCount, snapshot.FailureCount, snapshot.TotalTokens) {
		return false
	}
	for _, buckets := range []map[string]int64{snapshot.RequestsByDay, snapshot.RequestsByHour, snapshot.TokensByDay, snapshot.TokensByHour} {
		for _, count := range buckets {
			if count < 0 {
				return false
			}
		}
	}
	for _, api := range snapshot.APIs {
		if !nonnegativeUsageCounts(api.TotalRequests, api.TotalTokens, api.FailureCount) {
			return false
		}
		for _, model := range api.Models {
			if !nonnegativeUsageCounts(model.TotalRequests, model.TotalTokens, model.FailureCount) {
				return false
			}
			for _, detail := range model.Details {
				tokens := detail.Tokens
				if !nonnegativeUsageCounts(tokens.InputTokens, tokens.OutputTokens, tokens.ReasoningTokens, tokens.CachedTokens, tokens.TotalTokens) {
					return false
				}
			}
		}
	}
	return true
}

func nonnegativeUsageCounts(counts ...int64) bool {
	for _, count := range counts {
		if count < 0 {
			return false
		}
	}
	return true
}

// GetUsageQueue pops queued usage records from the usage queue.
func (h *Handler) GetUsageQueue(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	count, errCount := parseUsageQueueCount(c.Query("count"))
	if errCount != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCount.Error()})
		return
	}

	items := redisqueue.PopOldest(count)
	records := make([]usageQueueRecord, 0, len(items))
	for _, item := range items {
		records = append(records, usageQueueRecord(append([]byte(nil), item...)))
	}

	c.JSON(http.StatusOK, records)
}

func parseUsageQueueCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	count, errCount := strconv.Atoi(value)
	if errCount != nil || count <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return count, nil
}
