package templates

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
)

func TestRuntimeIdentitySummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		identity agentidentity.Identity
		want     string
	}{
		{
			name: "runtime codex identity",
			identity: agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", time.Time{}).
				Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", time.Time{})),
			want: "Codex · openai · gpt-5.6-sol · xhigh",
		},
		{
			name: "unknown compact values omitted",
			identity: agentidentity.Configured("claude-local", "claude_code", "local", "code", "fable", "ollama", "", "", time.Time{}).
				Merge(agentidentity.RuntimeUpdate("qwen3-coder", "", "", "", time.Time{})),
			want: "Claude Code · ollama · qwen3-coder",
		},
		{name: "empty identity", identity: agentidentity.Identity{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := runtimeIdentitySummary(tt.identity); got != tt.want {
				t.Fatalf("runtimeIdentitySummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRuntimeIdentityBadgeSummary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		identity       agentidentity.Identity
		includeBackend bool
		want           string
	}{
		{
			name: "compact runtime identity",
			identity: agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", time.Time{}).
				Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", time.Time{})),
			want: "gpt-5.6-sol · xhigh",
		},
		{
			name: "cozy runtime identity",
			identity: agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", time.Time{}).
				Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", time.Time{})),
			includeBackend: true,
			want:           "Codex · gpt-5.6-sol · xhigh",
		},
		{
			name:     "long model middle truncated",
			identity: agentidentity.Configured("codex-high", "codex", "high", "code", "abcdefghij0123456789ABCDEFGHIJ", "", "xhigh", "", time.Time{}),
			want:     "abcdefghij…BCDEFGHIJ · xhigh",
		},
		{name: "empty identity", identity: agentidentity.Identity{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := runtimeIdentityBadgeSummary(tt.identity, tt.includeBackend); got != tt.want {
				t.Fatalf("runtimeIdentityBadgeSummary() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRuntimeIdentityFlyoutDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		identity          agentidentity.Identity
		providerSessionID string
		detentSessionID   int64
		want              string
	}{
		{
			name: "all long-tail fields",
			identity: agentidentity.Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", time.Time{}).
				Merge(agentidentity.RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", time.Time{})),
			providerSessionID: "thread-demo-core-5260",
			detentSessionID:   5260,
			want:              "Provider: openai · Provider session: thread-demo-core-5260 · Role: code · Detent session: 5260",
		},
		{
			name:     "summary fallback",
			identity: agentidentity.Identity{BackendKind: "codex", ResolvedModel: agentidentity.NewValue("gpt-5.6-sol", agentidentity.ProvenanceRuntime)},
			want:     "Codex · gpt-5.6-sol",
		},
		{name: "empty detail", identity: agentidentity.Identity{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := runtimeIdentityFlyoutDetail(tt.identity, tt.providerSessionID, tt.detentSessionID); got != tt.want {
				t.Fatalf("runtimeIdentityFlyoutDetail() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRuntimeIdentityDetailValueShowsAvailabilityAndProvenance(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value agentidentity.Value
		want  string
	}{
		{name: "runtime value", value: agentidentity.NewValue("xhigh", agentidentity.ProvenanceRuntime), want: "xhigh · runtime"},
		{name: "configured value", value: agentidentity.NewValue("high", agentidentity.ProvenanceConfigured), want: "high · configured"},
		{name: "unknown value", value: agentidentity.UnknownValue(), want: "Unavailable · unknown"},
		{name: "missing value", value: agentidentity.Value{}, want: "Unavailable · unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := runtimeIdentityDetailValue(tt.value); got != tt.want {
				t.Fatalf("runtimeIdentityDetailValue() = %q, want %q", got, tt.want)
			}
		})
	}
}
