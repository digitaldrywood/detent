package hubserver

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPublishedReviewCheckpoint(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		checkpoint bool
		wantReview bool
	}{
		{name: "missing checkpoint"},
		{name: "valid checkpoint", checkpoint: true, wantReview: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDispatchGuardFixture(t)
			configureDispatchGuardState(t, f, tracker.NativeState{Name: "Fixing", Dispatchable: true, Transitions: []string{"Merging", "Blocked"}})
			moveDispatchGuardFixture(t, f, "Fixing")
			item := f.base + "/work-items/" + string(f.issue.WorkItemID)
			finish := tracker.NativeRunEvent{
				Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("finish")}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{
					Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "gpt-6-astra"},
					LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded",
					Disposition: &tracker.NativeDisposition{Status: "complete"},
				},
			}
			input := changeTestInput()
			input.RunID, input.AttemptID = f.run, f.attempt
			if test.checkpoint {
				checkpoint := finish
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Outcome, checkpoint.Data.Disposition = "run.checkpointed", hubapi.NewID("checkpoint"), "", nil
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				checkpoint.Data.Handoff.WorktreeState, checkpoint.Data.Handoff.HeadSHA, checkpoint.Data.Handoff.WorkspaceDigest = "clean", input.HeadSHA, strings.Repeat("d", 64)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, item+"/events", f.worker, checkpoint), http.StatusOK)
				finish.Data.Sequence++
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, item+"/changes", f.worker, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, Title: "Published source", Body: "Owned source publication"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, item+"/events", f.worker, finish), http.StatusOK)

			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: tracker.ChangeReviewPolicy{PolicyID: f.policy, RequireReview: true}}), http.StatusOK)
			response = performHubAPIRequest(t, f.service, http.MethodPost, item+"/changes/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ChangeVersionInput: input})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			rewriteDispatchAttemptFixture(t, f, func(run *tracker.NativeRunData) {
				run.Finalization = &tracker.NativeFinalization{Changed: true, ChangeID: change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
			})
			f.release(t)

			runner := f.runner
			admission := map[string]any{"context": tracker.NativeAdmissionContext{ObservedAt: f.now, PolicyID: f.policy, WorkflowStates: []string{"Fixing"}}, "runner_revision": 1}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": runner.redemption.BackendIsolation, "admission": admission}), http.StatusOK)
			response = performHubAPIRequest(t, f.service, http.MethodGet, item+"/runtime", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var current tracker.NativeRuntimeEvidence
			decodeHubResponse(t, response, &current)
			if len(current.Admission) != 1 {
				t.Fatalf("missing current admission: %s", response.Body)
			}
			exclusion := current.Admission[0].AnsweredExclusion
			if test.wantReview {
				if exclusion == nil || exclusion.Kind != string(gate.ReasonAutomatedReviewMissing) || exclusion.AttemptID != f.attempt {
					t.Fatalf("published review = %+v, want attempt %q awaiting review: %s", exclusion, f.attempt, response.Body)
				}
			} else if exclusion != nil {
				t.Fatalf("published review = %+v without a checkpoint, want none", exclusion)
			}
			if got := f.candidate(t); got == test.wantReview {
				t.Fatalf("candidate=%t, want %t", got, !test.wantReview)
			}
		})
	}
}
