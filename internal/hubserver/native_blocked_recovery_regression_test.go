package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestAuditDispositionRecoversRecordedBlockedLane(t *testing.T) {
	t.Run("resume in one tenant", testResumeClearedAuditAcrossTenants)
	for _, name := range []string{"partial and final clearance", "receipt replay", "concurrent operator stop", "newer version", "independent question"} {
		t.Run(name, func(t *testing.T) {
			f := newDispatchGuardFixture(t)
			configureHubOwnedBlockedRecovery(t, f)
			if name == "concurrent operator stop" {
				configureDispatchGuardState(t, f, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo", "Blocked"}})
			}
			prior := f.reload(t)
			run, key := blockedAuditRun(f)
			change, version, review := blockOnAuditReview(t, f, run)
			itemPath := f.base + "/work-items/" + string(prior.WorkItemID)
			mutation := tracker.Mutation{LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}
			if name == "independent question" {
				mutation.IdempotencyKey = "product-question"
				question := tracker.CreateComment{Mutation: mutation, Body: workpad.BlockedDetail("", "Choose the default visibility.")}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/comments", f.worker, question), http.StatusOK)
			}
			f.release(t)
			blocked := f.reload(t)
			if blocked.State != "Blocked" {
				t.Fatalf("audit parked item in %s", blocked.State)
			}
			dispositions := f.base + "/security-audits/dispositions"
			first := securityaudit.DispositionCommand{RequestID: "dispose-one", Key: key, FindingID: "finding_one", Confirm: true, Evidence: "The focused route test exercised the guard."}
			last := securityaudit.DispositionCommand{RequestID: "dispose-two", Key: key, FindingID: "finding_two", Confirm: true, Evidence: "The focused rejection test verified zero writes."}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, dispositions, testHubAdminToken, first), http.StatusOK)
			if got := f.reload(t); got.State != blocked.State || got.Revision != blocked.Revision {
				t.Fatalf("partial clearance returned %s at revision %d", got.State, got.Revision)
			}
			if name == "newer version" {
				publishNewerBlockedAuditVersion(t, f, change, version)
			}
			if name == "receipt replay" {
				seedAuditDispositionReceipt(t, f, change.ID, review.Sequence, run.ID, last)
			}
			var response *httptest.ResponseRecorder
			if name == "concurrent operator stop" {
				response = postAuditDispositionDuringStop(t, f, itemPath, dispositions, last, blocked)
			} else {
				response = performHubAPIRequest(t, f.service, http.MethodPost, dispositions, testHubAdminToken, last)
			}
			requireNativeStatus(t, response, http.StatusOK)
			wantState, wantRevision := blocked.State, blocked.Revision
			if name == "partial and final clearance" || name == "receipt replay" {
				wantState, wantRevision = prior.State, blocked.Revision+1
			}
			if got := f.reload(t); got.State != wantState || got.Revision != wantRevision {
				t.Fatalf("final clearance returned %s at revision %d, want %s at revision %d", got.State, got.Revision, wantState, wantRevision)
			}
			replay := performHubAPIRequest(t, f.service, http.MethodPost, dispositions, testHubAdminToken, last)
			requireNativeStatus(t, replay, http.StatusOK)
			if got := f.reload(t); replay.Body.String() != response.Body.String() || got.State != wantState || got.Revision != wantRevision {
				t.Fatalf("replay changed the receipt or lane: %s at revision %d", got.State, got.Revision)
			}
		})
	}
}

func blockedAuditRun(f dispatchGuardFixture) (securityaudit.Run, securityaudit.Key) {
	key := securityaudit.Key{ProjectID: string(f.project.ID), Repository: "example-org/example-project", PRNumber: 1, BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40)}
	run := securityaudit.Run{ID: 70, ProjectID: key.ProjectID, IssueID: string(f.issue.WorkItemID), Repository: key.Repository, PRNumber: key.PRNumber, BaseSHA: key.BaseSHA, HeadSHA: key.HeadSHA,
		Verdict: securityaudit.VerdictFail, ExitStatus: securityaudit.ExitStatusSuccess, InvocationID: "audit-example", WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: f.now, StartedAt: f.now, CompletedAt: f.now,
		OutputBytes: 2, OutputDigest: securityaudit.OutputDigest("{}"), Summary: "Example audit", ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(),
		Findings: []securityaudit.Finding{{ID: "finding_one", Severity: "p1", Certainty: securityaudit.CertaintyUndetermined, UnresolvedFact: "Does the route call the guard?"}, {ID: "finding_two", Severity: "p2", Certainty: securityaudit.CertaintyUndetermined, UnresolvedFact: "Does rejection prevent a write?"}}}
	return run, key
}

func testResumeClearedAuditAcrossTenants(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	prior := f.issue
	run, key := blockedAuditRun(f)
	run.Findings = run.Findings[:1]
	blockOnAuditReview(t, f, run)
	itemPath := f.base + "/work-items/" + string(prior.WorkItemID)
	mutation := tracker.Mutation{LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}
	org := prior.OrganizationID + "_second"
	peer := newNativeFixtureWithProjectID(t, f.service, org, "Other tenant project", f.project.ID)
	other := dispatchGuardFixture{conversationWorkerFixture: &conversationWorkerFixture{nativeFixture: peer, now: f.now, policy: f.policy}, worktreeState: "clean"}
	approveHubTestPolicy(t, f.service, peer.base+"/policy", hubTestPolicy())
	configureHubOwnedBlockedRecovery(t, other)
	other.issue = peer.create(t, "Other audit item")
	other.runner = conversationWorkerRunner(t, peer)
	other.worker = other.runner.redemption.Credential
	other.runner.reportCapacity(t)
	other.claim(t)
	otherRun := run
	otherRun.IssueID = string(other.issue.WorkItemID)
	blockOnAuditReview(t, other, otherRun)
	other.release(t)
	otherBlocked := other.reload(t)
	if otherBlocked.State != "Blocked" || otherBlocked.Number != prior.Number {
		t.Fatalf("other tenant item #%d started in %s, want #%d in Blocked", otherBlocked.Number, otherBlocked.State, prior.Number)
	}
	for i := range 401 {
		if i > 0 && i%75 == 0 {
			f.advance(time.Minute)
		}
		mutation.IdempotencyKey = fmt.Sprintf("progress-%d", i)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/comments", f.worker, tracker.CreateComment{Mutation: mutation, Body: "Progress recorded."}), http.StatusOK)
	}
	f.release(t)
	blocked := f.reload(t)
	if blocked.State != "Blocked" {
		t.Fatalf("item awaiting disposition was in %s", blocked.State)
	}
	command := securityaudit.DispositionCommand{RequestID: "clear-audit", Key: key, FindingID: run.Findings[0].ID, Confirm: true, Evidence: "The focused route test exercised the guard."}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/security-audits/dispositions", testHubAdminToken, command), http.StatusOK)
	if got := f.reload(t); got.State != "Blocked" || got.Revision != blocked.Revision {
		t.Fatalf("disposition crossed the page bound: %s at revision %d", got.State, got.Revision)
	}
	f.runner.reportCapacity(t)
	claim := hubOwnedRecoveryClaim(f, "resume-cleared-audit")
	claim.WorkItemID = blocked.WorkItemID
	claimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	if claimed.Code != http.StatusOK {
		t.Fatalf("next recovery claim status = %d, want 200: %s", claimed.Code, claimed.Body.String())
	}
	if got := f.reload(t); got.State != prior.State || got.Revision != blocked.Revision+1 {
		t.Fatalf("next claim left cleared item in %s at revision %d", got.State, got.Revision)
	}
	if got := other.reload(t); got.State != "Blocked" || got.Revision != otherBlocked.Revision {
		t.Fatalf("other tenant moved to %s at revision %d", got.State, got.Revision)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/security-audits/70/dispositions", other.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var receipts []securityaudit.Disposition
	decodeHubResponse(t, response, &receipts)
	if len(receipts) != 0 {
		t.Fatalf("other tenant has %d dispositions", len(receipts))
	}
}

func blockOnAuditReview(t *testing.T, f dispatchGuardFixture, run securityaudit.Run) (eventstore.Change, tracker.ChangeVersion, eventstore.Evidence) {
	t.Helper()
	change, version, review := seedBlockedAuditReview(t, f, f.issue, run)
	f.completionBody = run.CompletionMessage("The audit needs an answer.")
	f.finish(t, &tracker.NativeDisposition{Status: workpad.StatusBlocked, HumanAction: true})
	current := f.reload(t)
	path := f.base + "/work-items/" + string(current.WorkItemID)
	mutation := tracker.Mutation{IdempotencyKey: "audit-hold", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}
	hold := tracker.Transition{Mutation: mutation, ExpectedRevision: current.Revision, State: "Blocked", Reason: "worker_progress", ReasonDetail: f.completionBody}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.worker, hold), http.StatusOK)
	mutation.IdempotencyKey = "audit-question"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.worker, tracker.CreateComment{Mutation: mutation, Body: f.completionBody}), http.StatusOK)
	return change, version, review
}

func seedBlockedAuditReview(t *testing.T, f dispatchGuardFixture, item tracker.NativeIssue, run securityaudit.Run) (eventstore.Change, tracker.ChangeVersion, eventstore.Evidence) {
	t.Helper()
	ctx := t.Context()
	content := eventstore.NewContent(f.service.runDetailStore())
	const changeID = "change_shared_audit"
	change, err := content.PrepareChange(ctx, item.OrganizationID, auditRecoveryJSON(t, tracker.ChangeRequest{ID: changeID, OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.WorkItemID, Title: "Example change", Revision: 1, CreatedAt: f.now, UpdatedAt: f.now}))
	if err != nil {
		t.Fatal(err)
	}
	version := tracker.ChangeVersion{ID: "version_shared_audit", ChangeID: changeID, Number: 1, CreatedAt: f.now,
		ChangeVersionInput: tracker.ChangeVersionInput{Repository: run.Repository, BaseSHA: run.BaseSHA, HeadSHA: run.HeadSHA, AttemptID: f.attempt, PolicyID: f.policy}}
	row, err := content.PrepareVersion(ctx, item.OrganizationID, auditRecoveryJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	review := tracker.ChangeReview{ID: "review_shared_audit", VersionID: version.ID, CreatedAt: f.now, Decision: "changes_requested", SecurityAudit: &tracker.SecurityAuditReview{VersionID: version.ID, SessionID: run.ID, Run: run}}
	evidence, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: item.OrganizationID, ChangeID: changeID, VersionID: &version.ID, Kind: "review", CreatedAt: f.now}, auditRecoveryJSON(t, review))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(ctx, item.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(ctx, change); err != nil {
			return err
		}
		if _, err := w.CreateVersion(ctx, row); err != nil {
			return err
		}
		if err := w.LinkIssue(ctx, eventstore.Link{OrganizationID: item.OrganizationID, ProjectID: item.ProjectID, WorkItemID: item.WorkItemID, ChangeID: changeID}); err != nil {
			return err
		}
		change.CurrentVersionID = &version.ID
		if _, err := w.UpdateChange(ctx, change); err != nil {
			return err
		}
		evidence, err = w.AppendEvidence(ctx, evidence)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return change, version, evidence
}

func publishNewerBlockedAuditVersion(t *testing.T, f dispatchGuardFixture, change eventstore.Change, version tracker.ChangeVersion) {
	t.Helper()
	ctx := t.Context()
	version.ID, version.Number, version.HeadSHA = hubapi.NewID("version"), 2, strings.Repeat("c", 40)
	row, err := eventstore.NewContent(f.service.runDetailStore()).PrepareVersion(ctx, change.OrganizationID, auditRecoveryJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(ctx, change.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateVersion(ctx, row); err != nil {
			return err
		}
		current, err := w.LockChange(ctx, change.ID)
		if err != nil {
			return err
		}
		current.CurrentVersionID = &version.ID
		_, err = w.UpdateChange(ctx, current)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func seedAuditDispositionReceipt(t *testing.T, f dispatchGuardFixture, changeID string, reviewSequence, runID int64, request securityaudit.DispositionCommand) {
	t.Helper()
	ctx := t.Context()
	value := securityaudit.Disposition{AuditRunID: runID, FindingID: request.FindingID, Status: securityaudit.DispositionFalsePositive, Evidence: request.Evidence, ServiceIdentity: securityaudit.ServiceIdentity(string(f.project.ID)), RecordedAt: f.service.config.Clock().UTC()}
	ref, err := f.service.runDetailStore().StoreDetail(ctx, string(f.project.OrganizationID), auditRecoveryJSON(t, value))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := hubapi.CommandHash(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.(eventstore.AuditDispositionWriter).CreateAuditDisposition(ctx, eventstore.AuditDisposition{Organization: f.project.OrganizationID, Project: f.project.ID, ChangeID: changeID, ReviewSequence: reviewSequence, ActorID: f.authority.ID, RequestKey: request.RequestID, RequestHash: hash, Disposition: value, Reference: ref})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type auditRecoveryPauseStore struct {
	attachment.DetailStore
	prepare func()
	resume  chan struct{}
}

func (s auditRecoveryPauseStore) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	var event tracker.CollaborationEvent
	if json.Unmarshal(raw, &event) == nil && event.Type == "workflow.transitioned" && event.Actor.Kind == "system" {
		s.prepare()
		select {
		case <-s.resume:
		case <-ctx.Done():
			return attachment.ArchiveReference{}, ctx.Err()
		}
	}
	return s.DetailStore.StoreDetail(ctx, org, raw)
}

func postAuditDispositionDuringStop(t *testing.T, f dispatchGuardFixture, itemPath, dispositions string, command securityaudit.DispositionCommand, blocked tracker.NativeIssue) *httptest.ResponseRecorder {
	t.Helper()
	prepared := make(chan struct{})
	pause := auditRecoveryPauseStore{DetailStore: f.service.runDetailStore(), prepare: sync.OnceFunc(func() { close(prepared) }), resume: make(chan struct{})}
	f.service.config.RunDetails = pause
	resume := sync.OnceFunc(func() { close(pause.resume) })
	request := newHubAPIRequest(t, http.MethodPost, dispositions, testHubAdminToken, command).WithContext(t.Context())
	done := make(chan struct{})
	response := httptest.NewRecorder()
	t.Cleanup(func() {
		resume()
		<-done
		f.service.config.RunDetails = pause.DetailStore
	})
	go func() {
		defer close(done)
		f.service.Handler().ServeHTTP(response, request)
	}()
	select {
	case <-done:
		t.Error("disposition returned before preparing lane recovery")
		return response
	case <-prepared:
	}
	stop := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "operator-keeps-blocked"}, ExpectedRevision: blocked.Revision, State: "Blocked", Reason: "user_requested"}
	stopped := performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/workflow", testHubAdminToken, stop)
	if stopped.Code != http.StatusOK {
		t.Errorf("operator stop returned %d: %s", stopped.Code, stopped.Body.String())
	}
	resume()
	<-done
	return response
}

func auditRecoveryJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

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
