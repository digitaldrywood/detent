package hubserver

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestHubOwnedBlockedRecoveryR2HumanDisposition(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	f.failure = &tracker.NativeTerminalFailure{ErrorClass: tracker.WorkspaceSharedMemoryExhausted, Stage: "workspace_preparation", Source: "runner", Coverage: "complete", ObservedAt: f.now, Summary: "Example setup failed"}
	f.completionBody = "Completion prefix without the final Workpad block"
	f.finish(t, &tracker.NativeDisposition{Status: "blocked", Blockers: true, HumanAction: true, BlockerEvidence: []workpad.Blocker{{Owner: "human", Reason: "Choose the default visibility", Unverifiable: true}}})
	current := f.reload(t)
	req := tracker.Transition{ExpectedRevision: current.Revision, State: "Blocked", Reason: "worker_progress", Mutation: tracker.Mutation{IdempotencyKey: "review-product-question", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}}
	resp := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(current.WorkItemID)+"/workflow", f.worker, req)
	requireNativeStatus(t, resp, http.StatusOK)
	if got := f.reload(t); got.State != "Blocked" {
		t.Errorf("typed human blocker accepted but item stayed %s revision %d", got.State, got.Revision)
	}
}
func TestHubOwnedBlockedRecoveryR2LegacyInstanceCommand(t *testing.T) {
	for _, command := range []string{"workflow", "claim"} {
		t.Run(command, func(t *testing.T) {
			f := newDispatchGuardFixture(t)
			configureHubOwnedBlockedRecovery(t, f)
			f.succeed(t, &tracker.NativeDisposition{Status: workpad.StatusBlocked, Blockers: true, BlockerEvidence: []workpad.Blocker{
				{Owner: workpad.BlockerOwnerInstance, Ref: "instance:check-barrier", Reason: "Barrier failed on this host", Unverifiable: true},
			}})
			blocked := seedHubOwnedBlockedRecovery(t, f.nativeFixture, f.reload(t), tracker.Actor{Kind: "runner", PrincipalID: f.runner.binding.RunnerID})
			next := conversationWorkerRunner(t, f.nativeFixture)
			reportRunnerCapacity(t, f.service, f.base, next.redemption.Credential, next.binding.MachineID, next.redemption.DisplayName, next.redemption.BackendIsolation, next.redemption.Capacity)
			if command == "workflow" {
				request := tracker.Transition{ExpectedRevision: blocked.Revision, State: "Todo", Reason: "dependency_ready", ReasonDetail: "recorded_blocker_recovery", PolicyID: f.policy, BlockerAttemptID: f.attempt, Mutation: tracker.Mutation{IdempotencyKey: "legacy-instance-command", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}}
				path := f.base + "/work-items/" + string(blocked.WorkItemID) + "/workflow"
				var receipt string
				for range 2 {
					response := performHubAPIRequest(t, f.service, http.MethodPost, path, next.redemption.Credential, request)
					requireNativeStatus(t, response, http.StatusOK)
					if receipt != "" && receipt != response.Body.String() {
						t.Fatal("standalone recovery replay changed the receipt")
					}
					receipt = response.Body.String()
				}
			} else {
				claim := hubOwnedRecoveryClaim(f, "legacy-instance-claim")
				claim.WorkItemID, claim.MachineID = blocked.WorkItemID, next.binding.MachineID
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", next.redemption.Credential, claim)
				requireNativeStatus(t, response, http.StatusOK)
				var lease tracker.NativeLease
				decodeHubResponse(t, response, &lease)
				if lease.WorkItemID != blocked.WorkItemID || lease.MachineID != next.binding.MachineID {
					t.Fatalf("unexpected lease: %+v", lease)
				}
			}
			if after := f.reload(t); after.State != "Todo" || after.Revision != blocked.Revision+1 {
				t.Fatalf("recovery left %s at revision %d", after.State, after.Revision)
			}
		})
	}
}

func TestHubOwnedBlockedRecoveryR2LiveLease(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	blocked := seedHubOwnedBlockedRecovery(t, f.nativeFixture, f.reload(t), tracker.Actor{Kind: "runner", PrincipalID: f.runner.binding.RunnerID})
	var rec tracker.NativeRecovery
	req := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(blocked.WorkItemID)+"?view=recovery", f.worker, nil)
	requireNativeStatus(t, req, http.StatusOK)
	decodeHubResponse(t, req, &rec)
	if rec.ContinuationEligible == nil || !*rec.ContinuationEligible {
		t.Error("live running lease lost continuation eligibility")
	}
	next := conversationWorkerRunner(t, f.nativeFixture)
	reportRunnerCapacity(t, f.service, f.base, next.redemption.Credential, next.binding.MachineID, next.redemption.DisplayName, next.redemption.BackendIsolation, next.redemption.Capacity)
	claim := hubOwnedRecoveryClaim(f, "competing-lease")
	claim.WorkItemID, claim.MachineID = blocked.WorkItemID, next.binding.MachineID
	resp := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", next.redemption.Credential, claim)
	if resp.Code == http.StatusOK || f.reload(t).State != "Blocked" {
		t.Error("competing claim released live hold")
	}
}
func TestHubOwnedBlockedRecoveryR2ProviderClaim(t *testing.T) {
	f := newDispatchGuardFixture(t)
	blocked := legacyInstanceBlock(t, f)
	publishCapacity(t, f.nativeFixture, f.runner, capacityReport(f.now))
	claim := providerClaim(f.runner, blocked, "recovery-provider")
	claim.WorkflowStates = []string{"Todo"}
	var preview tracker.NativeCapacityPage
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", f.worker, tracker.NativeCapacityPreview{NativeClaim: hubOwnedRecoveryClaim(f, "")})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &preview)
	if len(preview.Items) != 1 || preview.Items[0].Revision != blocked.Revision {
		t.Fatalf("expected a preview at the Blocked revision, got %+v", preview.Items)
	}
	claim.ProviderCandidates[0].Revision = preview.Items[0].Revision
	rejected := claim
	rejected.ProviderCandidates = append([]tracker.NativeCapacityCandidate(nil), claim.ProviderCandidates...)
	rejected.ProviderCandidates[0].Requirement.Model = "unavailable"
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, rejected)
	if after := f.reload(t); response.Code == http.StatusOK || after.Revision != blocked.Revision || after.State != "Blocked" {
		t.Errorf("unavailable provider returned HTTP %d and left %s at revision %d", response.Code, after.State, after.Revision)
	}
	var acquired tracker.NativeLease
	for range 2 {
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
		requireNativeStatus(t, response, http.StatusOK)
		var lease tracker.NativeLease
		decodeHubResponse(t, response, &lease)
		if lease.WorkItemID != blocked.WorkItemID || lease.ID == "" || acquired.ID != "" && lease.ID != acquired.ID {
			t.Fatalf("unexpected acquired or replayed lease: %+v", lease)
		}
		acquired = lease
	}
	if after := f.reload(t); after.State != "Todo" || after.Revision != blocked.Revision+1 {
		t.Fatalf("claim and replay left %s at revision %d", after.State, after.Revision)
	}
}
func TestHubOwnedBlockedRecoveryR2PostedProductQuestion(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	current := f.reload(t)
	path := f.base + "/work-items/" + string(current.WorkItemID)
	move := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "review-block", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ExpectedRevision: current.Revision, State: "Blocked", Reason: "worker_progress"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.worker, move), http.StatusOK)
	body := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - owner: human\n    reason: Product approval\nhuman_action: Choose the default visibility.\n```"
	question := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "review-question", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, Body: body}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.worker, question), http.StatusOK)
	f.failure = &tracker.NativeTerminalFailure{ErrorClass: tracker.WorkspaceSharedMemoryExhausted, Stage: "workspace_preparation", Source: "runner", Coverage: "complete", ObservedAt: f.now, Summary: "Example setup failed"}
	f.succeed(t, &tracker.NativeDisposition{Status: "blocked", HumanAction: true})
	blocked := f.reload(t)
	claim := hubOwnedRecoveryClaim(f, "review-comment")
	claim.WorkItemID = blocked.WorkItemID
	resp := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	if got := f.reload(t); got.State != "Blocked" || resp.Code == http.StatusOK {
		t.Errorf("explicit posted product question released, claim HTTP %d state %s", resp.Code, got.State)
	}
}
func TestHubOwnedBlockedRecoveryR2NondispatchablePrior(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	configureDispatchGuardState(t, f, tracker.NativeState{Name: "In Progress", Transitions: []string{"Blocked", "Todo"}})
	current := f.reload(t)
	path := f.base + "/work-items/" + string(current.WorkItemID)
	move := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "review-progress", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ExpectedRevision: current.Revision, State: "In Progress", Reason: "worker_progress"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.worker, move), http.StatusOK)
	f.finish(t, &tracker.NativeDisposition{Status: "blocked", HumanAction: true, ReasonCode: "instance_limitation"})
	current = f.reload(t)
	move.State, move.IdempotencyKey, move.ExpectedRevision = "Blocked", "review-block", current.Revision
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.worker, move), http.StatusOK)
	claim := hubOwnedRecoveryClaim(f, "retry-after-progress")
	claim.WorkItemID = current.WorkItemID
	if response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim); response.Code == http.StatusOK {
		t.Fatal("retry claim displaced a live lease")
	}
	f.release(t)
	resp := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	if resp.Code != http.StatusOK {
		t.Errorf("ended machine-only hold left item in %s with no next claim: %d %s", f.reload(t).State, resp.Code, resp.Body.String())
	}
}

func TestHubOwnedBlockedRecoveryR2SkipsOperatorPage(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	f.release(t)
	stopped := f.reload(t)
	path := f.base + "/work-items/" + string(stopped.WorkItemID) + "/workflow"
	stop := tracker.Transition{ExpectedRevision: stopped.Revision, State: "Blocked", Reason: "user_requested", Mutation: tracker.Mutation{IdempotencyKey: "operator-stop"}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, stop), http.StatusOK)
	request := tracker.NativeCapacityPreview{NativeClaim: hubOwnedRecoveryClaim(f, ""), Limit: 1}
	var page tracker.NativeCapacityPage
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", f.worker, request)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &page)
	if len(page.Items) != 0 || page.Next == 0 {
		t.Fatalf("expected an empty page with a cursor past the operator hold, got %+v", page)
	}
	request.After = page.Next
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", f.worker, request)
	requireNativeStatus(t, response, http.StatusOK)
	page = tracker.NativeCapacityPage{}
	decodeHubResponse(t, response, &page)
	if len(page.Items) != 0 || page.Next != 0 {
		t.Fatalf("expected the cursor to advance past the operator hold, got %+v", page)
	}
}

func TestHubOwnedBlockedRecoveryR2PreservesInterruptedStop(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	current := f.reload(t)
	path := f.base + "/work-items/" + string(current.WorkItemID)
	stop := tracker.Transition{ExpectedRevision: current.Revision, State: "Blocked", Reason: "worker_progress", ReasonDetail: "operator stopped",
		Mutation: tracker.Mutation{IdempotencyKey: "legacy-stop", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.worker, stop), http.StatusOK)
	finished := tracker.NativeRunEvent{Type: "run.finished", SchemaVersion: 1, Mutation: tracker.Mutation{IdempotencyKey: "legacy-stop-completion"},
		Data: tracker.NativeRunData{Sequence: 2, Identity: &f.started, LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "interrupted",
			TerminalFailure: &tracker.NativeTerminalFailure{Decision: &tracker.FailureAttribution{Outcome: "instance", Reason: "runner cancelled the execution"}, Source: "runner", Coverage: "complete", ObservedAt: f.now, Summary: "Execution interrupted"}}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", f.worker, finished), http.StatusOK)
	f.release(t)
	claim := hubOwnedRecoveryClaim(f, "claim-after-legacy-stop")
	claim.WorkItemID = current.WorkItemID
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	if after := f.reload(t); response.Code == http.StatusOK || after.State != "Blocked" || after.Revision != current.Revision+1 {
		t.Fatalf("interrupted stop returned HTTP %d and left %s at revision %d", response.Code, after.State, after.Revision)
	}
}

func TestHubOwnedBlockedRecoveryResumesCommentEvidence(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	f.finish(t, &tracker.NativeDisposition{Status: workpad.StatusBlocked, ReasonCode: "instance_limitation"})
	path := f.base + "/work-items/" + string(f.issue.WorkItemID)
	var first tracker.NativeComment
	for i := range 401 {
		if i > 0 && i%75 == 0 {
			f.advance(time.Minute)
		}
		request := tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("progress-%d", i), LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, Body: "Machine diagnostic."}
		response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.worker, request)
		requireNativeStatus(t, response, http.StatusOK)
		if i == 0 {
			decodeHubResponse(t, response, &first)
		}
	}
	reportRunnerCapacity(t, f.service, f.base, f.worker, f.runner.binding.MachineID, f.runner.redemption.DisplayName, f.runner.redemption.BackendIsolation, f.runner.redemption.Capacity)
	f.release(t)
	blocked := seedHubOwnedBlockedRecovery(t, f.nativeFixture, f.reload(t), tracker.Actor{Kind: "runner", PrincipalID: f.runner.binding.RunnerID})
	response := performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=recovery", f.worker, nil)
	requireNativeStatus(t, response, http.StatusOK)
	claim := hubOwnedRecoveryClaim(f, "resume-comment-evidence")
	claim.WorkItemID = blocked.WorkItemID
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	requireNativeStatus(t, response, http.StatusConflict)
	if got := f.reload(t); got.State != "Blocked" || got.Revision != blocked.Revision {
		t.Fatalf("partial evidence moved the item to %s revision %d", got.State, got.Revision)
	}
	edit := tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "question-in-examined-page"}, ExpectedRevision: first.Revision, Body: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - owner: human\n    reason: Product approval\nhuman_action: Choose the default visibility.\n```"}
	response = performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+first.ID, f.token, edit)
	requireNativeStatus(t, response, http.StatusOK)
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	requireNativeStatus(t, response, http.StatusConflict)
	if got := f.reload(t); got.State != "Blocked" || got.Revision != blocked.Revision {
		t.Fatalf("claim crossed a question in the examined page: %+v", got)
	}
	edit.IdempotencyKey, edit.ExpectedRevision, edit.Body = "answered-question", first.Revision+1, "Use private visibility."
	response = performHubAPIRequest(t, f.service, http.MethodPatch, path+"/comments/"+first.ID, f.token, edit)
	requireNativeStatus(t, response, http.StatusOK)
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if got := f.reload(t); lease.WorkItemID != blocked.WorkItemID || got.Revision != blocked.Revision+1 || got.State != "Todo" {
		t.Fatalf("resumed claim returned lease %+v with item %s at revision %d", lease, got.State, got.Revision)
	}
}
