package tracker

import "strings"

// Per-attempt usage (decisions section 17.5). A runner reports what a turn
// spent; the execution accumulates the attempt's running total and the hub
// stores it per day, provider and model.

// Provider identifiers the usage report publishes. A backend kind is
// normalized into one of these, so "claude_code" and "claude-code" are one
// provider on the report.
const (
	UsageProviderCodex   = "codex"
	UsageProviderClaude  = "claude"
	UsageProviderUnknown = "unknown"
)

// NativeUsage is what one attempt spent with one provider and model. Input
// counts every input token; CachedInput is the part of it the provider
// served from its cache. CostEstimate is what the runner priced the usage
// at; when it is zero the hub prices it from its own table.
type NativeUsage struct {
	Provider     string  `json:"provider"`
	Model        string  `json:"model"`
	Input        int64   `json:"input"`
	CachedInput  int64   `json:"cached_input"`
	Output       int64   `json:"output"`
	CostEstimate float64 `json:"cost_estimate"`
	Currency     string  `json:"currency"`
}

// Tokens is what the usage report counts as processed: every input token
// plus every output token.
func (u NativeUsage) Tokens() int64 { return u.Input + u.Output }

// UncachedInput is the input the provider had to read. A provider that
// reports cached tokens alongside input rather than inside it would make
// the difference negative, which is reported as none rather than as debt.
func (u NativeUsage) UncachedInput() int64 {
	if u.CachedInput >= u.Input {
		return 0
	}
	return u.Input - u.CachedInput
}

// UsageProviderID normalizes an agent backend kind into a report provider.
func UsageProviderID(backendKind string) string {
	normalized := strings.ToLower(strings.TrimSpace(backendKind))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	switch normalized {
	case "":
		return UsageProviderUnknown
	case "claude_code", "claude":
		return UsageProviderClaude
	default:
		return normalized
	}
}

// UsageProviderLabel is how the report names a provider. A provider the hub
// does not know is labelled by its own identifier rather than invented.
func UsageProviderLabel(provider string) string {
	switch provider {
	case UsageProviderCodex:
		return "Codex"
	case UsageProviderClaude:
		return "Claude Code"
	default:
		return provider
	}
}
