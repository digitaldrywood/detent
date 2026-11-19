package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

// dispatchGuardFixture is the conversation worker fixture with the few
// helpers the claim guard needs: read the candidate set, finish an attempt,
// and move the item the ways a person or the orchestrator would.
type dispatchGuardFixture struct {
	*conversationWorkerFixture
	worktreeState      string
	completionBody     string
	checkpoint         *tracker.NativeCheckpoint
	failure            *tracker.NativeTerminalFailure
	authority          hubapi.APICredential
	acceptanceRecorded bool
}

func newDispatchGuardFixture(t *testing.T) dispatchGuardFixture {
	t.Helper()
	f := dispatchGuardFixture{conversationWorkerFixture: newConversationWorkerFixture(t), worktreeState: "clean"}
	reportRunnerCapacity(t, f.service, f.base, f.worker, f.runner.binding.MachineID, f.runner.redemption.DisplayName, f.runner.redemption.BackendIsolation, f.runner.redemption.Capacity)
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(testHubAdminToken), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authority status%d: %v", status, err)
	}
	f.authority = credential
	return f
}

func (f dispatchGuardFixture) scope() hubscope.NativeScope {
	return hubscope.NativeScope{
		Organization: f.project.OrganizationID,
		Project:      f.project.ID,
		Credential:   f.authority,
	}
}

// candidate reports whether the claim query still offers the fixture's item.
func (f dispatchGuardFixture) candidate(t *testing.T) bool {
	t.Helper()
	scope := f.scope()
	ctx := f.service.runDetailContext(t.Context())
	request := tracker.NativeClaim{WorkItemID: f.issue.WorkItemID, PolicyID: f.policy, MachineID: f.runner.binding.MachineID, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration"}}
	now := f.service.config.Clock()
	var plan hubrecords.NativeClaimPlan
	err := f.service.config.TenantRepository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		if err := f.service.authorizeAccessScope(ctx, r, scope, true); err != nil {
			return err
		}
		query := nativeTenantCandidateQuery(scope, request, f.issue.WorkItemID, now)
		query.IncludeLeasedAnchor = true
		page, err := r.CandidatePage(ctx, query)
		if err != nil {
			return err
		}
		if len(page.Items) == 0 {
			return ErrNoClaimableWork
		}
		plan, err = f.service.readNativeClaimPlanRows(ctx, scope, request, now, r, &page.Items[0], hubrecords.NativeClaimPlan{})
		return err
	})
	if err == nil {
		if err = f.service.prepareNativeClaimLegacyAuditFacts(ctx, scope, []hubrecords.NativeClaimPlan{plan}); err == nil {
			plans := []hubrecords.NativeClaimPlan{plan}
			if err = f.service.prepareNativeClaimApprovals(ctx, scope, plans); err == nil {
				_, err = f.service.hydrateNativeClaimPlan(ctx, scope, request, now, plans[0], nil)
			}
		}
	}
	if errors.Is(err, ErrNoClaimableWork) {
		return false
	}
	if err != nil {
		t.Fatalf("current typed claim candidate: %v", err)
	}
	return true
}

// succeed finishes the running attempt and releases its lease, which is
// everything the hub itself does when an attempt completes: nothing moves the
// item.
func (f dispatchGuardFixture) succeed(t *testing.T, disposition ...*tracker.NativeDisposition) {
	t.Helper()
	f.finish(t, disposition...)
	f.release(t)
}

func (f dispatchGuardFixture) finish(t *testing.T, disposition ...*tracker.NativeDisposition) {
	t.Helper()
	identity := f.started
	event := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("finish")}, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{
			Sequence: 2, Identity: &identity,
			LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded",
		},
	}
	if len(disposition) > 0 {
		event.Data.Disposition = disposition[0]
	}
	event.Data.CompletionBody = f.completionBody
	checkpoint := event
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Outcome = "run.checkpointed", hubapi.NewID("checkpoint"), ""
	checkpoint.Data.Disposition = nil
	checkpoint.Data.CompletionBody = ""
	checkpoint.Data.Handoff = nativeTestCheckpoint()
	checkpoint.Data.Handoff.WorktreeState = f.worktreeState
	if f.checkpoint != nil {
		checkpoint.Data.Handoff = f.checkpoint
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, checkpoint), http.StatusOK)
	if f.acceptanceRecorded {
		event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "completed", Completion: &tracker.NativeCompletionObservation{AcceptanceRecorded: true, ObservedAt: f.now}}
	}

	event.Data.Sequence++
	if f.failure != nil {
		event.Data.Outcome = "failed"
		event.Data.TerminalFailure = f.failure
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
}

// reload re-reads the item so the next mutation carries its current revision.
func (f dispatchGuardFixture) reload(t *testing.T) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}

// continueConversation records the continuation the way the operator API
// does, through the same acceptCommand path.
func (f dispatchGuardFixture) continueConversation(t *testing.T, key string) {
	t.Helper()
	command := conversation.Command{Key: key, Kind: conversation.CommandContinue, Text: "Continue with the release notes.", Expected: conversation.Expected{AttemptID: f.record.Execution.Owner.AttemptID}}
	if _, err := f.service.commandPostConversation(t.Context(), f.scope(), f.record.ID, command); err != nil {
		t.Fatal(err)
	}

}

func configureDispatchGuardState(t *testing.T, f dispatchGuardFixture, state tracker.NativeState) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		if state.Terminal {
			state.Dispatchable = false
		}
		for _, target := range []tracker.NativeState{{Name: "Blocked", Transitions: []string{"Todo"}}, {Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}}} {
			if !slices.ContainsFunc(project.States, func(candidate tracker.NativeState) bool { return candidate.Name == target.Name }) {
				project.States = append(project.States, target)
			}
		}
		index := slices.IndexFunc(project.States, func(candidate tracker.NativeState) bool { return candidate.Name == state.Name })
		if index < 0 {
			project.States = append(project.States, state)
		} else {
			project.States[index] = state
		}
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func moveDispatchGuardFixture(t *testing.T, f dispatchGuardFixture, state string) {
	t.Helper()
	initial := f
	initial.completionBody, initial.worktreeState, initial.checkpoint = "", "clean", nil
	initial.succeed(t, &tracker.NativeDisposition{Status: "in_progress"})
	current := f.reload(t)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		for i := range project.States {
			if project.States[i].Name == current.State && !slices.Contains(project.States[i].Transitions, state) {
				project.States[i].Transitions = append(project.States[i].Transitions, state)
			}
		}
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.claim(t, false)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/workflow", f.worker, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "fixture-dispatch-lane", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ExpectedRevision: current.Revision, State: state, Reason: "worker_progress"})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &f.issue)
	f.startAttempt(t, tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "gpt-6-astra"})
}

func rewriteDispatchAttemptFixture(t *testing.T, f dispatchGuardFixture, mutate func(*tracker.NativeRunData)) {
	t.Helper()
	ctx := f.service.runDetailContext(t.Context())
	scope := f.scope()
	before, err := f.service.readNativeAttemptSnapshotTenant(ctx, scope, string(f.issue.WorkItemID), f.attempt)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.service.readNativeAttemptRunTenant(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	original, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&run)
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.runDetailStore().StoreDetail(ctx, string(scope.Organization), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNativeDetailReference(ctx, f.service.runDetailStore(), scope.Organization, ref, raw); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: f.service.config.Clock()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result, err := conversationTestAdmin(t, f.service).ExecContext(ctx, `UPDATE native_attempts SET payload_object_key=$1,claim_disposition=NULL WHERE organization_id=$2 AND id=$3 AND sequence=$4 AND payload_object_key=$5 AND lease_id=$6 AND fencing_token=$7 AND run_id=$8 AND status=$9`, ref.Key, scope.Organization, f.attempt, before.Attempt.Sequence, before.Attempt.PayloadObjectKey, before.Attempt.LeaseID, before.Attempt.FencingToken, before.Attempt.RunID, before.Attempt.Status)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		t.Fatalf("fixture attempt CAS rows%d: %v", count, err)
	}
	after, err := f.service.readNativeAttemptSnapshotTenant(ctx, scope, string(f.issue.WorkItemID), f.attempt)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := f.service.prepareAttemptClaimFacts(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { return w.SetAttemptClaimFacts(ctx, facts) }); err != nil {
		t.Fatal(err)
	}
	retained, err := f.service.readNativeAttemptRunTenant(ctx, before)
	if err != nil {
		t.Fatal(err)
	}
	old, err := json.Marshal(retained)
	if err != nil || string(old) != string(original) {
		t.Fatalf("fixture changed immutable attempt history: %v", err)
	}
}

func TestClaimCandidatesRequireUnansweredWorkItem(t *testing.T) {
	t.Parallel()
	instanceReport := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:worker-loopback\n    reason: sandbox refused listener with EPERM\nhuman_action: null\n```"
	definitionReport := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:tool\n    reason: approved portable project configuration and workflow prose unavailable\nhuman_action: null\n```"
	completeReport := "Source ready for finalization.\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
	for _, test := range []struct {
		name                 string
		disposition          *tracker.NativeDisposition
		finalMessage         string
		worktreeState        string
		manualHold           bool
		edited               bool
		continued            bool
		commented            bool
		wantCandidate        bool
		claimUnavailable     bool
		accepted             bool
		legacy               bool
		retainedSource       bool
		checkpointEdit       func(*tracker.NativeCheckpoint)
		otherVersion         bool
		newerChange          bool
		expiredLease         bool
		reviewOutcome        string
		machineVerdict       string
		implementationReview bool
		historicalReview     bool
		publication          string
		terminal             bool
		readAdmission        bool
		moveRefusal          bool
		moveGuidance         bool
		exclusion            string
		fullHost             bool
		aged                 bool
	}{
		{name: "continued instance cooldown exposes full host", finalMessage: instanceReport, continued: true, wantCandidate: true, readAdmission: true, fullHost: true},
		{name: "expired cooldown exposes full host", aged: true, wantCandidate: true, readAdmission: true, fullHost: true},
		{name: "completed instance report remains eligible", finalMessage: instanceReport, readAdmission: true, wantCandidate: true},
		{name: "completed definition blocker remains eligible with clean source", finalMessage: definitionReport, wantCandidate: true},
		{name: "completed definition blocker remains eligible with dirty source", finalMessage: definitionReport, worktreeState: "dirty", wantCandidate: true},
		{name: "ordinary comment remains eligible dirty definition blocker", finalMessage: definitionReport, worktreeState: "dirty", commented: true, wantCandidate: true},
		{name: "instance report remains runnable after revision edit", finalMessage: instanceReport, edited: true, wantCandidate: true, readAdmission: true},
		{name: "dirty definition blocker resumes after revision edit", finalMessage: definitionReport, worktreeState: "dirty", edited: true, wantCandidate: true},
		{name: "definition blocker resumes after continuation", finalMessage: definitionReport, continued: true, wantCandidate: true},
		{name: "dirty definition blocker resumes after continuation", finalMessage: definitionReport, worktreeState: "dirty", continued: true, wantCandidate: true},
		{name: "recorded prerequisite survives revision edit without relation", finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '#42'\n    reason: prerequisite must finish\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\nhuman_action: null\n```", edited: true, wantCandidate: true},
		{name: "instance report respects manual workflow hold", finalMessage: instanceReport, manualHold: true},
		{name: "instance and human action remain held", finalMessage: strings.Replace(instanceReport, "human_action: null", "human_action: Approve the exception", 1)},
		{name: "instance and external blocker remains eligible", finalMessage: strings.Replace(instanceReport, "human_action: null", "  - ref: '#42'\n    reason: Await dependency\nhuman_action: null", 1), wantCandidate: true},
		{name: "reason only human blocker remains eligible", finalMessage: strings.Replace(instanceReport, "  - ref: instance:worker-loopback\n", "", 1), wantCandidate: true},
		{name: "instance and reason code remain held", finalMessage: strings.Replace(instanceReport, "status: blocked", "status: blocked\nreason_code: permission_wait", 1)},
		{name: "native273 malformed predicate remains eligible", finalMessage: strings.Replace(instanceReport, "    reason:", "    predicate: instance_available\n    reason:", 1), wantCandidate: true},
		{name: "unfinished custom Rework remains runnable", disposition: &tracker.NativeDisposition{Status: "in_progress"}, wantCandidate: true},
		{name: "unfinished dirty source remains runnable", disposition: &tracker.NativeDisposition{Status: "in_progress"}, worktreeState: "dirty", wantCandidate: true},
		{name: "unaccepted completed custom Rework remains claimable", disposition: &tracker.NativeDisposition{Status: "complete"}, wantCandidate: true},
		{name: "accepted completion remains answered", disposition: &tracker.NativeDisposition{Status: "complete"}, accepted: true},
		{name: "unaccepted completed dirty source remains claimable", disposition: &tracker.NativeDisposition{Status: "complete"}, worktreeState: "dirty", wantCandidate: true},
		{name: "explicit blocker remains eligible", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, readAdmission: true, wantCandidate: true},
		{name: "nondispatchable blocker move retains ordinary refusal", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, manualHold: true, readAdmission: true, moveRefusal: true},
		{name: "terminal blocker move retains ordinary refusal", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, terminal: true, readAdmission: true, moveRefusal: true},
		{name: "dirty external blocker remains eligible", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, worktreeState: "dirty", wantCandidate: true},
		{name: "unfinished external blocker remains eligible", disposition: &tracker.NativeDisposition{Status: "in_progress", Blockers: true}, wantCandidate: true},
		{name: "unfinished human action keeps the answer", disposition: &tracker.NativeDisposition{Status: "in_progress", HumanAction: true}},
		{name: "missing legacy disposition remains eligible", readAdmission: true, wantCandidate: true},
		{name: "aged attempt without disposition or report retries", aged: true, wantCandidate: true, readAdmission: true},
		{name: "uncertain push without a disposition is reconciled by any runner", retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "git_push", "pending", hubapi.NewID("effect")
		}},
		{name: "legacy complete unpublished retained source is runnable", finalMessage: completeReport, legacy: true, retainedSource: true, wantCandidate: true},
		{name: "typed complete unpublished retained source is runnable", finalMessage: completeReport, retainedSource: true, wantCandidate: true},
		{name: "typed complete without legacy body is runnable", disposition: &tracker.NativeDisposition{Status: "complete"}, retainedSource: true, wantCandidate: true},
		{name: "legacy complete dirty retained source is runnable", finalMessage: completeReport, legacy: true, retainedSource: true, worktreeState: "dirty", wantCandidate: true},
		{name: "legacy retained source without report remains eligible", legacy: true, retainedSource: true, wantCandidate: true},
		{name: "aged legacy retained source without report retries", legacy: true, retainedSource: true, aged: true, wantCandidate: true},
		{name: "legacy instance report with retained source remains eligible", finalMessage: instanceReport, legacy: true, retainedSource: true, wantCandidate: true},
		{name: "legacy definition blocker with retained source remains eligible", finalMessage: definitionReport, legacy: true, retainedSource: true, wantCandidate: true},
		{name: "legacy human action with retained source stays answered", finalMessage: strings.Replace(completeReport, "human_action: null", "human_action: Approve the exception", 1), legacy: true, retainedSource: true},
		{name: "legacy external blocker with retained source remains eligible", finalMessage: strings.Replace(completeReport, "blockers: []", "blockers:\n  - ref: '#42'\n    reason: Await dependency", 1), legacy: true, retainedSource: true, wantCandidate: true},
		{name: "legacy malformed report with retained source remains eligible", finalMessage: strings.Replace(completeReport, "schema: 1", "schema: 2", 1), legacy: true, retainedSource: true, wantCandidate: true},
		{name: "invalid legacy reason is not human intent", wantCandidate: true, finalMessage: strings.Replace(completeReport, "status: complete", "status: complete\nreason_code: permission_wait", 1), legacy: true, retainedSource: true},
		{name: "typed blocker overrides legacy complete", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, finalMessage: completeReport, legacy: true, retainedSource: true, wantCandidate: true},
		{name: "typed human action overrides legacy complete", disposition: &tracker.NativeDisposition{Status: "complete", HumanAction: true}, finalMessage: completeReport, legacy: true, retainedSource: true},
		{name: "typed instance reason overrides legacy complete", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "instance_limitation"}, finalMessage: completeReport, legacy: true, retainedSource: true, wantCandidate: true},
		{name: "legacy in progress retained source remains eligible", finalMessage: strings.Replace(completeReport, "status: complete", "status: in_progress", 1), legacy: true, retainedSource: true, wantCandidate: true},
		{name: "legacy complete with missing head remains eligible", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) { c.HeadSHA = "" }, wantCandidate: true},
		{name: "legacy complete with missing digest remains eligible", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) { c.WorkspaceDigest = "" }, wantCandidate: true},
		{name: "legacy complete with unavailable source remains eligible", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) { c.Availability = "missing" }, wantCandidate: true, claimUnavailable: true},
		{name: "legacy complete requiring manual recovery remains eligible", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) { c.Resume = "manual_recovery" }, wantCandidate: true},
		{name: "pending publication is reconciled by any runner", finalMessage: completeReport, legacy: true, retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "git_push", "pending", hubapi.NewID("effect")
		}},
		{name: "ambiguous publication is reconciled by any runner", finalMessage: completeReport, legacy: true, retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "pr_create", "ambiguous", hubapi.NewID("effect")
		}},
		{name: "typed complete published current version waits for review", reviewOutcome: "approved", finalMessage: completeReport, retainedSource: true, publication: "owned", readAdmission: true, exclusion: "automated_review_missing"},
		{name: "published audit pass validator wait retries same source", finalMessage: completeReport, retainedSource: true, publication: "owned", machineVerdict: "wait", wantCandidate: true, readAdmission: true},
		{name: "published validator rework retries same source", finalMessage: completeReport, retainedSource: true, publication: "owned", machineVerdict: "rework", wantCandidate: true},
		{name: "published implementation wait retries same source", finalMessage: completeReport, retainedSource: true, publication: "owned", machineVerdict: "wait", implementationReview: true, wantCandidate: true},
		{name: "historical validator wait cannot release pending current review", finalMessage: completeReport, retainedSource: true, publication: "owned", machineVerdict: "wait", historicalReview: true, readAdmission: true, exclusion: "automated_review_missing"},
		{name: "published validator wait preserves human question", finalMessage: strings.Replace(completeReport, "human_action: null", "human_action: Approve the exception", 1), retainedSource: true, publication: "owned", machineVerdict: "wait", readAdmission: true, exclusion: "human_action"},
		{name: "same head cannot borrow an unrelated version", otherVersion: true, finalMessage: completeReport, retainedSource: true, publication: "owned", wantCandidate: true},
		{name: "newer unrelated draft does not hide published review wait", newerChange: true, reviewOutcome: "approved", finalMessage: completeReport, retainedSource: true, publication: "owned", readAdmission: true, exclusion: "automated_review_missing"},
		{name: "expired review wait leaves promotion to orchestrator", expiredLease: true, reviewOutcome: "approved", finalMessage: completeReport, retainedSource: true, publication: "owned", exclusion: "automated_review_missing"},
		{name: "published version resumes implementation for actionable findings", finalMessage: completeReport, retainedSource: true, publication: "owned", reviewOutcome: "changes_requested"},
		{name: "published version retries implementation after failed validation", finalMessage: completeReport, retainedSource: true, publication: "owned", reviewOutcome: "failed_check", wantCandidate: true},
		{name: "published version review preserves active claim owner", finalMessage: completeReport, retainedSource: true, publication: "owned", reviewOutcome: "active", wantCandidate: true},
		{name: "aged published current version still waits for review", finalMessage: completeReport, retainedSource: true, publication: "owned", aged: true, readAdmission: true, exclusion: "automated_review_missing"},
		{name: "legacy complete with genuine publication stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "owned"},
		{name: "legacy complete with published retained head remains eligible", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "head", wantCandidate: true},
		{name: "unrelated historical publication preserves retained source", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "unrelated", wantCandidate: true},
		{name: "earlier published head from same attempt preserves retained source", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "earlier", wantCandidate: true},
		{name: "aged instance blocker report retries", finalMessage: instanceReport, aged: true, wantCandidate: true, readAdmission: true},
		{name: "aged validation failure retries", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "validation_failed"}, aged: true, wantCandidate: true},
		{name: "aged moved head retries", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "head_moved"}, aged: true, wantCandidate: true},
		{name: "confirmed push without a disposition continues", retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "git_push", "confirmed", hubapi.NewID("effect")
		}},
		{name: "confirmed pull request without a disposition continues", retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "pr_create", "confirmed", hubapi.NewID("effect")
		}},
		{name: "aged human action stays held", disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}, aged: true, readAdmission: true, exclusion: "human_action"},
		{name: "aged permission wait stays held", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait"}, aged: true, readAdmission: true, exclusion: "permission_wait"},
		{name: "aged external blocker remains eligible", finalMessage: strings.Replace(instanceReport, "human_action: null", "  - ref: '#42'\n    reason: Await dependency\nhuman_action: null", 1), aged: true, wantCandidate: true},
		{name: "aged accepted completion stays held", disposition: &tracker.NativeDisposition{Status: "complete"}, accepted: true, aged: true, readAdmission: true, exclusion: "recorded_acceptance"},
		{name: "legacy complete preserves manual hold", finalMessage: completeReport, legacy: true, retainedSource: true, manualHold: true},
		{name: "legacy complete preserves terminal lane", finalMessage: completeReport, legacy: true, retainedSource: true, terminal: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDispatchGuardFixture(t)
			if test.worktreeState != "" {
				f.worktreeState = test.worktreeState
			}
			f.completionBody = test.finalMessage
			if test.retainedSource {
				f.worktreeState = "unpushed"
				if test.worktreeState != "" {
					f.worktreeState = test.worktreeState
				}
				f.checkpoint = nativeTestCheckpoint()
				f.checkpoint.WorktreeState = f.worktreeState
				f.checkpoint.HeadSHA = strings.Repeat("b", 40)
				f.checkpoint.WorkspaceDigest = strings.Repeat("d", 64)
				if test.checkpointEdit != nil {
					test.checkpointEdit(f.checkpoint)
				}
			}
			if test.finalMessage != "" && !test.legacy {
				if signal, reported := workpad.SignalFromComment(test.finalMessage, "", ""); reported && signal != nil && signal.Invalid == nil {
					test.disposition = &tracker.NativeDisposition{Status: signal.Status, Blockers: len(signal.Blockers) != 0, HumanAction: signal.HumanAction != "", ReasonCode: signal.ReasonCode, BlockerEvidence: signal.Blockers}
				}
			}
			configureDispatchGuardState(t, f, tracker.NativeState{Name: "Fixing", Dispatchable: !test.manualHold, Transitions: []string{"Merging", "Blocked"}})
			if test.moveRefusal {
				configureDispatchGuardState(t, f, tracker.NativeState{Name: "Fixing", Dispatchable: !test.manualHold, Transitions: []string{"Blocked"}})
			}
			moveDispatchGuardFixture(t, f, "Fixing")
			revision := f.reload(t).Revision
			f.acceptanceRecorded = test.accepted
			path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "/changes"
			var change tracker.ChangeRequest
			if test.publication != "" {
				response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.worker, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, Title: "Retained source", Body: "Owned source publication"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &change)
				if test.newerChange {
					response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.worker, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "unrelated-draft", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, Title: "Unrelated draft", Body: "No published source"})
					requireNativeStatus(t, response, http.StatusOK)
					var draft tracker.ChangeRequest
					decodeHubResponse(t, response, &draft)
					if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE change_requests SET created_at=$1 WHERE organization_id=$2 AND project_id=$3 AND id=$4", f.now.Add(time.Hour), f.project.OrganizationID, f.project.ID, draft.ID); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.finish(t, test.disposition)
			if test.aged {
				if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE native_attempts SET updated_at=$1 WHERE organization_id=$2 AND id=$3", f.now.Add(-2*time.Hour), f.project.OrganizationID, f.attempt); err != nil {
					t.Fatal(err)
				}
			}
			var published tracker.ChangeVersion
			var historicalVersion string
			var changePath string
			if test.reviewOutcome != "" {
				configureDispatchGuardState(t, f, tracker.NativeState{Name: "Fixing", Dispatchable: true, Transitions: []string{"Merging", "Blocked"}})
				configureDispatchGuardState(t, f, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}})
			}
			if test.publication != "" {
				rules := tracker.ChangeReviewPolicy{PolicyID: f.policy, RequireReview: true}
				if test.reviewOutcome != "" {
					principal, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
					if err != nil || status != http.StatusOK {
						t.Fatalf("fixture operator authority status%d: %v", status, err)
					}
					rules.RequiredChecks = []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: principal.ID, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules}), http.StatusOK)
				input := changeTestInput()
				if test.machineVerdict != "" {
					input.Validation = &gate.CommandResult{Command: "make check-landing", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 1}
				}
				if test.publication == "owned" || test.publication == "earlier" {
					input.RunID, input.AttemptID = f.run, f.attempt
				}
				if test.publication == "unrelated" || test.publication == "earlier" {
					input.HeadSHA = strings.Repeat("c", 40)
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ChangeVersionInput: input})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &published)
				if test.historicalReview {
					historicalVersion = published.ID
					response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "current-version", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ExpectedVersionID: published.ID, ChangeVersionInput: input})
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &published)
				}
				changePath = path + "/" + change.ID + "/versions/" + published.ID
				if test.publication == "owned" || test.publication == "earlier" {
					finalization := tracker.NativeFinalization{Settled: true, Changed: true, ChangeID: change.ID, VersionID: published.ID, HeadSHA: published.HeadSHA}
					if test.otherVersion {
						finalization.VersionID = "version_other"
					}
					rewriteDispatchAttemptFixture(t, f, func(run *tracker.NativeRunData) {
						run.Finalization = &finalization
					})
				}
				if test.machineVerdict != "" {
					audit := tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: published.ID, Decision: "approved", CreatedAt: f.now, SecurityAudit: &tracker.SecurityAuditReview{Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "audit-invocation", ProjectID: string(f.project.ID), Repository: published.Repository, BaseSHA: published.BaseSHA, HeadSHA: published.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: f.now, StartedAt: f.now.Add(-time.Second), CompletedAt: f.now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}}
					result := &gate.ValidatorResult{VersionID: published.ID, Submitted: true, Verdict: test.machineVerdict, Repository: published.Repository, BaseSHA: published.BaseSHA, HeadSHA: published.HeadSHA, NotVerified: []string{"Acceptance criterion requires production"}}
					review := tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: published.ID, Decision: "commented", CreatedAt: f.now, Validator: result}
					if test.implementationReview {
						review.ImplementationReview, review.Validator = result, nil
					}
					if test.historicalReview {
						review.VersionID, result.VersionID = historicalVersion, historicalVersion
					}
					for _, review := range []tracker.ChangeReview{audit, review} {
						raw, err := json.Marshal(review)
						if err != nil {
							t.Fatal(err)
						}
						row, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: f.project.OrganizationID, ChangeID: change.ID, Kind: "review"}, raw)
						if err != nil {
							t.Fatal(err)
						}
						if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
							_, err := w.AppendEvidence(t.Context(), row)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				}

				if test.reviewOutcome != "" {
					check := changeTestResult(published)
					if test.reviewOutcome == "failed_check" {
						check.Conclusion = "failure"
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/checks", f.token, check), http.StatusOK)
				}
				if test.reviewOutcome == "active" {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "early-review"}, Decision: "approved"}), http.StatusOK)
					if f.reload(t).State != "Fixing" {
						t.Fatal("review stole the active completion owner's lane")
					}
				}
			}
			if test.terminal {
				configureDispatchGuardState(t, f, tracker.NativeState{Name: "Fixing", Dispatchable: !test.manualHold, Terminal: true, Transitions: []string{"Blocked"}})
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/attempts/"+f.attempt, f.worker, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var attempt tracker.NativeAttempt
			decodeHubResponse(t, response, &attempt)
			if attempt.Status != "succeeded" || attempt.Outcome != "succeeded" || attempt.CompletionBody != test.finalMessage || attempt.Checkpoint == nil || attempt.Checkpoint.WorktreeState != f.worktreeState || attempt.WorkItemRevision != revision || attempt.DispatchGeneration != 0 || test.legacy && test.disposition == nil && attempt.Disposition != nil {
				t.Fatalf("completion lost successful current-revision evidence: %#v", attempt)
			}
			claim := tracker.NativeClaim{PolicyID: f.policy, WorkItemID: f.issue.WorkItemID, MachineID: f.runner.binding.MachineID, SessionID: hubapi.NewID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim), http.StatusConflict)
			if test.expiredLease {
				if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE leases SET acquired_at=LEAST(acquired_at,$1::timestamptz-interval '1 minute'),renewed_at=LEAST(renewed_at,$1::timestamptz-interval '1 minute'),expires_at=$1 WHERE organization_id=$2 AND lease_id=$3", f.now.Add(-time.Minute), f.project.OrganizationID, f.lease.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.release(t)
			}
			if f.reload(t).Revision != revision {
				t.Fatal("completion fabricated an item edit")
			}
			if (test.edited || test.continued) && !f.candidate(t) {
				t.Fatal("historical failure vetoed work before fresh context")
			}
			if test.edited {
				title := "Updated title"
				response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-title"}, ExpectedRevision: revision, Title: &title})
				requireNativeStatus(t, response, http.StatusOK)
			}
			if test.continued {
				f.continueConversation(t, "continue-definition")
				if f.reload(t).Revision != revision {
					t.Fatal("continuation fabricated an item edit")
				}
			}
			if test.commented {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "context-comment"}, Body: "Additional context for review."}), http.StatusOK)
				if f.reload(t).Revision != revision {
					t.Fatal("ordinary comment fabricated an item edit")
				}
			}
			var generation int64
			var wantGeneration int64
			if test.continued {
				wantGeneration = 1
			}
			err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.WorkItem(t.Context(), f.project.ID, f.issue.WorkItemID)
				generation = row.DispatchGeneration
				return err
			})
			if err != nil || generation != wantGeneration {
				t.Fatalf("dispatch generation=%d, want %d, error=%v", generation, wantGeneration, err)
			}
			if got := f.candidate(t); got != test.wantCandidate {
				t.Fatalf("candidate=%t, want %t", got, test.wantCandidate)
			}
			if test.readAdmission {
				r := f.runner
				context := tracker.NativeAdmissionContext{ObservedAt: f.now, PolicyID: f.policy, WorkflowStates: []string{"Fixing"}}
				payload := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation, "admission": map[string]any{"context": context, "runner_revision": 1}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, payload), http.StatusOK)
				if test.fullHost {
					if _, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), "UPDATE machines SET capacity=0 WHERE organization_id=$1 AND id=$2", f.project.OrganizationID, r.binding.MachineID); err != nil {
						t.Fatal(err)
					}
				}
				response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/runtime", f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var current tracker.NativeRuntimeEvidence
				decodeHubResponse(t, response, &current)
				if len(current.Admission) != 1 {
					t.Fatalf("missing current admission: %s", response.Body)
				}
				a := current.Admission[0]
				if test.exclusion != "" {
					if a.AnsweredExclusion == nil || a.AnsweredExclusion.Kind != test.exclusion || a.AnsweredExclusion.AttemptID != f.attempt {
						t.Fatalf("missing answered classification: %s", response.Body)
					}
					deadline := a.AnsweredExclusion.RetryEligibleAt
					if strings.HasSuffix(test.exclusion, "_cooldown") {
						if deadline == nil || !deadline.Equal(attempt.UpdatedAt.Add(30*time.Minute)) {
							t.Fatalf("wrong existing cooldown deadline: %s", response.Body)
						}
					} else if deadline != nil {
						t.Fatalf("authoritative hold acquired deadline: %s", response.Body)
					}
				} else if a.AnsweredExclusion != nil {
					t.Fatalf("old answer still excludes current generation: %s", response.Body)
				}
				if test.fullHost && a.ReasonCode != "host_capacity" {
					t.Fatalf("old cooldown hid current host capacity: %s", response.Body)
				}
				ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
				call := hostedContextProtocol(t, f.service, ctx, "http")
				if test.moveRefusal {
					before := f.reload(t)
					attemptPath := f.base + "/work-items/" + string(f.issue.WorkItemID) + "/attempts/" + f.attempt
					response := performHubAPIRequest(t, f.service, http.MethodGet, attemptPath, f.worker, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var beforeAttempt tracker.NativeAttempt
					decodeHubResponse(t, response, &beforeAttempt)
					for _, move := range []struct {
						name, identifier string
						denied           bool
					}{
						{"same state", string(f.issue.WorkItemID), false},
						{"foreign scope", "prj_foreign#1", true},
					} {
						reply := call("tools/call", operatortool.MoveItem, map[string]any{"project_id": string(f.project.ID), "request_id": move.name, "identifier": move.identifier, "expected_revision": strconv.FormatInt(int64(before.Revision), 10), "target_state": before.State})
						raw := hostedContextData(t, reply, true)
						if move.denied {
							want := "work item not found in this project"
							var result struct {
								Content []struct{ Text string } `json:"content"`
							}
							if err := json.Unmarshal(reply.Result, &result); err != nil || len(result.Content) != 1 || result.Content[0].Text != want || len(raw) != 0 {
								t.Fatalf("%s refusal changed: %s %s", move.name, reply.Result, reply.Error)
							}
							continue
						}
						var refusal operatortool.RequestError
						wantCode := "invalid_request"
						if !test.moveGuidance {
							wantCode = "transition_not_allowed"
						}
						if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Code != wantCode {
							t.Fatalf("%s refusal=%s result=%s rpc_error=%s err=%v", move.name, raw, reply.Result, reply.Error, err)
						}
						if !test.moveGuidance {
							if refusal.Message != "Workflow transition is not allowed" {
								t.Fatalf("%s refusal changed: %s", move.name, raw)
							}
							continue
						}
						for _, direction := range []string{"linked issue conversation", "continue", "/conversations/{conversation}/commands", "expected.attempt_id", "explain_item", "snapshot", "recorded owners", "external prerequisite", "resume_local_project"} {
							if !strings.Contains(refusal.Message, direction) {
								t.Fatalf("%s recovery direction %q: %s", move.name, direction, raw)
							}
						}
					}
					response = performHubAPIRequest(t, f.service, http.MethodGet, attemptPath, f.worker, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var after tracker.NativeAttempt
					decodeHubResponse(t, response, &after)
					if !reflect.DeepEqual(f.reload(t), before) || !reflect.DeepEqual(after, beforeAttempt) || f.candidate(t) {
						t.Fatal("refused move changed item, retained source, blocker or dispatch authority")
					}
				}
				for _, name := range []string{operatortool.ExplainItem, operatortool.BoardSession} {
					data := hostedContextData(t, call("tools/call", name, map[string]any{"project_id": string(f.project.ID), "reference": strconv.Itoa(f.issue.Number)}), false)
					var projected tracker.NativeRuntimeEvidence
					if name == operatortool.ExplainItem {
						var explanation explain.IssueExplanation
						if err := json.Unmarshal(data, &explanation); err != nil {
							t.Fatal(err)
						}
						if explanation.NativeRuntime == nil {
							t.Fatalf("MCP dropped current runtime: %s", data)
						}
						projected = *explanation.NativeRuntime
						if a.Outcome == "skipped" && (len(explanation.Eligibility.Refusals) != 1 || !reflect.DeepEqual(explanation.Eligibility.Refusals[0].AnsweredExclusion, a.AnsweredExclusion) || explanation.Eligibility.Refusals[0].ReasonCode != a.ReasonCode || explanation.Eligibility.Refusals[0].Historical) {
							t.Fatalf("MCP dropped current refusal: %s", data)
						}
					} else {
						var session operatortool.WorkReadResult[tracker.NativeRuntimeEvidence]
						if err := json.Unmarshal(data, &session); err != nil {
							t.Fatal(err)
						}
						projected = session.Data
					}
					if !reflect.DeepEqual(projected.Admission, current.Admission) || !reflect.DeepEqual(projected.LatestDecision, current.LatestDecision) {
						t.Fatalf("MCP changed current admission or historical decision: %s", data)
					}
				}
			}
			if test.wantCandidate && !test.claimUnavailable {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.nativeFixture.worker(t, "other-worker"), claim), http.StatusNotFound)
				f.claim(t)
				if f.lease.FencingToken <= attempt.FencingToken {
					t.Fatal("reclaim did not grant newer fenced authority")
				}
				if test.edited || test.continued {
					f.succeed(t, test.disposition)
					if !f.candidate(t) {
						t.Fatal("fresh failure report vetoed current work")
					}
				}
			} else {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim), http.StatusConflict)
			}
			if test.reviewOutcome != "" {
				decision := "approved"
				if test.reviewOutcome == "changes_requested" {
					decision = "changes_requested"
				}
				request := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "review-after-wait"}, Decision: decision, Body: "Review current source"}
				if _, err := f.service.config.TenantRepository.(*postgres.Repository).BackfillCurrentChangeReviews(t.Context(), f.project.OrganizationID, eventstore.NewContent(f.service.runDetailStore()), corestore.MaxPageSize); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/reviews", f.token, request), http.StatusOK)
				want := "Fixing"
				if current := f.reload(t); current.State != want {
					t.Fatalf("review result lane=%s want=%s", current.State, want)
				}
				if got := f.candidate(t); !got {
					t.Fatalf("review result candidate=%t lane=%s", got, want)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/reviews", f.token, request), http.StatusOK)
				if current := f.reload(t); current.State != want {
					t.Fatal("review replay changed lane")
				}
			}
		})
	}
	f := newDispatchGuardFixture(t)

	if !f.candidate(t) {
		t.Fatal("a running attempt must not remove the item from the candidate query; the lease does that")
	}

	f.succeed(t, &tracker.NativeDisposition{Status: "blocked", HumanAction: true})
	if f.candidate(t) {
		t.Fatal("a succeeded attempt on an unchanged item is still offered: this is the re-dispatch loop")
	}

	// A conversation continuation bumps no revision and moves no lane, so it
	// has to be written down as an explicit request.
	f.continueConversation(t, "continue-1")
	if !f.candidate(t) {
		t.Fatal("a continuation must re-offer the item")
	}

	f.claim(t)
	f.succeed(t, &tracker.NativeDisposition{Status: "blocked", HumanAction: true})
	if f.candidate(t) {
		t.Fatal("the continuation's own attempt must not re-offer the item again")
	}

	// An edit moves the item on, so the succeeded attempt answered an older
	// version of it.
	issue := f.reload(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token, map[string]any{
		"idempotency_key": hubapi.NewID("edit"), "expected_revision": fmt.Sprint(issue.Revision), "title": "Edited after the attempt",
	}), http.StatusOK)
	if !f.candidate(t) {
		t.Fatal("an edit must re-offer the item")
	}

	f.claim(t)
	f.succeed(t, &tracker.NativeDisposition{Status: "blocked", HumanAction: true})
	if f.candidate(t) {
		t.Fatal("the edit's own attempt must not re-offer the item again")
	}

	// A person moving the item back into a dispatchable lane is a workflow
	// transition, which is also an edit of the item.
	issue = f.reload(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/workflow", f.token, map[string]any{
		"idempotency_key": hubapi.NewID("move"), "expected_revision": fmt.Sprint(issue.Revision), "state": "In Progress", "reason": "user_requested",
	}), http.StatusOK)
	if !f.candidate(t) {
		t.Fatal("a manual move back to a dispatchable lane must re-offer the item")
	}
}

func TestClaimCandidatesKeepOfferingUnsuccessfulAttempts(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		outcome    string
		checkpoint string
		candidate  bool
	}{
		{name: "failed", outcome: "failed", candidate: true},
		{name: "cancelled", outcome: "cancelled", candidate: true},
		{name: "interrupted", outcome: "interrupted", candidate: true},
		{name: "successful dirty source without disposition", outcome: "succeeded", checkpoint: "dirty", candidate: true},
		{name: "successful finalized unpushed source", outcome: "succeeded", checkpoint: "unpushed", candidate: true},
		{name: "successful clean source", outcome: "succeeded", checkpoint: "clean", candidate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDispatchGuardFixture(t)
			event := tracker.NativeRunEvent{
				Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("finish")}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{
					Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
					LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: test.outcome,
				},
			}
			if test.checkpoint != "" {
				checkpoint := event
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Outcome = "run.checkpointed", hubapi.NewID("checkpoint"), ""
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				checkpoint.Data.Handoff.WorktreeState = test.checkpoint
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, checkpoint), http.StatusOK)
				event.Data.Sequence++
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
			event.IdempotencyKey = hubapi.NewID("replay-finish")
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
			f.release(t)
			if got := f.candidate(t); got != test.candidate {
				t.Fatalf("claim candidate = %t, want %t for %s with %s checkpoint", got, test.candidate, test.outcome, test.checkpoint)
			}
		})
	}
}

// TestRecordNativeAttemptCapturesDispatchState proves the two numbers the
// claim guard reads are the ones the attempt was dispatched for, recorded at
// run.started.
func TestRecordNativeAttemptCapturesDispatchState(t *testing.T) {
	t.Parallel()
	f := newDispatchGuardFixture(t)

	var revision, generation int64
	read := func(t *testing.T) {
		t.Helper()
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.Attempt(t.Context(), f.attempt)
			revision, generation = row.WorkItemRevision, row.DispatchGeneration
			return err
		}); err != nil {
			t.Fatalf("read attempt dispatch state: %v", err)
		}
	}
	read(t)
	issue := f.reload(t)
	if revision != int64(issue.Revision) {
		t.Fatalf("attempt work_item_revision = %d, want the item's revision %d", revision, issue.Revision)
	}
	if generation != 0 {
		t.Fatalf("attempt dispatch_generation = %d, want 0 before any request", generation)
	}

	f.succeed(t)
	f.continueConversation(t, "continue-1")
	f.claim(t)
	read(t)
	if generation != 1 {
		t.Fatalf("continuation attempt dispatch_generation = %d, want the request it was dispatched for", generation)
	}
}

func TestNativeDerivedHoldDispatchEquivalence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		disposition *tracker.NativeDisposition
		state       string
		dependency  bool
		want        bool
	}{
		{name: "external blocker", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, want: true},
		{name: "instance limitation", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "instance_limitation"}, want: true},
		{name: "untyped", want: true},
		{name: "human action", disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true, Blockers: true}},
		{name: "permission", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait", Blockers: true}},
		{name: "current dependency", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, dependency: true},
		{name: "operator lane", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, state: "Blocked"},
		{name: "terminal lane", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, state: "Done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDispatchGuardFixture(t)
			if test.dependency {
				blocker := f.create(t, "Current prerequisite")
				request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "dependency"}, ExpectedRevision: f.reload(t).Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/dependencies", f.token, request), http.StatusOK)
			}
			f.succeed(t, test.disposition)
			if test.state != "" {
				current := f.reload(t)
				configureDispatchGuardState(t, f, tracker.NativeState{Name: current.State, Dispatchable: false, Terminal: test.state == "Done"})
			}
			before := f.candidate(t)
			rewriteDispatchAttemptFixture(t, f, func(run *tracker.NativeRunData) {
				if run.Disposition != nil {
					run.Disposition.Blockers = false
					run.Disposition.BlockerEvidence = nil
					run.Disposition.Status = ""
					if run.Disposition.ReasonCode != "permission_wait" {
						run.Disposition.ReasonCode = ""
					}
				}
			})
			after := f.candidate(t)
			if before != test.want || after != test.want {
				t.Fatalf("before=%t after=%t want=%t", before, after, test.want)
			}
		})
	}
}

func TestNativeRecordedDependencyUsesCurrentEdges(t *testing.T) {
	t.Parallel()
	f := newDispatchGuardFixture(t)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		record.Core.RequireDependencies = true
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("fixture authority status%d: %v", status, err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	candidate := func(t *testing.T) bool {
		t.Helper()
		found := false
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			if err := f.service.authorizeAccessScope(t.Context(), r, scope, false); err != nil {
				return err
			}
			page, err := r.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: scope.Project, Scope: string(scope.Project), WorkItemID: f.issue.WorkItemID, AvailableAt: f.service.config.Clock(), Limit: 1})
			found = len(page.Items) == 1
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return found
	}
	blockers := []tracker.NativeIssue{f.create(t, "First prerequisite"), f.create(t, "Second prerequisite")}
	for _, blocker := range blockers {
		current := f.reload(t)
		request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: string(blocker.WorkItemID)}, ExpectedRevision: current.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/dependencies", f.token, request), http.StatusOK)
	}
	f.succeed(t, &tracker.NativeDisposition{Status: "blocked", Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: "prerequisite", Predicate: &workpad.Predicate{Type: workpad.PredicateIssueState}}}})
	if candidate(t) {
		t.Fatal("unfinished dependencies allowed a candidate")
	}
	for index, blocker := range blockers {
		request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "finish-" + string(blocker.WorkItemID)}, ExpectedRevision: blocker.Revision, State: "Done", Reason: "user_requested"}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(blocker.WorkItemID)+"/workflow", f.token, request), http.StatusOK)
		if got, want := candidate(t), index == len(blockers)-1; got != want {
			t.Fatalf("after prerequisite %d: candidate=%t want=%t", index, got, want)
		}
	}
	before, err := f.service.readNativeAttemptSnapshotTenant(t.Context(), scope, string(f.issue.WorkItemID), f.attempt)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.service.readNativeAttemptRunTenant(t.Context(), before)
	if err != nil {
		t.Fatal(err)
	}
	if run.Disposition == nil || len(run.Disposition.BlockerEvidence) != 1 {
		t.Fatalf("missing historical dependency evidence: %+v", run.Disposition)
	}
	run.Disposition = nil
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(scope.Organization), raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateNativeDetailReference(t.Context(), f.service.runDetailStore(), scope.Organization, ref, raw); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: scope.Organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: f.service.config.Clock()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	result, err := conversationTestAdmin(t, f.service).ExecContext(t.Context(), `UPDATE native_attempts SET payload_object_key=$1,claim_disposition='' WHERE organization_id=$2 AND id=$3 AND sequence=$4 AND payload_object_key=$5 AND lease_id=$6 AND fencing_token=$7 AND run_id=$8 AND status=$9`, ref.Key, scope.Organization, f.attempt, before.Attempt.Sequence, before.Attempt.PayloadObjectKey, before.Attempt.LeaseID, before.Attempt.FencingToken, before.Attempt.RunID, before.Attempt.Status)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("historical fixture CAS rows%d: %v", changed, err)
	}
	after, err := f.service.readNativeAttemptSnapshotTenant(t.Context(), scope, string(f.issue.WorkItemID), f.attempt)
	if err != nil {
		t.Fatal(err)
	}
	cleared, err := f.service.readNativeAttemptRunTenant(t.Context(), after)
	if err != nil {
		t.Fatal(err)
	}
	expected := before.Attempt
	expected.PayloadObjectKey = ref.Key
	expected.ClaimDisposition = new("")
	if cleared.Disposition != nil || !reflect.DeepEqual(after.Attempt, expected) || !reflect.DeepEqual(after.Lease, before.Lease) {
		t.Fatal("historical fixture changed attempt/lease identity or retained disposition")
	}
	original, err := f.service.readNativeAttemptRunTenant(t.Context(), before)
	if err != nil || original.Disposition == nil {
		t.Fatalf("original immutable history lost: %v", err)
	}
	if !candidate(t) {
		t.Fatal("wiping historical dependency evidence changed eligibility")
	}
}

func TestHubOwnedBlockedRecoveryClaimsLegacyInstanceHold(t *testing.T) {
	f := newDispatchGuardFixture(t)
	f.failure = &tracker.NativeTerminalFailure{ErrorClass: tracker.WorkspaceSharedMemoryExhausted, Stage: "workspace_preparation", Source: "runner", Coverage: "complete", ObservedAt: f.now, Summary: "Example workspace setup failed"}
	f.completionBody = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Restore the host shared-memory capacity.\n```"
	blocked := legacyInstanceBlock(t, f)
	claim := hubOwnedRecoveryClaim(f, "recover-instance")
	path := f.base + "/work-items/" + string(blocked.WorkItemID)
	var recovery tracker.NativeRecovery
	read := performHubAPIRequest(t, f.service, http.MethodGet, path+"?view=recovery", f.worker, nil)
	requireNativeStatus(t, read, http.StatusOK)
	decodeHubResponse(t, read, &recovery)
	if recovery.ContinuationEligible == nil || !*recovery.ContinuationEligible || f.reload(t).Revision != blocked.Revision {
		t.Fatalf("diagnostic recovery eligibility=%v at revision %d", recovery.ContinuationEligible, f.reload(t).Revision)
	}
	var preview tracker.NativeCapacityPage
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", f.worker, tracker.NativeCapacityPreview{NativeClaim: claim})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &preview)
	if len(preview.Items) != 1 || preview.Items[0].State != "Todo" || preview.Items[0].Revision != blocked.Revision || f.reload(t).State != "Blocked" {
		t.Fatalf("preview changed or omitted the legacy hold: %+v", preview.Items)
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != blocked.WorkItemID || lease.FencingToken <= f.lease.FencingToken {
		t.Fatalf("claim = %+v", lease)
	}
	current := f.reload(t)
	if current.State != "Todo" || current.Revision != blocked.Revision+1 {
		t.Fatalf("recovered item %s at %d, want Todo at %d", current.State, current.Revision, blocked.Revision+1)
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var replay tracker.NativeLease
	decodeHubResponse(t, response, &replay)
	if replay.ID != lease.ID || f.reload(t).Revision != current.Revision {
		t.Fatal("claim replay changed the lease or applied another transition")
	}
}

func seedHubOwnedBlockedRecovery(t *testing.T, f nativeFixture, issue tracker.NativeIssue, actor tracker.Actor) tracker.NativeIssue {
	t.Helper()
	from := issue.State
	issue.State = "Blocked"
	issue.Revision++
	content := eventstore.NewContent(f.service.runDetailStore())
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.LockWorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
		if err != nil {
			return err
		}
		row.WorkflowStateName = &issue.State
		if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	appendTestHealthEventAs(t, f, actor, issue, "workflow.transitioned", tracker.CollaborationData{Revision: issue.Revision, FromState: from, ToState: issue.State, Reason: "worker_progress"}, f.service.config.Clock())
	return issue
}

func configureHubOwnedBlockedRecovery(t *testing.T, f dispatchGuardFixture) {
	t.Helper()
	configureDispatchGuardState(t, f, tracker.NativeState{Name: "Cancelled", Terminal: true})
	configureDispatchGuardState(t, f, tracker.NativeState{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done", "Blocked", "Cancelled"}})
}

func legacyInstanceBlock(t *testing.T, f dispatchGuardFixture) tracker.NativeIssue {
	t.Helper()
	configureHubOwnedBlockedRecovery(t, f)
	if f.checkpoint == nil {
		f.checkpoint = nativeTestCheckpoint()
		f.checkpoint.Resume, f.checkpoint.WorktreeState = "fresh_checkout", "clean"
	}
	disposition := &tracker.NativeDisposition{Status: workpad.StatusBlocked, HumanAction: true}
	if f.failure == nil {
		disposition.ReasonCode = "instance_limitation"
		disposition.BlockerEvidence = []workpad.Blocker{{Owner: workpad.BlockerOwnerInstance, Ref: "instance:postgres"}}
	}
	f.succeed(t, disposition)
	return seedHubOwnedBlockedRecovery(t, f.nativeFixture, f.reload(t), tracker.Actor{Kind: "runner", PrincipalID: f.runner.binding.RunnerID})
}

func hubOwnedRecoveryClaim(f dispatchGuardFixture, session string) tracker.NativeClaim {
	return tracker.NativeClaim{PolicyID: f.policy, MachineID: f.runner.binding.MachineID, ProtocolMajor: tracker.NativeProtocolMajor, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: []string{"Todo"}, SessionID: session, TTLSeconds: 600}
}

func TestHubOwnedBlockedRecoveryUsesDependencyPredicates(t *testing.T) {
	for _, merged := range []bool{false, true} {
		t.Run(fmt.Sprintf("requires merge=%t", merged), func(t *testing.T) {
			f := newDispatchGuardFixture(t)
			configureHubOwnedBlockedRecovery(t, f)
			prerequisite := f.create(t, "Example prerequisite")
			path := f.base + "/work-items/" + string(f.issue.WorkItemID)
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/dependencies", f.token, tracker.DependencyMutation{
				Mutation: tracker.Mutation{IdempotencyKey: "record-prerequisite"}, ExpectedRevision: f.reload(t).Revision, RelatedWorkItemID: prerequisite.WorkItemID, Operation: "add",
			})
			requireNativeStatus(t, response, http.StatusOK)
			ref := fmt.Sprintf("%s#%d", f.project.ID, prerequisite.Number)
			predicate := &workpad.Predicate{Type: workpad.PredicateIssueState, Identifier: ref, States: []string{"open"}}
			if merged {
				predicate.Type, predicate.States = workpad.PredicatePullRequestState, []string{"open", "closed", "missing"}
			}
			f.succeed(t, &tracker.NativeDisposition{Status: workpad.StatusBlocked, Blockers: true, BlockerEvidence: []workpad.Blocker{{Ref: ref, Owner: workpad.BlockerOwnerOrchestrator, Predicate: predicate}}})
			blocked := seedHubOwnedBlockedRecovery(t, f.nativeFixture, f.reload(t), tracker.Actor{Kind: "runner", PrincipalID: f.runner.binding.RunnerID})
			claim := hubOwnedRecoveryClaim(f, "recover-dependency")
			claim.WorkItemID = blocked.WorkItemID
			assertHeld := func() {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
				if response.Code == http.StatusOK || f.reload(t).Revision != blocked.Revision {
					t.Fatalf("unresolved prerequisite released the hold: %s", response.Body.String())
				}
			}
			assertHeld()
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(prerequisite.WorkItemID)+"/workflow", f.token, tracker.Transition{
				Mutation: tracker.Mutation{IdempotencyKey: "cancel-prerequisite"}, ExpectedRevision: prerequisite.Revision, State: "Cancelled", Reason: "user_requested",
			}), http.StatusOK)
			if merged {
				assertHeld()
				seedAnalyticsTestChange(t, f.service, f.scope(), prerequisite.WorkItemID, "merged-prerequisite", f.now,
					&tracker.ChangeLanding{MergeSHA: strings.Repeat("c", 40), BaseRef: "develop", Method: "merge", LandedAt: f.now})
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
			requireNativeStatus(t, response, http.StatusOK)
			if after := f.reload(t); after.State != "Todo" || after.Revision != blocked.Revision+1 {
				t.Fatalf("resolved prerequisite left state=%q revision=%d", after.State, after.Revision)
			}
		})
	}
}

func TestHubOwnedBlockedRecoveryPreservesHumanIntent(t *testing.T) {
	t.Run("operator stop", func(t *testing.T) {
		f := newDispatchGuardFixture(t)
		configureHubOwnedBlockedRecovery(t, f)
		f.succeed(t, &tracker.NativeDisposition{Status: workpad.StatusBlocked, ReasonCode: "instance_limitation"})
		path := f.base + "/work-items/" + string(f.issue.WorkItemID)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.token, tracker.Transition{
			Mutation: tracker.Mutation{IdempotencyKey: "operator-hold"}, ExpectedRevision: f.reload(t).Revision, State: "Blocked", Reason: "user_requested",
		}), http.StatusOK)
		stopped := f.reload(t)
		claim := hubOwnedRecoveryClaim(f, "held-by-operator")
		claim.WorkItemID = stopped.WorkItemID
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
		if response.Code == http.StatusOK || f.reload(t).Revision != stopped.Revision {
			t.Fatalf("operator-stopped item was released: %s", response.Body.String())
		}
		var history tracker.Page[tracker.CollaborationEvent]
		decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history?limit=100", f.token, nil), &history)
		if !slices.ContainsFunc(history.Items, func(event tracker.CollaborationEvent) bool {
			return event.Type == "workflow.transitioned" && event.Actor.Kind == "human" && event.Data.ToState == "Blocked" && event.Data.Operation == "operator_stop"
		}) {
			t.Fatal("operator stop is missing typed evidence")
		}
	})
	t.Run("separate product question", func(t *testing.T) {
		f := newDispatchGuardFixture(t)
		f.completionBody = "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - owner: human\n    reason: Product approval\nhuman_action: Choose the default visibility.\n```"
		blocked := legacyInstanceBlock(t, f)
		claim := hubOwnedRecoveryClaim(f, "held-by-question")
		claim.WorkItemID = blocked.WorkItemID
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
		if response.Code == http.StatusOK || f.reload(t).Revision != blocked.Revision {
			t.Fatalf("separate product question was released: %s", response.Body.String())
		}
		f.issue = f.create(t, "Independent issue")
		f.claim(t)
		f.completionBody = ""
		recoverable := legacyInstanceBlock(t, f)
		claim.WorkItemID, claim.SessionID = recoverable.WorkItemID, "no-product-question"
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim), http.StatusOK)
	})
}

type hubOwnedRecoverySource struct {
	ReconcileBackend
	t       *testing.T
	baseSHA string
	current bool
}

func (s *hubOwnedRecoverySource) CommitOnCurrentBase(_ context.Context, repository, branch, commit string) (bool, error) {
	s.t.Helper()
	if repository != "example-org/example-project" || branch != "develop" || commit != s.baseSHA {
		s.t.Errorf("unexpected source proof: %q %q %q", repository, branch, commit)
		return false, nil
	}
	return s.current, nil
}

func TestHubOwnedBlockedRecoveryVerifiesCurrentBase(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	const repository = "example-org/example-project"
	coordinatorFixtureProject(t, f.service, f.project.OrganizationID, f.project.ID, func(row *projectstore.Record) {
		row.Settings.CheckoutRepository = repository
	})
	changeID := hubapi.NewID("change")
	version := tracker.ChangeVersion{
		ID: hubapi.NewID("version"), ChangeID: changeID, Number: 1, CreatedAt: f.now, Actor: tracker.Actor{Kind: "runner", PrincipalID: f.runner.binding.RunnerID},
		ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("a", 40), PolicyID: f.policy, Repository: repository, AttemptID: f.attempt},
	}
	change := tracker.ChangeRequest{ID: changeID, OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: f.issue.WorkItemID, CurrentVersion: version.ID, Revision: 1, Title: "Source change", CreatedAt: f.now, UpdatedAt: f.now}
	content := eventstore.NewContent(f.service.runDetailStore())
	changeRow, err := content.PrepareChange(t.Context(), f.project.OrganizationID, historyTestJSON(t, change))
	if err != nil {
		t.Fatal(err)
	}
	versionRow, err := content.PrepareVersion(t.Context(), f.project.OrganizationID, historyTestJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateChange(t.Context(), changeRow); err != nil {
			return err
		}
		if _, err := w.CreateVersion(t.Context(), versionRow); err != nil {
			return err
		}
		return w.PutPRPublication(t.Context(), eventstore.NativePRPublication{
			OrganizationID: f.project.OrganizationID, VersionID: version.ID, ChangeID: version.ChangeID, HeadSHA: version.HeadSHA,
			Repository: repository, BaseRef: "develop", Branch: "example-branch", PolicyID: f.policy, RecorderAttemptID: f.attempt,
			External: tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://github.com/" + repository + "/pull/1"},
		})
	}); err != nil {
		t.Fatal(err)
	}
	source := &hubOwnedRecoverySource{t: t, baseSHA: strings.Repeat("a", 40)}
	f.service.config.ReconcileBackend = source
	f.checkpoint = &tracker.NativeCheckpoint{Resume: "resume_session", Storage: "git_ref", Availability: "available", WorktreeState: "clean", ExternalEffect: "none", EffectState: "none",
		Ref: tracker.CheckpointRefPrefix + "example", BaseSHA: source.baseSHA, CommitSHA: version.HeadSHA, TreeSHA: strings.Repeat("c", 40),
		Change: &tracker.NativeChangeReference{ChangeID: version.ChangeID, VersionID: version.ID, HeadSHA: version.HeadSHA},
	}
	f.succeed(t, &tracker.NativeDisposition{Status: workpad.StatusBlocked, ReasonCode: "stale_base", HumanAction: true})
	blocked := seedHubOwnedBlockedRecovery(t, f.nativeFixture, f.reload(t), tracker.Actor{Kind: "runner", PrincipalID: f.runner.binding.RunnerID})
	claim := hubOwnedRecoveryClaim(f, "recover-stale-base")
	claim.WorkItemID = blocked.WorkItemID
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim)
	if response.Code == http.StatusOK || f.reload(t).Revision != blocked.Revision {
		t.Fatalf("unverified base was released: %s", response.Body.String())
	}
	source.current = true
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim), http.StatusOK)
}

func TestHubOwnedBlockedRecoveryAllowsOnlyOneClaim(t *testing.T) {
	f := newDispatchGuardFixture(t)
	blocked := legacyInstanceBlock(t, f)
	second := conversationWorkerRunner(t, f.nativeFixture)
	reportRunnerCapacity(t, f.service, f.base, second.redemption.Credential, second.binding.MachineID, second.redemption.DisplayName, second.redemption.BackendIsolation, second.redemption.Capacity)
	start := make(chan struct{})
	results := make(chan int, 2)
	for _, current := range []runnerFixture{f.runner, second} {
		go func() {
			<-start
			claim := hubOwnedRecoveryClaim(f, "recover-"+current.binding.RunnerID)
			claim.WorkItemID, claim.MachineID = blocked.WorkItemID, current.binding.MachineID
			results <- performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", current.redemption.Credential, claim).Code
		}()
	}
	close(start)
	var successful int
	for range 2 {
		if <-results == http.StatusOK {
			successful++
		}
	}
	if successful != 1 || f.reload(t).Revision != blocked.Revision+1 {
		t.Fatalf("successful claims=%d state=%+v", successful, f.reload(t))
	}
}

func TestHubOwnedBlockedRecoveryUsesNextRunnerReadiness(t *testing.T) {
	f := newDispatchGuardFixture(t)
	blocked := legacyInstanceBlock(t, f)
	placementFixtureHeartbeat(t, f.nativeFixture, f.runner.binding.RunnerID, f.now.Add(-time.Hour))
	first := hubOwnedRecoveryClaim(f, "stale-runner")
	first.WorkItemID = blocked.WorkItemID
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, first)
	if response.Code == http.StatusOK {
		t.Fatal("runner with stale readiness claimed the item")
	}
	next := conversationWorkerRunner(t, f.nativeFixture)
	reportRunnerCapacity(t, f.service, f.base, next.redemption.Credential, next.binding.MachineID, next.redemption.DisplayName, next.redemption.BackendIsolation, next.redemption.Capacity)
	claim := hubOwnedRecoveryClaim(f, "ready-runner")
	claim.WorkItemID, claim.MachineID = blocked.WorkItemID, next.binding.MachineID
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", next.redemption.Credential, claim)
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.MachineID != next.binding.MachineID || f.reload(t).Revision != blocked.Revision+1 {
		t.Fatalf("healthy runner did not receive the recovered work: %+v", lease)
	}
}

func TestHubOwnedBlockedRecoveryLeavesNewFailureDispatchable(t *testing.T) {
	f := newDispatchGuardFixture(t)
	configureHubOwnedBlockedRecovery(t, f)
	f.completionBody = "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: Repair the runner checkout.\n```"
	f.finish(t, &tracker.NativeDisposition{Status: workpad.StatusBlocked, HumanAction: true, ReasonCode: "instance_limitation"})
	current := f.reload(t)
	path := f.base + "/work-items/" + string(current.WorkItemID)
	request := tracker.Transition{ExpectedRevision: current.Revision, State: "Blocked", Reason: "worker_progress", ReasonDetail: f.completionBody,
		Mutation: tracker.Mutation{IdempotencyKey: "machine-hold", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken},
	}
	for range 2 {
		response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/workflow", f.worker, request)
		requireNativeStatus(t, response, http.StatusOK)
		if after := f.reload(t); after.State != current.State || after.Revision != current.Revision {
			t.Fatalf("instance hold changed state=%q revision=%d", after.State, after.Revision)
		}
	}
	f.release(t)
	claim := hubOwnedRecoveryClaim(f, "retry-instance")
	claim.WorkItemID = current.WorkItemID
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim), http.StatusOK)
}
