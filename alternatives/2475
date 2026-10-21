package orchestrator

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestProjectDiagnosticsReconcilesOwners(t *testing.T) {
	now := time.Date(2026, 10, 9, 13, 20, 0, 0, time.UTC)
	issue := connector.Issue{ID: "wi_completed", State: "In Progress"}
	running := Running{Issue: issue, WorkAttemptID: 4, Generation: 9, Mode: "implement"}
	record := deferredCompletion{Schema: deferredCompletionSchema, Running: running, CompletedAt: now.Add(-time.Minute), DeferredAt: now, Error: "tracker unavailable: token=private customer prompt", FenceRetryAt: now.Add(time.Minute), RetryAttempt: 2}
	state := State{Running: map[string]Running{issue.ID: running}, Claimed: map[string]Claimed{issue.ID: {Issue: issue, LeaseRenewedAt: now}}, deferredCompletions: map[string]deferredCompletion{issue.ID: record}, SchedulerDecisions: []telemetry.SchedulerDecision{{IssueID: "wi_ready", DecisionAt: now.Add(-time.Minute), Result: "skipped", Reason: dispatchSkipProjectCapacityFull}}}
	metadata, err := deferredCompletionMetadataJSON(record)
	if err != nil {
		t.Fatal(err)
	}
	active := []store.WorkAttempt{{ID: 4, IssueID: issue.ID, WorkerMetadataJSON: metadata}, {ID: 5, IssueID: "wi_unmatched", Phase: "implementation"}}
	ownerBytes := func() []byte {
		raw, err := json.Marshal([]any{state.Running, state.Claimed, state.deferredCompletions, state.SchedulerDecisions})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := string(ownerBytes())
	durable := append([]store.WorkAttempt(nil), active...)
	d := state.ProjectDiagnostics(active, now)
	if len(d.Records) != 2 || d.Counts["runtime_unsettled"] != 1 || d.Counts["durable_active"] != 2 {
		t.Fatalf("snapshot=%+v", d)
	}
	completed := d.Records[0]
	if completed.LocalAttemptID != 4 || completed.Generation != 9 || !reflect.DeepEqual(completed.Membership, []string{"running", "deferred", "durable_active", "claimed"}) || completed.ProviderCompletedAt != record.CompletedAt || completed.HostOperation != "orchestrator.retry_deferred_completion" || !completed.OperationPending || completed.RetryAt != record.FenceRetryAt {
		t.Fatalf("completion=%+v", completed)
	}
	if d.Records[1].IssueID != "wi_unmatched" || d.Records[1].Unavailable["native_attempt_id"] == "" {
		t.Fatalf("unmatched=%+v", d.Records[1])
	}
	if len(d.Admissions) != 1 || d.Admissions[0].Predicate != dispatchSkipProjectCapacityFull || d.Admissions[0].ObservedAt != now.Add(-time.Minute) {
		t.Fatalf("decision=%+v", d.Admissions)
	}
	if before != string(ownerBytes()) || !reflect.DeepEqual(durable, active) {
		t.Fatal("diagnostic mutated owners")
	}
	first, _ := json.Marshal(d)
	second, _ := json.Marshal(state.ProjectDiagnostics(active, now))
	if string(first) != string(second) {
		t.Fatal("read changed observation")
	}
}
