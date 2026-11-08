package nativechange

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The execution accumulates per-attempt usage: a
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
	observed := codex
	observed.SourceID, observed.UsageKind, observed.Revision = "session_42", "incremental", 1
	grown := observed
	grown.Input, grown.Output, grown.Revision = 150, 30, 2
	started := tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Currency: "USD", SourceID: "session_42", UsageKind: "incremental", Revision: 1}
	cases := []struct {
		name     string
		existing []tracker.NativeUsage
		entry    tracker.NativeUsage
		want     []tracker.NativeUsage
	}{

		{name: "witnessed zero usage retains the session", entry: started, want: []tracker.NativeUsage{started}},
		{name: "zero usage replay retains one session", existing: []tracker.NativeUsage{started}, entry: started, want: []tracker.NativeUsage{started}},
		{name: "later tokens replace the zero session", existing: []tracker.NativeUsage{started}, entry: observed, want: []tracker.NativeUsage{observed}},
		{name: "stable observation replay never doubles tokens", existing: []tracker.NativeUsage{observed}, entry: observed, want: []tracker.NativeUsage{observed}},
		{name: "later observation replaces cumulative totals", existing: []tracker.NativeUsage{observed}, entry: grown, want: []tracker.NativeUsage{grown}},
		{name: "older observation cannot lose accepted totals", existing: []tracker.NativeUsage{grown}, entry: observed, want: []tracker.NativeUsage{grown}},
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

func TestNativeUsageBudgetRefusalKeepsTurnLive(t *testing.T) {
	for _, batched := range []bool{false, true} {
		name := "single event"
		if batched {
			name = "observation batch"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now()
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				var requests [][]byte
				var accepted []tracker.NativeRunEvent
				client, err := newTestClient(t, Config{URL: "http://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					body, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					requests = append(requests, body)
					var event tracker.NativeRunEvent
					if err := json.Unmarshal(body, &event); err != nil {
						return nil, err
					}
					if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/events") || len(event.Data.Usage) != 1 {
						t.Fatal("usage did not reach the event publication boundary")
					}
					if ctx.Err() != nil {
						t.Fatal("turn was cancelled during usage publication")
					}
					if len(requests) == 1 {
						return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"60"}}, Body: io.NopCloser(strings.NewReader(`{"code":"runner_request_budget"}`)), Request: request}, nil
					}
					accepted = append(accepted, event)
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"accepted":true}`)), Request: request}, nil
				})}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_example", "prj_example")
				if err != nil {
					t.Fatal(err)
				}
				lease := tracker.NativeLease{RunDetailBatching: batched, WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease", FencingToken: 1}
				claim := nativeClaim{source: fixtureConnector(native), lease: lease, deadline: now.Add(time.Hour)}
				scheduler, err := newTestScheduler(client, SchedulerConfig{Now: time.Now, HeartbeatInterval: time.Second, LeaseTTL: time.Hour, Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}})
				if err != nil {
					t.Fatal(err)
				}
				fixtureInstallClaim(t, scheduler, claim, policy.Descriptor{}, "project")
				execution := &nativeExecution{scheduler: authority(scheduler), claim: claim, runtimePublishedAt: now, cancel: cancel, data: tracker.NativeRunData{AttemptID: "attempt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RunID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", LeaseID: lease.ID, FencingToken: lease.FencingToken, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example", Model: "example-model"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now}}}
				update := func(revision int64) {
					err := execution.RecordUsage(ctx, tracker.NativeUsage{Provider: "example", Model: "example-model", SourceID: "session_1", Revision: revision, Input: revision * 10, Output: revision})
					if err != nil {
						cancel(err)
						t.Fatalf("usage callback cancelled the live turn: %v", err)
					}
				}
				count, batchSize := int64(1), int64(1)
				if batched {
					count, batchSize = 50, 8
				}
				for revision := int64(1); revision <= count; revision++ {
					update(revision)
					if batched && revision < 8 && len(requests) != 0 {
						t.Fatal("changed usage bypassed observation batching")
					}
				}
				if len(requests) != int(count/batchSize)+1 || !bytes.Equal(requests[0], requests[1]) || len(accepted) != int(count/batchSize) {
					t.Fatalf("budget retry changed event identity or sent extra writes: requests=%d accepted=%d", len(requests), len(accepted))
				}
				if time.Since(now) != time.Minute || ctx.Err() != nil || execution.data.Outcome != "" {
					t.Fatalf("budget refusal ended the live turn: elapsed=%s cause=%v", time.Since(now), context.Cause(ctx))
				}
				if len(accepted[0].PriorEvents) != int(batchSize)-1 || accepted[0].Data.Usage[0].Input != batchSize*10 || execution.data.Usage[0].Input != count*10 {
					t.Fatalf("batch lost cumulative usage: %+v", accepted[0])
				}
				update(count + 1)
				execution.storedSeq = execution.data.Sequence + 1
				if err := execution.append(ctx, "run.checkpointed", "", &tracker.NativeCheckpoint{Resume: "fresh_checkout"}); err != nil {
					t.Fatal(err)
				}
				last := accepted[len(accepted)-1]
				if ctx.Err() != nil || last.Type != "run.checkpointed" || last.Data.Usage[0].Input != (count+1)*10 {
					t.Fatalf("live turn could not checkpoint after refusal: %+v cause=%v", last, context.Cause(ctx))
				}
			})
		})
	}
}
