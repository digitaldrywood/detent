package hubclient

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestNativeRuntimeCheckpointsDoNotGrowHistory(t *testing.T) {
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
	profile := workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 42, Generation: 2, SessionID: 43, Stage: "implementation", Status: "running", Coverage: "partial", StartedAt: at, AsOf: at}
	observation := tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 2, Phase: "implementation", HeartbeatAt: at, Activity: &profile}
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
	for range 128 {
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
	for range 128 {
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
	profile.Spans[0].FinishedAt = profile.AsOf
	profile.Spans[0].Outcome = "completed"
	if err := execution.ObserveRuntime(t.Context(), observation); err != nil {
		t.Fatal(err)
	}
	if err := execution.Finish(t.Context(), "failed"); err != nil {
		t.Fatal(err)
	}
	evidence = assertProgress(36, 3)
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
