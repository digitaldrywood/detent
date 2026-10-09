package hubclient

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestNativeAttemptTerminalActivityReceipts(t *testing.T) {
	for _, tt := range []struct {
		name, outcome string
		duration      time.Duration
		merge         bool
	}{
		{name: "failed", outcome: "failed", duration: 20 * time.Second},
		{name: "two seconds", outcome: "succeeded", duration: 2 * time.Second},
		{name: "interrupted", outcome: "interrupted", duration: 3 * time.Second},
		{name: "merge", outcome: "failed", duration: 20 * time.Second, merge: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
				{Name: "Done", Terminal: true},
			}, true)
			issue := h.createInProgress(t, tt.name)
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			at := time.Now().UTC().Add(-time.Minute)
			clock := at
			h.scheduler.now = func() time.Time { return clock }
			if tt.merge {
				if err := execution.StartLanding(t.Context(), 42, 2); err != nil {
					t.Fatal(err)
				}
			} else if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			clock = at.Add(tt.duration)
			if err := execution.Finish(t.Context(), tt.outcome); err != nil {
				t.Fatal(err)
			}
			evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
			if err != nil || evidence.Attempt == nil || evidence.Attempt.Runtime == nil || evidence.Attempt.Runtime.Activity == nil {
				t.Fatalf("terminal receipt missing: evidence=%+v err=%v", evidence, err)
			}
			p := evidence.Attempt.Runtime.Activity
			if p.Summary == nil || !p.StartedAt.Equal(at) || !p.FinishedAt.Equal(clock) || !p.Summary.Through.Equal(clock) || p.Breakdown().ElapsedSeconds != tt.duration.Seconds() {
				t.Fatalf("terminal receipt does not cover attempt: %+v", p)
			}
			if tt.merge && p.Breakdown().ObservedSeconds != tt.duration.Seconds() {
				t.Fatalf("programmatic merge time unobserved: %+v", p.Breakdown())
			}
		})
	}
}

func TestNativeRuntimeCheckpointsDoNotGrowHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{
		{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
		{Name: "Done", Terminal: true},
	}, true)
	issue := h.createInProgress(t, "Activity checkpoints")
	item := tracker.NativeWorkItemID(issue.ID)
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
	at := time.Now().UTC()
	timing := (gate.CommandResult{Command: "make check-land", HeadSHA: strings.Repeat("a", 40), TreeSHA: strings.Repeat("b", 40), StartedAt: at, FinishedAt: at.Add(time.Second)}).PipelineTiming("worker_check_land")
	execution.RecordPipelineTiming(timing)
	execution.RecordPipelineTiming(timing)
	profile := workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 42, Generation: 2, SessionID: 43, Stage: "implementation", Status: "running", Coverage: "partial", StartedAt: at, AsOf: at}
	observation := tracker.NativeRuntimeObservation{Recovery: &tracker.NativeRecoveryDecision{Action: "fresh_checkout", Reason: "session_restart_required"}, LocalAttemptID: 42, Generation: 2, Phase: "implementation", HeartbeatAt: at, Activity: &profile,
		GitHub: &tracker.NativeGitHubScope{Scope: "native_landing", StartedAt: at, ObservedAt: at}}
	if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	assertProgress := func(sequence int64, history int) tracker.NativeRuntimeEvidence {
		t.Helper()
		evidence, err := h.admin.RuntimeEvidence(t.Context(), item, "")
		if err != nil || evidence.Attempt == nil || evidence.Attempt.Sequence != sequence {
			t.Fatalf("attempt=%+v, err=%v, want sequence=%d", evidence.Attempt, err, sequence)
		}
		if timings := evidence.Attempt.Runtime.Pipeline; len(timings) != 1 || timings[0].ReceiptID != timing.ReceiptID || timings[0].Stage != "worker_check_land" {
			t.Fatalf("buffered timing lost or duplicated: %+v", timings)
		}
		if recovery := evidence.Attempt.Runtime.Recovery; recovery == nil || recovery.Action != "fresh_checkout" || recovery.Reason != "session_restart_required" {
			t.Fatalf("runtime update lost recovery decision: %#v", evidence.Attempt.Runtime)
		}
		if evidence.Attempt.Runtime.GitHub == nil || evidence.Attempt.Runtime.GitHub.Scope != "native_landing" || evidence.Attempt.Runtime.GitHub.ObservedAt != at {
			t.Fatalf("runtime update lost GitHub observation: %#v", evidence.Attempt.Runtime)
		}
		events, err := h.admin.History(t.Context(), item, "")
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events.Items {
			if strings.HasPrefix(event.Type, "run.") {
				count++
			}
		}
		if count != history {
			t.Fatalf("run history=%d, want %d", count, history)
		}
		return evidence
	}
	for i := range 128 {
		// Exercise normal lease renewal while writing repeated runtime checkpoints.
		if i%16 == 0 {
			if _, err := h.scheduler.RenewClaim(t.Context(), issue.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		observation.GitHub = nil
		observation.Recovery = nil
		profile.AsOf = profile.AsOf.Add(time.Second)
		observation.HeartbeatAt = profile.AsOf
		if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
			t.Fatal(err)
		}
		if err := execution.FlushRuntime(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	assertProgress(1, 1)
	profile.Spans = []workflowmetrics.ActivitySpan{{ID: "tool", Kind: "context_read", Evidence: "read_tool", StartedAt: at, Outcome: "running", Attribution: "observed_read_request"}}
	transport := &executionTransport{next: h.native.client.httpClient.Transport}
	h.native.client.httpClient.Transport = transport
	for i := range 32 {
		profile.Dropped = uint64(i)
		profile.Unpaired = uint64(i)
		if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			transport.drop.Store(true)
			if err := execution.FlushRuntime(t.Context()); err == nil {
				t.Fatal("runtime acknowledgment loss was not injected")
			}
		}
		if err := execution.FlushRuntime(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	evidence := assertProgress(33, 1)
	if activity := evidence.Attempt.Runtime.Activity; len(activity.Spans) != 1 || activity.Dropped != 31 || activity.Unpaired != 31 || activity.Spans[0].StartedAt != at || activity.Spans[0].CausalAttribution != "unknown_provider_origin" {
		t.Fatalf("mutable activity=%+v", activity)
	}
	profile.Spans = nil
	profile.AsOf = at.Add(1060 * time.Second)
	for i := range 1060 {
		span := workflowmetrics.ActivitySpan{ID: "recent-" + strconv.Itoa(i), Kind: "implementation", Outcome: "completed", StartedAt: at.Add(time.Duration(i) * time.Second), FinishedAt: at.Add(time.Duration(i+1) * time.Second)}
		for range 32 {
			span.Actions = append(span.Actions, workflowmetrics.ActivityAction{Type: "read", Kind: "context_read", CausalAttribution: "unknown_provider_origin", Attribution: "observed_read_request", SourceCoverage: "recorder_snapshot"})
		}
		profile.Spans = append(profile.Spans, span)
	}
	if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	if err := execution.FlushRuntime(t.Context()); err != nil {
		t.Fatal(err)
	}
	evidence = assertProgress(34, 1)
	activity := evidence.Attempt.Runtime.Activity
	raw, err := json.Marshal(activity)
	if err != nil || len(raw) > 128*1024 || activity.ProjectionOmitted == 0 || activity.Dropped != 31 || activity.Breakdown().ObservedSeconds != 1060 || activity.Spans[len(activity.Spans)-1].FinishedAt != profile.AsOf {
		t.Fatalf("authenticated bounded whole-attempt activity: bytes=%d profile=%+v err=%v", len(raw), activity, err)
	}
	for i := range 128 {
		// Exercise normal lease renewal while writing repeated runtime checkpoints.
		if i%16 == 0 {
			if _, err := h.scheduler.RenewClaim(t.Context(), issue.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		profile.AsOf = profile.AsOf.Add(time.Second)
		observation.HeartbeatAt = profile.AsOf
		if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
			t.Fatal(err)
		}
		if err := execution.FlushRuntime(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	assertProgress(34, 1)
	observation.Phase = "validation"
	if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	evidence = assertProgress(35, 2)
	if len(evidence.Attempt.Runtime.Phases) != 2 || evidence.Attempt.Runtime.Phases[0].FinishedAt.IsZero() {
		t.Fatalf("phase history=%+v", evidence.Attempt.Runtime.Phases)
	}
	profile.Status = "completed"
	profile.FinishedAt = profile.AsOf
	if len(profile.Spans) == 0 {
		t.Fatal("missing runtime span")
	}
	profile.Spans[0].FinishedAt = profile.AsOf
	profile.Spans[0].Outcome = "completed"
	if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	execution.change = &runner.NativeChange{Error: "recorded refusal: /private/source/main.go token=private-secret " + strings.Repeat("é", 4096) + "\nprivate source content"}
	if err := execution.Finish(t.Context(), "failed"); err != nil {
		t.Fatal(err)
	}
	evidence = assertProgress(36, 3)
	finalization := evidence.Attempt.Finalization
	if finalization == nil || evidence.Attempt.FinalizationAvailability != "available" || finalization.Settled || !finalization.TextTruncated || !finalization.TextRedacted || len(finalization.Error) > tracker.NativeFinalizationTextLimit || !utf8.ValidString(finalization.Error) || !strings.HasPrefix(finalization.Error, "recorded refusal:") || finalization.ObservedAt.IsZero() || !strings.Contains(finalization.Coverage, "not_issue_acceptance") {
		t.Fatalf("bounded finalizer result=%+v", finalization)
	}
	for _, secret := range []string{"/private/source", "private-secret", "private source content"} {
		if strings.Contains(finalization.Error, secret) {
			t.Fatalf("finalizer result leaked %q", secret)
		}
	}
	if evidence.Attempt.Status != "failed" || evidence.Attempt.Runtime.Activity.Status != "completed" || evidence.Attempt.Runtime.Activity.Spans[0].Outcome != "completed" {
		t.Fatalf("final activity=%+v", evidence.Attempt)
	}
	final, err := h.admin.History(t.Context(), item, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range final.Items {
		if event.Type == "run.finished" && (event.Data.Run.Runtime.Activity.FinishedAt.IsZero() || event.Data.Run.Runtime.Activity.Status != "completed" || event.Data.Run.Runtime.Activity.Dropped != 31) {
			t.Fatalf("final immutable profile=%+v", event.Data.Run.Runtime.Activity)
		}
	}
}
