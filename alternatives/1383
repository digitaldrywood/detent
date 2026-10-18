package hubclient

import (
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/procgroup"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// The execution accumulates per-attempt usage (decisions section 17.5): a
// runner reports what one turn spent and the running total rides the next
// run.checkpointed or run.finished.

func TestMergeNativeUsage(t *testing.T) {
	t.Parallel()
	codex := tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 100, CachedInput: 60, Output: 20, CostEstimate: 0.01, Currency: "USD"}
	claude := tracker.NativeUsage{Provider: "claude", Model: "claude-opus-5", Input: 40, CachedInput: 10, Output: 5, CostEstimate: 0.02, Currency: "USD"}
	paid := int64(1000)
	doubled := int64(2000)
	paidEntry := codex
	paidEntry.BillingMode, paidEntry.ReportedCostMicros, paidEntry.CostSource = "metered", &paid, "runner_report"
	paidTotal := paidEntry
	paidTotal.Input, paidTotal.CachedInput, paidTotal.Output, paidTotal.CostEstimate, paidTotal.ReportedCostMicros = 200, 120, 40, 0.02, &doubled
	incomplete := paidTotal
	incomplete.BillingMode, incomplete.ReportedCostMicros, incomplete.CostSource, incomplete.CostCoverage = "unknown", &paid, "runner_report", "partial"
	cases := []struct {
		name     string
		existing []tracker.NativeUsage
		entry    tracker.NativeUsage
		want     []tracker.NativeUsage
	}{

		{name: "reported turn costs accumulate separately", existing: []tracker.NativeUsage{paidEntry}, entry: paidEntry, want: []tracker.NativeUsage{paidTotal}},
		{name: "missing mode and partial cost cannot claim a paid attempt total", existing: []tracker.NativeUsage{paidEntry}, entry: codex, want: []tracker.NativeUsage{incomplete}},
		{name: "first entry", entry: codex, want: []tracker.NativeUsage{codex}},
		{
			name:     "same provider and model adds up",
			existing: []tracker.NativeUsage{codex},
			entry:    codex,
			want: []tracker.NativeUsage{{
				Provider: "codex", Model: "gpt-6-astra", Input: 200, CachedInput: 120, Output: 40,
				CostEstimate: 0.02, Currency: "USD",
			}},
		},
		{
			name:     "another model is its own entry",
			existing: []tracker.NativeUsage{codex},
			entry:    claude,
			want:     []tracker.NativeUsage{codex, claude},
		},
		{
			name:     "a turn that spent nothing is dropped",
			existing: []tracker.NativeUsage{codex},
			entry:    tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra"},
			want:     []tracker.NativeUsage{codex},
		},
		{name: "nothing at all stays nothing", entry: tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := mergeNativeUsage(test.existing, test.entry)
			if len(got) != len(test.want) {
				t.Fatalf("mergeNativeUsage() = %+v, want %+v", got, test.want)
			}
			for i := range got {
				if !reflect.DeepEqual(got[i], test.want[i]) {
					t.Fatalf("mergeNativeUsage()[%d] = %+v, want %+v", i, got[i], test.want[i])
				}
			}
		})
	}
}

// mergeNativeUsage never writes through a slice the caller still holds: an
// event already built from the old total must not change underneath it.
func TestMergeNativeUsageDoesNotAliasTheCaller(t *testing.T) {
	t.Parallel()
	entry := tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 100, Output: 10}
	first := mergeNativeUsage(nil, entry)
	if first == nil {
		t.Fatal("mergeNativeUsage(nil, entry) returned no total")
	}
	snapshot := first
	second := mergeNativeUsage(first, entry)
	if second == nil {
		t.Fatal("mergeNativeUsage(first, entry) returned no total")
	}
	if snapshot[0].Input != 100 {
		t.Fatalf("the earlier total changed to %d, want 100", snapshot[0].Input)
	}
	if second[0].Input != 200 {
		t.Fatalf("the new total = %d, want 200", second[0].Input)
	}
}

func TestNativeExecutionRecordUsage(t *testing.T) {
	t.Parallel()
	execution := &nativeExecution{}
	entry := tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 100, CachedInput: 40, Output: 12, CostEstimate: 0.004, Currency: "USD"}
	// No identity yet: the usage is remembered, not reported.
	if err := execution.RecordUsage(t.Context(), entry); err != nil {
		t.Fatalf("RecordUsage() = %v, want nil", err)
	}
	if err := execution.RecordUsage(t.Context(), entry); err != nil {
		t.Fatalf("RecordUsage() = %v, want nil", err)
	}
	if len(execution.data.Usage) != 1 || execution.data.Usage[0].Input != 200 || execution.data.Usage[0].Output != 24 {
		t.Fatalf("accumulated usage = %+v, want one entry of 200 input and 24 output", execution.data.Usage)
	}
	// A provider or model the runner could not name is not usable usage.
	if err := execution.RecordUsage(t.Context(), tracker.NativeUsage{Input: 10}); err != nil {
		t.Fatalf("RecordUsage(nameless) = %v, want nil", err)
	}
	if len(execution.data.Usage) != 1 {
		t.Fatalf("usage after a nameless report = %+v, want it unchanged", execution.data.Usage)
	}
	first := procgroup.Usage{PeakMemoryBytes: 4096, UserCPUSeconds: 1.25, SystemCPUSeconds: 0.5, WallSeconds: 2}
	execution.RecordProcessUsage(first)
	snapshot := execution.data.ProcessUsage
	if snapshot == nil || *snapshot != first {
		t.Fatalf("recorded process usage = %+v, want %+v", snapshot, first)
	}
	execution.RecordProcessUsage(procgroup.Usage{PeakMemoryBytes: 2048, UserCPUSeconds: 2, SystemCPUSeconds: 0.25, WallSeconds: 3})
	want := procgroup.Usage{PeakMemoryBytes: 4096, UserCPUSeconds: 3.25, SystemCPUSeconds: 0.75, WallSeconds: 5}
	if got := execution.data.ProcessUsage; got == nil || *got != want {
		t.Fatalf("accumulated process usage = %+v, want %+v", got, want)
	}
	if *snapshot != first {
		t.Fatalf("earlier process usage changed to %+v, want %+v", *snapshot, first)
	}

}
