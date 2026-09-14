package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestResolveCursorModel(t *testing.T) {
	models := []*registry.ModelInfo{}
	for _, id := range []string{"claude-fable-5-1-medium", "claude-fable-5-1-thinking-medium", "claude-fable-5-1-thinking-high", "claude-fable-5-1-thinking-high-fast", "composer-2.5", "composer-2.5-fast", "cursor-grok-4.6-high", "cursor-grok-4.6-xhigh"} {
		models = append(models, &registry.ModelInfo{ID: id})
	}
	for _, tc := range []struct {
		name, model, format, body, want string
		fail                            bool
	}{
		{"response", "claude-fable-5-1", "openai-response", `{"reasoning":{"effort":"high"}}`, "claude-fable-5-1-thinking-high", false},
		{"chat", "cursor-grok-4.6", "openai", `{"reasoning_effort":"xhigh"}`, "cursor-grok-4.6-xhigh", false},
		{"claude", "claude-fable-5-1", "claude", `{"thinking":{"type":"enabled"},"output_config":{"effort":"high"}}`, "claude-fable-5-1-thinking-high", false},
		{"fast", "claude-fable-5-1", "openai-response", `{"reasoning":{"effort":"high"},"service_tier":"priority"}`, "claude-fable-5-1-thinking-high-fast", false},
		{"none", "claude-fable-5-1", "openai-response", `{"reasoning":{"effort":"none"}}`, "claude-fable-5-1-medium", false},
		{"default", "composer-2.5", "claude", `{}`, "composer-2.5", false},
		{"composer fast", "composer-2.5", "claude", `{"speed":"fast"}`, "composer-2.5-fast", false},
		{"exact", "claude-fable-5-1-thinking-high", "claude", `{"output_config":{"effort":"low"}}`, "claude-fable-5-1-thinking-high", false},
		{"unsupported", "claude-fable-5-1", "openai-response", `{"reasoning":{"effort":"low"}}`, "", true},
		{"unsupported fast", "cursor-grok-4.6", "openai", `{"service_tier":"priority"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveCursorModel(tc.model, []byte(tc.body), tc.format, models)
			if (err != nil) != tc.fail || got != tc.want {
				t.Fatalf("got %q, %v; want %q, failure=%v", got, err, tc.want, tc.fail)
			}
		})
	}
	augmented := AddCursorModelFamilies(models)
	if len(augmented) != len(models)+2 {
		t.Fatalf("unexpected family catalog length %d", len(augmented))
	}
	if models[0].Thinking != nil {
		t.Fatal("mutated input")
	}
}

func TestCursorClaudeOptionsForEffortOnlyFamilies(t *testing.T) {
	models := []*registry.ModelInfo{{ID: "gpt-5.6-sol-high"}, {ID: "gpt-5.6-sol-medium"}, {ID: "gpt-5.6-sol-none"}}
	for _, tc := range []struct{ body, want string }{
		{`{"thinking":{"type":"enabled"},"output_config":{"effort":"high"}}`, "gpt-5.6-sol-high"},
		{`{"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}}`, "gpt-5.6-sol-medium"},
		{`{"thinking":{"type":"enabled","budget_tokens":16384}}`, "gpt-5.6-sol-high"},
		{`{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, "gpt-5.6-sol-none"},
	} {
		got, err := ResolveCursorModel("gpt-5.6-sol", []byte(tc.body), "claude", models)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %s, %v; want %s", tc.body, got, err, tc.want)
		}
	}
}

func TestAddCursorModelFamilies(t *testing.T) {
	models := []*registry.ModelInfo{
		{ID: "composer-2.5", DisplayName: "Composer", ContextLength: 200000},
		{ID: "composer-2.5-fast"},
		{ID: "claude-opus-5-thinking-high", ContextLength: 1000000},
		{ID: "claude-opus-5-low"},
		{ID: "claude-opus-5-thinking-high"},
	}
	got := AddCursorModelFamilies(models)
	if len(got) != 6 {
		t.Fatalf("length %d", len(got))
	}
	for i, original := range models {
		if got[i].ID != original.ID {
			t.Fatalf("lost original model %s", original.ID)
		}
	}
	family := got[len(got)-1]
	if family.ID != "claude-opus-5" || family.Thinking == nil || len(family.Thinking.Levels) != 2 {
		t.Fatalf("bad family: %+v", family)
	}
	if models[0].DisplayName != "Composer" || models[2].Thinking != nil {
		t.Fatal("mutated inputs")
	}
	if got[0] == models[0] || got[0].ContextLength != 200000 || got[0].DisplayName != "Composer" {
		t.Fatal("base model metadata not independently preserved")
	}
	base, variant := cursorVariant("gpt-5.5-extra-high-fast")
	if base != "gpt-5.5" || variant.Effort != "xhigh" || !variant.Fast {
		t.Fatalf("legacy extra-high parsing: %s %+v", base, variant)
	}
}

func TestComposerIgnoresHarnessEffortDefaults(t *testing.T) {
	models := []*registry.ModelInfo{{ID: "composer-2.5"}, {ID: "composer-2.5-fast"}}
	for _, tc := range []struct{ format, body, want string }{
		{"openai-response", `{"reasoning":{"effort":"medium"}}`, "composer-2.5"},
		{"openai", `{"reasoning_effort":"high","service_tier":"priority"}`, "composer-2.5-fast"},
		{"claude", `{"output_config":{"effort":"medium"},"thinking":{"type":"enabled","budget_tokens":16384}}`, "composer-2.5"},
		{"claude", `{"output_config":{"effort":"high"},"speed":"fast"}`, "composer-2.5-fast"},
	} {
		got, err := ResolveCursorModel("composer-2.5", []byte(tc.body), tc.format, models)
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %s, %v; want %s", tc.body, got, err, tc.want)
		}
	}
}

func TestCursorSingletonMetadataUnchanged(t *testing.T) {
	model := &registry.ModelInfo{ID: "gpt-5-mini", DisplayName: "GPT-5 Mini"}
	got := AddCursorModelFamilies([]*registry.ModelInfo{model})
	if len(got) != 1 || got[0] != model {
		t.Fatal("singleton catalog entry changed")
	}
}
