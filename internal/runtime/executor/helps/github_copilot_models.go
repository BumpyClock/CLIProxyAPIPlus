package helps

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

// EnrichGitHubCopilotModels labels the route and fills missing reasoning metadata
// from exact canonical matches, preserving existing Copilot restrictions.
func EnrichGitHubCopilotModels(models []*registry.ModelInfo, catalog []byte) {
	for _, model := range models {
		name := model.DisplayName
		if name == "" {
			name = model.ID
		}
		if !strings.HasPrefix(name, "GH ") {
			model.DisplayName = "GH " + name
		}
	}
	var payload struct {
		Models []struct {
			Slug   string `json:"slug"`
			Levels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(catalog, &payload); err != nil {
		return
	}
	levelsByID := make(map[string][]string, len(payload.Models))
	for _, model := range payload.Models {
		for _, level := range model.Levels {
			levelsByID[model.Slug] = append(levelsByID[model.Slug], level.Effort)
		}
	}
	for _, model := range models {
		if model.Thinking != nil {
			continue
		}
		if levels := levelsByID[model.ID]; len(levels) > 0 {
			model.Thinking = &registry.ThinkingSupport{Levels: append([]string(nil), levels...)}
		} else if canonical := registry.LookupStaticModelInfo(model.ID); canonical != nil && canonical.ID == model.ID {
			model.Thinking = canonical.Thinking
		}
	}
}
