package templates

import (
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/agentidentity"
)

const runtimeIdentityBadgeModelRunes = 20

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

func runtimeIdentityBadgeSummary(identity agentidentity.Identity, includeBackend bool) string {
	identity = identity.Normalize()
	parts := make([]string, 0, 3)
	if includeBackend {
		if backend := runtimeIdentitySystemName(identity.BackendKind); backend != "" {
			parts = append(parts, backend)
		}
	}
	for _, value := range []string{middleTruncate(identity.Model(), runtimeIdentityBadgeModelRunes), identity.ReasoningEffort.Value} {
		if value = strings.TrimSpace(value); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " · ")
}

func middleTruncate(value string, maxRunes int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return value
	}
	if maxRunes == 1 {
		return "…"
	}
	remaining := maxRunes - 1
	front := (remaining + 1) / 2
	back := remaining - front
	return string(runes[:front]) + "…" + string(runes[len(runes)-back:])
}

func runtimeIdentityFlyoutDetail(identity agentidentity.Identity, threadID string, detentSessionID int64) string {
	identity = identity.Normalize()
	parts := make([]string, 0, 4)
	if identity.Provider.Known() {
		parts = append(parts, "Provider: "+identity.Provider.Value)
	}
	if threadID = strings.TrimSpace(threadID); threadID != "" {
		parts = append(parts, "Thread: "+threadID)
	}
	if identity.Role != "" {
		parts = append(parts, "Role: "+identity.Role)
	}
	if detentSessionID > 0 {
		parts = append(parts, "Session: "+strconv.FormatInt(detentSessionID, 10))
	}
	detail := strings.Join(parts, " · ")
	if detail == "" {
		return runtimeIdentitySummary(identity)
	}
	return detail
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
