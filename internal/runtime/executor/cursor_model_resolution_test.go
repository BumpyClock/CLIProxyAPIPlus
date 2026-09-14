package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCursorRejectsUnsupportedFamilyBeforeNetwork(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e := NewCursorExecutor(&config.Config{})
		e.openStream = func(string) (cursorStream, error) {
			t.Fatal("unexpected upstream connection")
			return nil, errors.New("unexpected connection")
		}
		auth := &cliproxyauth.Auth{ID: "cursor-resolution-test", Provider: "cursor", Metadata: map[string]any{"access_token": "test"}}
		helps.StoreCursorRoutingModels(auth.ID, []*registry.ModelInfo{{ID: "claude-fable-5-1-thinking-high"}})
		req := cliproxyexecutor.Request{Model: "claude-fable-5-1", Payload: []byte(`{"model":"claude-fable-5-1","messages":[]}`)}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), OriginalRequest: []byte(`{"reasoning":{"effort":"low"}}`)}
		var err error
		if stream {
			_, err = e.ExecuteStream(context.Background(), auth, req, opts)
		} else {
			_, err = e.Execute(context.Background(), auth, req, opts)
		}
		var status cursorStatusErr
		if !errors.As(err, &status) || status.StatusCode() != 400 {
			t.Fatalf("stream=%t: got %v, want 400", stream, err)
		}
	}
}

func TestCursorBuildRequestUsesResolvedFamilyVariant(t *testing.T) {
	payload := []byte(`{"model":"claude-fable-5-1","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"high"}`)
	resolved, err := helps.ResolveCursorModel("claude-fable-5-1", payload, "openai", []*registry.ModelInfo{{ID: "claude-fable-5-1-thinking-high"}})
	if err != nil {
		t.Fatal(err)
	}
	parsed := parseOpenAIRequest(payload)
	params := buildRunRequestParams(parsed, "test", resolved)
	if params.ModelId != "claude-fable-5-1-thinking-high" || parsed.Model != "claude-fable-5-1" {
		t.Fatalf("upstream=%s response=%s", params.ModelId, parsed.Model)
	}
}
