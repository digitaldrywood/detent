package tracker

import (
	"encoding/json"
	"testing"
)

// Usage rides the run event data (decisions section 17.5). The field is
// additive: a runner that reports none produces the same JSON as before.

func TestNativeRunDataUsageIsOmittedWhenEmpty(t *testing.T) {
	t.Parallel()
	encoded, err := json.Marshal(NativeRunData{LeaseID: "lease_1", RunID: "run_1", AttemptID: "att_1"})
	if err != nil {
		t.Fatalf("marshal run data: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode run data: %v", err)
	}
	if _, present := decoded["usage"]; present {
		t.Fatalf("run data without usage encoded %s", encoded)
	}
}

func TestNativeRunDataUsageRoundTrip(t *testing.T) {
	t.Parallel()
	data := NativeRunData{
		LeaseID: "lease_1", RunID: "run_1", AttemptID: "att_1",
		Usage: []NativeUsage{{
			Provider: "codex", Model: "gpt-6-astra",
			Input: 1200, CachedInput: 900, Output: 340,
			CostEstimate: 0.0125, Currency: "USD",
		}},
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal run data: %v", err)
	}
	var decoded NativeRunData
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode run data: %v", err)
	}
	if len(decoded.Usage) != 1 || decoded.Usage[0] != data.Usage[0] {
		t.Fatalf("usage round trip = %+v, want %+v", decoded.Usage, data.Usage)
	}
	var wire struct {
		Usage []map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("decode wire: %v", err)
	}
	for _, key := range []string{"provider", "model", "input", "cached_input", "output", "cost_estimate", "currency"} {
		if _, present := wire.Usage[0][key]; !present {
			t.Fatalf("usage entry is missing %q: %s", key, encoded)
		}
	}
}

func TestUsageProvider(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, kind, id, label string }{
		{name: "codex", kind: "codex", id: "codex", label: "Codex"},
		{name: "claude code", kind: "claude_code", id: "claude", label: "Claude Code"},
		{name: "claude code hyphenated", kind: "Claude-Code", id: "claude", label: "Claude Code"},
		{name: "unknown backend", kind: "gemini", id: "gemini", label: "gemini"},
		{name: "empty", kind: "", id: "unknown", label: "unknown"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			id := UsageProviderID(test.kind)
			if id != test.id {
				t.Fatalf("UsageProviderID(%q) = %q, want %q", test.kind, id, test.id)
			}
			if label := UsageProviderLabel(id); label != test.label {
				t.Fatalf("UsageProviderLabel(%q) = %q, want %q", id, label, test.label)
			}
		})
	}
}

func TestNativeUsageTotals(t *testing.T) {
	t.Parallel()
	usage := NativeUsage{Input: 1000, CachedInput: 700, Output: 200}
	if got := usage.Tokens(); got != 1200 {
		t.Fatalf("Tokens() = %d, want 1200", got)
	}
	if got := usage.UncachedInput(); got != 300 {
		t.Fatalf("UncachedInput() = %d, want 300", got)
	}
	// A provider that reports cached tokens on top of input rather than
	// inside it must not produce a negative uncached count.
	odd := NativeUsage{Input: 100, CachedInput: 400}
	if got := odd.UncachedInput(); got != 0 {
		t.Fatalf("UncachedInput() = %d, want 0", got)
	}
}
