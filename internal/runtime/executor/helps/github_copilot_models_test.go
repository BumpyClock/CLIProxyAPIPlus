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
