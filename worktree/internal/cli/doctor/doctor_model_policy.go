package doctor

import (
	"fmt"
	"sort"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func CheckModelPolicy(id string, cfg workflowconfig.Config) Check {
	policy := cfg.EffectiveModelSelection()
	var levels []string
	for level := range policy.Levels {
		levels = append(levels, level)
	}
	sort.Strings(levels)
	var details []string
	for _, name := range levels {
		level := policy.Levels[name]
		alias := policyString(level.Model)
		model := policy.Model(alias)
		modelSource := policySource(policy, "levels."+name+".model")
		if alias == "normal" || alias == "complex" {
			modelSource += "; " + alias + "_model=" + policySource(policy, alias+"_model")
		}
		details = append(details, fmt.Sprintf("level %s: model=%s (source=%s), effort=%s (source=%s)", name, model, modelSource, policyString(level.Effort), policySource(policy, "levels."+name+".effort")))
	}
	if !policy.Active() {
		details = append(details, "automatic selection disabled; backend/route defaults apply")
	}
	if len(levels) == 0 {
		details = append(details, "no model-selection levels configured")
	}
	return Check{
		Name:   "Project " + id + " model_policy",
		Status: OK,
		Detail: strings.Join(details, "; "),
		Hint:   "Compare printed policy sources with the operator's intended fleet policy.",
	}
}

func policyString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func policySource(policy workflowconfig.ModelSelection, key string) string {
	if source := policy.Sources[key]; source != "" {
		return source
	}
	return "unspecified"
}
