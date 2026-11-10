package helps

import (
	"reflect"
	"testing"

	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestEnrichGitHubCopilotModels(t *testing.T) {
	models := []*registry.ModelInfo{
		{ID: "gpt-6.1-sol", ContextLength: 500000, MaxCompletionTokens: 128000},
		{ID: "gpt-6.1-terra", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}}},
		{ID: "unknown-copilot-model"},
		{ID: "gpt-4o"},
	}
	catalog := []byte(`{"models":[
		{"slug":"gpt-6.1-sol","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"},{"effort":"max"},{"effort":"ultra"}]},
		{"slug":"gpt-6.1-terra","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]}
	]}`)
	EnrichGitHubCopilotModels(models, catalog)
	want := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	if models[0].Thinking == nil || !reflect.DeepEqual(models[0].Thinking.Levels, want) {
		t.Fatalf("canonical reasoning levels missing: %+v", models[0].Thinking)
	}
	if models[0].ContextLength != 500000 || models[0].MaxCompletionTokens != 128000 {
		t.Fatal("Copilot account token limits changed")
	}
	if models[0].ID != "gpt-6.1-sol" || models[0].DisplayName != "GH gpt-6.1-sol" {
		t.Fatal("Copilot display label missing or routing ID changed")
	}
	if !reflect.DeepEqual(models[1].Thinking.Levels, []string{"low", "high"}) {
		t.Fatal("explicit Copilot reasoning restrictions changed")
	}
	if models[2].Thinking != nil || models[3].Thinking != nil {
		t.Fatal("reasoning enabled for an unknown or non-reasoning model")
	}
	EnrichGitHubCopilotModels(models, catalog)
	if !reflect.DeepEqual(models[0].Thinking.Levels, want) {
		t.Fatal("repeated enrichment changed reasoning levels")
	}
	if models[0].DisplayName != "GH gpt-6.1-sol" {
		t.Fatal("repeated enrichment duplicated the GH label")
	}
}

func TestEnrichedCopilotCatalogMatchesCanonicalReasoning(t *testing.T) {
	model := &registry.ModelInfo{ID: "gpt-6-astra", Type: "github-copilot"}
	EnrichGitHubCopilotModels([]*registry.ModelInfo{model}, registry.GetCodexClientModelsJSON())
	model.ID = "copilot/gpt-6-astra"
	r := registry.GetGlobalRegistry()
	const clientID = "copilot-reasoning-enrichment-test"
	r.RegisterClient(clientID, "github-copilot", []*registry.ModelInfo{model})
	t.Cleanup(func() { r.UnregisterClient(clientID) })
	build := func(id, provider string) map[string]any {
		response := codexmodels.BuildResponseForClient([]map[string]any{{"id": id}}, func(string) []string {
			return []string{provider}
		}, false, "pi")
		return response["models"].([]map[string]any)[0]
	}
	canonical := build("gpt-6-astra", "codex")
	copilot := build(model.ID, "github-copilot")
	efforts := func(entry map[string]any) []string {
		var result []string
		for _, level := range entry["supported_reasoning_levels"].([]any) {
			result = append(result, level.(map[string]any)["effort"].(string))
		}
		return result
	}
	levels := efforts(canonical)
	if len(levels) == 0 || !reflect.DeepEqual(levels, efforts(copilot)) {
		t.Fatalf("Copilot reasoning %v differs from canonical %v", efforts(copilot), levels)
	}
}

func TestEnrichGitHubCopilotModelsExactFallback(t *testing.T) {
	tests := []struct {
		id   string
		want *registry.ThinkingSupport
	}{
		{id: "claude-haiku-4-5-20251001", want: &registry.ThinkingSupport{Min: 1024, Max: 128000, ZeroAllowed: true}},
		{id: "claude-opus-5-5-20261001"},
		{id: "claude-opus-5-5[1m]"},
		{id: "copilot/gpt-6-astra"},
		{id: "gpt-6-astra-preview"},
		{id: "unknown-copilot-model"},
		{id: "gpt-4o"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			model := &registry.ModelInfo{ID: tt.id, ContextLength: 128000, MaxCompletionTokens: 16000}
			EnrichGitHubCopilotModels([]*registry.ModelInfo{model}, []byte(`{"models":[]}`))
			if !reflect.DeepEqual(model.Thinking, tt.want) {
				t.Fatalf("thinking = %+v, want %+v from an exact static match", model.Thinking, tt.want)
			}
			if model.ID != tt.id || model.ContextLength != 128000 || model.MaxCompletionTokens != 16000 {
				t.Fatalf("routing ID or account limits changed: %+v", model)
			}
		})
	}
}

func TestEnrichGitHubCopilotModelsPreservesRestrictionsAndLabels(t *testing.T) {
	tests := []struct {
		name     string
		display  string
		thinking *registry.ThinkingSupport
		explicit bool
		wantName string
	}{
		{name: "levels", display: "Copilot Astra", thinking: &registry.ThinkingSupport{Levels: []string{"low", "high"}}, wantName: "GH Copilot Astra"},
		{name: "budget", display: "GH Copilot Astra", thinking: &registry.ThinkingSupport{Min: 1024, Max: 8192, ZeroAllowed: true, DynamicAllowed: true}, wantName: "GH Copilot Astra"},
		{name: "empty support", thinking: &registry.ThinkingSupport{}, wantName: "GH gpt-6-astra"},
		{name: "explicitly disabled", explicit: true, wantName: "GH gpt-6-astra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &registry.ModelInfo{
				ID: "gpt-6-astra", DisplayName: tt.display, Thinking: tt.thinking, ExplicitThinking: tt.explicit,
				ContextLength: 168000, MaxCompletionTokens: 8192,
			}
			want := *model
			want.DisplayName = tt.wantName
			if tt.thinking != nil {
				thinking := *tt.thinking
				thinking.Levels = append([]string(nil), tt.thinking.Levels...)
				want.Thinking = &thinking
			}
			for pass := 0; pass < 2; pass++ {
				EnrichGitHubCopilotModels([]*registry.ModelInfo{model}, registry.GetCodexClientModelsJSON())
				if !reflect.DeepEqual(*model, want) {
					t.Fatalf("pass %d changed Copilot metadata: got %+v, want %+v", pass, *model, want)
				}
			}
		})
	}
}

func TestEnrichGitHubCopilotModelsCatalogPrecedesStatic(t *testing.T) {
	model := &registry.ModelInfo{ID: "gpt-6-astra", ContextLength: 128000, MaxCompletionTokens: 8192}
	catalog := []byte(`{"models":[{"slug":"gpt-6-astra","supported_reasoning_levels":[{"effort":"high"}]}]}`)
	want := registry.ModelInfo{
		ID: "gpt-6-astra", DisplayName: "GH gpt-6-astra", ContextLength: 128000, MaxCompletionTokens: 8192,
		Thinking: &registry.ThinkingSupport{Levels: []string{"high"}},
	}
	for pass := 0; pass < 2; pass++ {
		EnrichGitHubCopilotModels([]*registry.ModelInfo{model}, catalog)
		if !reflect.DeepEqual(*model, want) {
			t.Fatalf("pass %d ignored catalog reasoning or changed Copilot metadata: %+v", pass, model)
		}
	}
}

func TestEnrichGitHubCopilotModelsRejectsMalformedCatalog(t *testing.T) {
	for _, catalog := range []string{
		``,
		`{"models":[`,
		`{"models":{}}`,
		`{"models":[{"slug":"gpt-6-astra","supported_reasoning_levels":[{"effort":"high"}]},{"slug":42}]}`,
	} {
		t.Run(catalog, func(t *testing.T) {
			model := &registry.ModelInfo{ID: "gpt-6-astra", ContextLength: 128000, MaxCompletionTokens: 8192}
			restricted := &registry.ModelInfo{ID: "gpt-6.1-sol", Thinking: &registry.ThinkingSupport{Levels: []string{"low"}}}
			EnrichGitHubCopilotModels([]*registry.ModelInfo{model, restricted}, []byte(catalog))
			want := registry.ModelInfo{ID: "gpt-6-astra", DisplayName: "GH gpt-6-astra", ContextLength: 128000, MaxCompletionTokens: 8192}
			if !reflect.DeepEqual(*model, want) {
				t.Fatalf("invalid catalog changed more than the display label: %+v", model)
			}
			if restricted.DisplayName != "GH gpt-6.1-sol" || !reflect.DeepEqual(restricted.Thinking.Levels, []string{"low"}) {
				t.Fatalf("invalid catalog changed explicit restrictions or skipped labeling: %+v", restricted)
			}
		})
	}
}
