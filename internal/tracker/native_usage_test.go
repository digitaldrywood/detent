package tracker

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
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
	for _, key := range []string{"usage", "process_usage"} {
		if _, present := decoded[key]; present {
			t.Fatalf("run data without usage encoded %s", encoded)
		}
	}
}

func TestNativeRunDataUsageRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		usage *procgroup.Usage
		want  *procgroup.Usage
	}{
		{name: "darwin bytes", usage: procgroup.NormalizeUsage("darwin", 2048, 1250*time.Millisecond, 250*time.Millisecond, 3*time.Second), want: &procgroup.Usage{PeakMemoryBytes: 2048, UserCPUSeconds: 1.25, SystemCPUSeconds: 0.25, WallSeconds: 3}},
		{name: "linux KiB", usage: procgroup.NormalizeUsage("linux", 2048, 1250*time.Millisecond, 250*time.Millisecond, 3*time.Second), want: &procgroup.Usage{PeakMemoryBytes: 2097152, UserCPUSeconds: 1.25, SystemCPUSeconds: 0.25, WallSeconds: 3}},
		{name: "unsupported", usage: procgroup.NormalizeUsage("windows", 2048, time.Second, time.Second, time.Second)},
		{name: "nil process state", usage: procgroup.UsageFromState(nil, time.Second)},
		{name: "missing system usage", usage: procgroup.UsageFromState(&os.ProcessState{}, time.Second)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			data := NativeRunData{
				LeaseID: "lease_1", RunID: "run_1", AttemptID: "att_1",
				ProcessUsage: test.usage,
				Usage:        []NativeUsage{{Provider: "codex", Model: "gpt-6-astra", Input: 1200, CachedInput: 900, Output: 340, CostEstimate: 0.0125, Currency: "USD"}},
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
			if !reflect.DeepEqual(decoded.ProcessUsage, test.want) {
				t.Fatalf("process usage round trip = %+v, want %+v", decoded.ProcessUsage, test.want)
			}
			var wire struct {
				Usage        []map[string]any `json:"usage"`
				ProcessUsage map[string]any   `json:"process_usage"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatalf("decode wire: %v", err)
			}
			for _, key := range []string{"provider", "model", "input", "cached_input", "output", "cost_estimate", "currency"} {
				if _, present := wire.Usage[0][key]; !present {
					t.Fatalf("usage entry is missing %q: %s", key, encoded)
				}
			}
			if test.want != nil {
				for _, key := range []string{"peak_memory_bytes", "user_cpu_seconds", "system_cpu_seconds", "wall_seconds"} {
					if _, present := wire.ProcessUsage[key]; !present {
						t.Fatalf("process usage is missing %q: %s", key, encoded)
					}
				}
			} else if wire.ProcessUsage != nil {
				t.Fatalf("unsupported process usage encoded %s", encoded)
			}
		})
	}
}

func TestUsageProvider(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, kind, id, label string }{
		{name: "codex", kind: "codex", id: "codex", label: "Codex"},
		{name: "Pi", kind: "pi_agent", id: "pi_agent", label: "Pi"},
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
