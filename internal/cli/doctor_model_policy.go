package cli

import (
	"fmt"
	"sort"
	"strings"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func checkDoctorModelPolicy(id string, cfg workflowconfig.Config) doctorCheck {
	policy := cfg.EffectiveModelSelection()
	var levels []string
	for level := range policy.Levels {
		levels = append(levels, level)
	}
	sort.Strings(levels)
	var details []string
	for _, name := range levels {
		level := policy.Levels[name]
		alias := doctorPolicyString(level.Model)
		model := policy.Model(alias)
		modelSource := doctorPolicySource(policy, "levels."+name+".model")
		if alias == "normal" || alias == "complex" {
			modelSource += "; " + alias + "_model=" + doctorPolicySource(policy, alias+"_model")
		}
		details = append(details, fmt.Sprintf("level %s: model=%s (source=%s), effort=%s (source=%s)", name, model, modelSource, doctorPolicyString(level.Effort), doctorPolicySource(policy, "levels."+name+".effort")))
	}
	if !policy.Active() {
		details = append(details, "automatic selection disabled; backend/route defaults apply")
	}
	if len(levels) == 0 {
		details = append(details, "no model-selection levels configured")
	}
	return doctorCheck{
		Name:   "Project " + id + " model_policy",
		Status: doctorOK,
		Detail: strings.Join(details, "; "),
		Hint:   "Compare printed policy sources with the operator's intended fleet policy.",
	}
}

func doctorPolicyString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func doctorPolicySource(policy workflowconfig.ModelSelection, key string) string {
	if source := policy.Sources[key]; source != "" {
		return source
	}
	return "unspecified"
}
