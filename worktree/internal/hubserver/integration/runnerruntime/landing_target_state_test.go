package runnerruntime_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLandingReceiptTargetStateFromLaterWorkerTransition(t *testing.T) {
	t.Parallel()
	h := hubfixture.Open(t, hubfixture.Config{})
	for _, test := range []struct {
		name, want string
		worker     bool
		human      bool
	}{
		{name: "no later transition"},
		{name: "worker transition", want: "Done", worker: true},
		{name: "human transition after the worker", worker: true, human: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := hubfixture.NewChangeInOrganization(t, h)
			runner := hubfixture.PrepareRunner(t, f.Native, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			runner.Enroll(t)
			hubfixture.PublishCapacity(t, f.Native, runner, hubfixture.CapacityReport(time.Now().UTC()))
			worker := runner.Redemption.Credential
			response := f.Hub.Request(t, http.MethodPost, f.Base+"/claims", worker, hubfixture.ProviderClaim(runner, f.Issue, "session"))
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			hubfixture.Decode(t, response, &lease)
			item := f.Base + "/work-items/" + string(f.Issue.WorkItemID)
			start := hubfixture.StartedEvent(lease)
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, item+"/events", worker, start), http.StatusOK)
			now := f.Hub.Now().UTC()
			finished := start
			finished.Type, finished.IdempotencyKey, finished.Data.Sequence, finished.Data.Outcome = "run.finished", "finished", 2, "succeeded"
			finished.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now, Landing: &tracker.NativeLandingReceipt{RefusalKind: "nothing_to_land", ObservedAt: now}}
			finished.Data.Disposition = &tracker.NativeDisposition{Status: "complete"}
			hubfixture.RequireStatus(t, f.Hub.Request(t, http.MethodPost, item+"/events", worker, finished), http.StatusOK)
			current := f.Issue
			move := func(token, state, reason string, mutation tracker.Mutation) {
				t.Helper()
				response := f.Hub.Request(t, http.MethodPost, item+"/workflow", token, tracker.Transition{Mutation: mutation, ExpectedRevision: current.Revision, State: state, Reason: reason})
				hubfixture.RequireStatus(t, response, http.StatusOK)
				hubfixture.Decode(t, response, &current)
			}
			if test.worker {
				move(worker, "Done", "worker_progress", tracker.Mutation{IdempotencyKey: "worker-done", LeaseID: lease.ID, FencingToken: lease.FencingToken})
			}
			if test.human {
				move(f.Token, "Todo", "user_requested", tracker.Mutation{IdempotencyKey: "human-reopen"})
			}
			response = f.Hub.Request(t, http.MethodGet, item+"/runtime?native_attempt_id="+start.Data.AttemptID, f.Token, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var evidence tracker.NativeRuntimeEvidence
			hubfixture.Decode(t, response, &evidence)
			if evidence.Attempt == nil || evidence.Attempt.Runtime == nil || evidence.Attempt.Runtime.Landing == nil {
				t.Fatalf("runtime lost the landing receipt: %+v", evidence.Attempt)
			}
			landing := evidence.Attempt.Runtime.Landing
			if landing.RefusalKind != "nothing_to_land" || landing.Landed || landing.TargetState != test.want || landing.FromState != "" {
				t.Fatalf("landing receipt=%+v, want target state %q", landing, test.want)
			}
		})
	}
}
