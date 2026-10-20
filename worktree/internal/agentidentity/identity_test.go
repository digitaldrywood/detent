package agentidentity

import (
	"testing"
	"time"
)

func TestIdentityMergePreservesRequestedModel(t *testing.T) {
	t.Parallel()

	configuredAt := time.Date(2026, time.July, 9, 12, 0, 0, 0, time.FixedZone("CDT", -5*60*60))
	runtimeAt := configuredAt.Add(time.Minute)
	configured := Configured("codex-high", "codex", "high", "code", "gpt-5.5", "", "", "", configuredAt)
	resolved := configured.Merge(RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "priority", runtimeAt))

	if got := resolved.RequestedModel; got != (Value{Value: "gpt-5.5", Provenance: ProvenanceConfigured}) {
		t.Fatalf("RequestedModel = %#v, want configured gpt-5.5", got)
	}
	if got := resolved.ResolvedModel; got != (Value{Value: "gpt-5.6-sol", Provenance: ProvenanceRuntime}) {
		t.Fatalf("ResolvedModel = %#v, want runtime gpt-5.6-sol", got)
	}
	if !resolved.RequestedDiffers() {
		t.Fatal("RequestedDiffers() = false, want true")
	}
	if resolved.ObservedAt == nil || !resolved.ObservedAt.Equal(runtimeAt) || resolved.ObservedAt.Location() != time.UTC {
		t.Fatalf("ObservedAt = %#v, want %s UTC", resolved.ObservedAt, runtimeAt)
	}
}

func TestConfiguredIdentityDoesNotClaimResolvedModel(t *testing.T) {
	t.Parallel()

	identity := Configured("claude", "claude_code", "default", "code", "fable", "ollama", "high", "", time.Time{})
	if identity.ResolvedModel.Known() {
		t.Fatalf("ResolvedModel = %#v, want unavailable until runtime observation", identity.ResolvedModel)
	}
	if got := identity.Model(); got != "fable" {
		t.Fatalf("Model() = %q, want fable", got)
	}
	if identity.HasRuntimeValues() {
		t.Fatal("HasRuntimeValues() = true, want false")
	}
}

func TestMateriallyEqualIgnoresObservationTime(t *testing.T) {
	t.Parallel()

	first := RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "", time.Unix(1, 0))
	second := RuntimeUpdate("gpt-5.6-sol", "openai", "xhigh", "", time.Unix(2, 0))
	if !first.MateriallyEqual(second) {
		t.Fatalf("MateriallyEqual() = false for timestamp-only change: %#v %#v", first, second)
	}
	if !first.HasRuntimeValues() {
		t.Fatal("HasRuntimeValues() = false, want true")
	}
}
