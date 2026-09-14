package helps

import (
	"fmt"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

type CursorModelVariant struct {
	ID       string
	Effort   string
	Thinking bool
	Fast     bool
}

type CursorModelFamily struct {
	ID       string
	Variants []CursorModelVariant
}

func AddCursorModelFamilies(models []*registry.ModelInfo) []*registry.ModelInfo {
	result := append([]*registry.ModelInfo(nil), models...)
	for _, family := range CursorModelFamilies(models) {
		if len(family.Variants) == 1 && family.Variants[0].ID == family.ID {
			continue
		}
		var representative *registry.ModelInfo
		index := -1
		for i, model := range models {
			if model == nil {
				continue
			}
			if model.ID == family.Variants[0].ID {
				representative = model
			}
			if model.ID == family.ID {
				index = i
				representative = model
			}
		}
		if representative == nil {
			continue
		}
		copy := *representative
		copy.ID = family.ID
		if index < 0 {
			copy.DisplayName = family.ID
		}
		levels := []string{}
		seen := map[string]bool{}
		for _, variant := range family.Variants {
			if variant.Effort != "" && !seen[variant.Effort] {
				levels = append(levels, variant.Effort)
				seen[variant.Effort] = true
			}
		}
		if len(levels) > 0 {
			copy.Thinking = &registry.ThinkingSupport{Levels: levels}
			copy.SupportedParameters = append(append([]string(nil), copy.SupportedParameters...), "reasoning_effort")
		}
		if index >= 0 {
			result[index] = &copy
		} else {
			result = append(result, &copy)
		}
	}
	return result
}

func cursorVariant(id string) (string, CursorModelVariant) {
	v := CursorModelVariant{ID: id}
	base := id
	for {
		cut := strings.LastIndexByte(base, '-')
		if cut < 0 {
			break
		}
		switch base[cut+1:] {
		case "fast":
			v.Fast = true
		case "thinking":
			v.Thinking = true
		case "none", "minimal", "low", "medium", "high", "xhigh", "max":
			if v.Effort != "" {
				return base, v
			}
			v.Effort = base[cut+1:]
			if v.Effort == "high" && strings.HasSuffix(base[:cut], "-extra") {
				v.Effort = "xhigh"
				cut -= len("-extra")
			}
		default:
			return base, v
		}
		base = base[:cut]
	}
	return base, v
}

// CursorModelFamilies derives selectable families exclusively from advertised IDs.
func CursorModelFamilies(models []*registry.ModelInfo) []CursorModelFamily {
	groups := map[string][]CursorModelVariant{}
	seen := map[string]bool{}
	for _, model := range models {
		if model == nil || model.ID == "" || seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		base, variant := cursorVariant(model.ID)
		groups[base] = append(groups[base], variant)
	}
	families := make([]CursorModelFamily, 0, len(groups))
	for id, variants := range groups {
		sort.Slice(variants, func(i, j int) bool { return variants[i].ID < variants[j].ID })
		families = append(families, CursorModelFamily{ID: id, Variants: variants})
	}
	sort.Slice(families, func(i, j int) bool { return families[i].ID < families[j].ID })
	return families
}

// ResolveCursorModel preserves explicit variant IDs and maps family options to
// an advertised variant. Effort defaults are ignored for families without effort
// variants; unsupported combinations of advertised dimensions return errors.
func ResolveCursorModel(model string, payload []byte, format string, models []*registry.ModelInfo) (string, error) {
	base, _ := cursorVariant(model)
	if base != model {
		return model, nil
	}
	var variants []CursorModelVariant
	for _, family := range CursorModelFamilies(models) {
		if family.ID == model {
			variants = family.Variants
			break
		}
	}
	if len(variants) == 0 {
		return model, nil
	}
	effort := thinking.ExtractReasoningEffort(payload, format, model)
	// Claude accepts output_config.effort with enabled as well as adaptive thinking.
	if format == "claude" {
		if option := gjson.GetBytes(payload, "output_config.effort"); option.Exists() {
			effort = strings.ToLower(strings.TrimSpace(option.String()))
		}
	}
	thinkingType := gjson.GetBytes(payload, "thinking.type").String()
	fast := false
	for _, path := range []string{"service_tier", "speed"} {
		value := gjson.GetBytes(payload, path).String()
		switch value {
		case "", "auto", "default", "standard":
		case "fast", "priority":
			fast = true
		default:
			return "", fmt.Errorf("cursor model %s does not support %s=%s", model, path, value)
		}
	}
	hasEffort, hasThinking := false, false
	for _, v := range variants {
		hasEffort = hasEffort || v.Effort != ""
		hasThinking = hasThinking || v.Thinking
	}
	// Harnesses can send a default effort even when the model has no such option.
	if !hasEffort && !hasThinking {
		effort, thinkingType = "", ""
	}
	if effort == "auto" {
		effort = ""
	}
	if effort == "" && thinkingType == "" && !fast {
		for _, v := range variants {
			if v.ID == model {
				return model, nil
			}
		}
	}
	wantThinking := hasThinking
	if thinkingType == "disabled" || effort == "none" {
		wantThinking = false
		if thinkingType == "disabled" && !hasThinking {
			effort = "none"
		}
	}
	if thinkingType == "enabled" || thinkingType == "adaptive" || thinkingType == "auto" {
		if effort == "none" {
			return "", fmt.Errorf("cursor model %s: enabled thinking conflicts with effort none", model)
		}
		wantThinking = hasThinking
	}
	if thinkingType != "" && thinkingType != "disabled" && thinkingType != "enabled" && thinkingType != "adaptive" && thinkingType != "auto" {
		return "", fmt.Errorf("cursor model %s: unsupported thinking type %s", model, thinkingType)
	}
	best, bestScore := "", -1
	for _, v := range variants {
		if v.Fast != fast || v.Thinking != wantThinking {
			continue
		}
		if effort != "" && effort != "none" && v.Effort != effort {
			continue
		}
		if effort == "none" && !hasThinking && v.Effort != "none" && v.Effort != "" {
			continue
		}
		score := 0
		switch v.Effort {
		case "medium":
			score = 7
		case "high":
			score = 6
		case "":
			score = 5
		case "low":
			score = 4
		case "minimal":
			score = 3
		case "xhigh":
			score = 2
		case "max":
			score = 1
		}
		if score > bestScore {
			best, bestScore = v.ID, score
		}
	}
	if best != "" {
		return best, nil
	}
	return "", fmt.Errorf("cursor model %s has no advertised variant for effort=%q thinking=%t fast=%t", model, effort, wantThinking, fast)
}
