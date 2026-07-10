package templates

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/agentidentity"
)

func runtimeIdentitySystemName(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "codex":
		return "Codex"
	case "claude_code":
		return "Claude Code"
	default:
		return strings.TrimSpace(kind)
	}
}

func runtimeIdentitySummary(identity agentidentity.Identity) string {
	identity = identity.Normalize()
	parts := make([]string, 0, 4)
	for _, value := range []string{
		runtimeIdentitySystemName(identity.BackendKind),
		identity.Provider.Value,
		identity.Model(),
		identity.ReasoningEffort.Value,
	} {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " · ")
}

func runtimeIdentityCardSummary(identity agentidentity.Identity) string {
	identity = identity.Normalize()
	parts := make([]string, 0, 2)
	for _, value := range []string{identity.Model(), identity.ReasoningEffort.Value} {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " · ")
}

func runtimeIdentityDetailValue(value agentidentity.Value) string {
	value = value.Normalize()
	provenance := strings.TrimSpace(string(value.Provenance))
	if provenance == "" {
		provenance = string(agentidentity.ProvenanceUnknown)
	}
	if !value.Known() {
		return "Unavailable · " + provenance
	}
	return value.Value + " · " + provenance
}

func runtimeIdentityString(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Unavailable"
	}
	return value
}

func runtimeIdentityModelValue(identity agentidentity.Identity) agentidentity.Value {
	identity = identity.Normalize()
	if identity.ResolvedModel.Known() {
		return identity.ResolvedModel
	}
	return identity.RequestedModel
}
