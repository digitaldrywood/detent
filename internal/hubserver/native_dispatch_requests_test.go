package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

// dispatchGuardFixture is the conversation worker fixture with the few
// helpers the claim guard needs: read the candidate set, finish an attempt,
// and move the item the ways a person or the orchestrator would.
type dispatchGuardFixture struct {
	*conversationWorkerFixture
	worktreeState  string
	completionBody string
	checkpoint     *tracker.NativeCheckpoint
}

func newDispatchGuardFixture(t *testing.T) dispatchGuardFixture {
	t.Helper()
	f := dispatchGuardFixture{conversationWorkerFixture: newConversationWorkerFixture(t), worktreeState: "clean"}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	return f
}

func (f dispatchGuardFixture) scope() nativeScope {
	return nativeScope{
		organization: f.project.OrganizationID,
		project:      f.project.ID,
		credential:   apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin},
	}
}

// candidate reports whether the claim query still offers the fixture's item.
func (f dispatchGuardFixture) candidate(t *testing.T) bool {
	t.Helper()
	scope := f.scope()
	tx := f.tx(t)
	defer tx.Rollback()
	ids, err := claimCandidateIDs(t.Context(), tx, claimCandidateQuery{NativeScope: &scope, Scope: string(f.project.ID)}, nil, nil, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("claimCandidateIDs() error = %v", err)
	}
	var id tracker.WorkItemID
	if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?", f.project.OrganizationID, f.project.ID, f.issue.WorkItemID).Scan(&id); err != nil {
		t.Fatalf("read issue row id: %v", err)
	}
	return slices.Contains(ids, id)
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
	event := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{
			Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
			LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded",
		},
	}
	if len(disposition) > 0 {
		event.Data.Disposition = disposition[0]
	}
	event.Data.CompletionBody = f.completionBody
	checkpoint := event
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Outcome = "run.checkpointed", newNativeID("checkpoint"), ""
	checkpoint.Data.Disposition = nil
	checkpoint.Data.CompletionBody = ""
	checkpoint.Data.Handoff = nativeTestCheckpoint()
	checkpoint.Data.Handoff.WorktreeState = f.worktreeState
	if f.checkpoint != nil {
		checkpoint.Data.Handoff = f.checkpoint
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, checkpoint), http.StatusOK)
	event.Data.Sequence++
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
	ctx := t.Context()
	tx := f.tx(t)
	record, err := f.chat.store.readConversation(ctx, tx, f.record.OrganizationID, f.record.ProjectID, f.record.ID)
	if err != nil {
		t.Fatalf("readConversation() error = %v", err)
	}
	command := conversation.Command{
		Key: key, Kind: conversation.CommandContinue, Text: "Continue with the release notes.",
		Expected: conversation.Expected{AttemptID: record.Execution.Owner.AttemptID},
	}
	if _, _, err := f.chat.store.reserveCommand(ctx, tx, record.ID, key, command.Kind, "hash-"+key, f.service.config.now()); err != nil {
		t.Fatalf("reserveCommand() error = %v", err)
	}
	if _, err := f.chat.acceptCommand(ctx, tx, f.scope(), &record, command, f.service.config.now()); err != nil {
		t.Fatalf("acceptCommand(continue) error = %v", err)
	}
	if err := f.chat.saveConversation(ctx, tx, &record, f.service.config.now()); err != nil {
		t.Fatalf("saveConversation() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestClaimCandidatesRequireUnansweredWorkItem is operations.md section 7. An
// item whose latest attempt succeeded at the item's current revision is not
// offered again on its own; each of the three things that genuinely mean "run
// it again" brings it back.
func TestClaimCandidatesRequireUnansweredWorkItem(t *testing.T) {
	t.Parallel()
	instanceReport := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:worker-loopback\n    reason: sandbox refused listener with EPERM\nhuman_action: null\n```"
	definitionReport := "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: instance:tool\n    reason: approved portable project configuration and workflow prose unavailable\nhuman_action: null\n```"
	completeReport := "Source ready for finalization.\n\n```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
	for _, test := range []struct {
		name           string
		disposition    *tracker.NativeDisposition
		finalMessage   string
		worktreeState  string
		manualHold     bool
		edited         bool
		continued      bool
		commented      bool
		wantCandidate  bool
		accepted       bool
		legacy         bool
		retainedSource bool
		checkpointEdit func(*tracker.NativeCheckpoint)
		otherVersion   bool
		newerChange    bool
		expiredLease   bool
		reviewOutcome  string
		priorFindings  bool
		publication    string
		terminal       bool
		readAdmission  bool
		moveRefusal    bool
		moveGuidance   bool
		exclusion      string
		fullHost       bool
		aged           bool
	}{
		{name: "continued instance cooldown exposes full host", finalMessage: instanceReport, continued: true, wantCandidate: true, readAdmission: true, fullHost: true},
		{name: "expired cooldown exposes full host", aged: true, wantCandidate: true, readAdmission: true, fullHost: true},
		{name: "completed instance report is answered", finalMessage: instanceReport, readAdmission: true, exclusion: "instance_cooldown"},
		{name: "completed definition blocker is answered with clean source", finalMessage: definitionReport},
		{name: "completed definition blocker is answered with dirty source", finalMessage: definitionReport, worktreeState: "dirty"},
		{name: "ordinary comment does not resume dirty definition blocker", finalMessage: definitionReport, worktreeState: "dirty", commented: true},
		{name: "instance report remains runnable after revision edit", finalMessage: instanceReport, edited: true, wantCandidate: true, readAdmission: true},
		{name: "dirty definition blocker resumes after revision edit", finalMessage: definitionReport, worktreeState: "dirty", edited: true, wantCandidate: true},
		{name: "definition blocker resumes after continuation", finalMessage: definitionReport, continued: true, wantCandidate: true},
		{name: "dirty definition blocker resumes after continuation", finalMessage: definitionReport, worktreeState: "dirty", continued: true, wantCandidate: true},
		{name: "recorded prerequisite survives revision edit without relation", finalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - ref: '#42'\n    reason: prerequisite must finish\n    owner: orchestrator\n    predicate:\n      type: issue_state\n      states: [open]\nhuman_action: null\n```", edited: true},
		{name: "instance report respects manual workflow hold", finalMessage: instanceReport, manualHold: true},
		{name: "instance and human action remain held", finalMessage: strings.Replace(instanceReport, "human_action: null", "human_action: Approve the exception", 1)},
		{name: "instance and external blocker remain held", finalMessage: strings.Replace(instanceReport, "human_action: null", "  - ref: '#42'\n    reason: Await dependency\nhuman_action: null", 1)},
		{name: "reason only human blocker remains held", finalMessage: strings.Replace(instanceReport, "  - ref: instance:worker-loopback\n", "", 1)},
		{name: "instance and reason code remain held", finalMessage: strings.Replace(instanceReport, "status: blocked", "status: blocked\nreason_code: permission_wait", 1)},
		{name: "native273 malformed predicate remains held", finalMessage: strings.Replace(instanceReport, "    reason:", "    predicate: instance_available\n    reason:", 1)},
		{name: "unfinished custom Rework remains runnable", disposition: &tracker.NativeDisposition{Status: "in_progress"}, wantCandidate: true},
		{name: "unfinished dirty source remains runnable", disposition: &tracker.NativeDisposition{Status: "in_progress"}, worktreeState: "dirty", wantCandidate: true},
		{name: "unaccepted completed custom Rework remains claimable", disposition: &tracker.NativeDisposition{Status: "complete"}, wantCandidate: true},
		{name: "accepted completion remains answered", disposition: &tracker.NativeDisposition{Status: "complete"}, accepted: true},
		{name: "unaccepted completed dirty source remains claimable", disposition: &tracker.NativeDisposition{Status: "complete"}, worktreeState: "dirty", wantCandidate: true},
		{name: "explicit blocker keeps the answer", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, readAdmission: true, exclusion: "external_blocker", moveRefusal: true, moveGuidance: true},
		{name: "nondispatchable blocker move retains ordinary refusal", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, manualHold: true, readAdmission: true, moveRefusal: true},
		{name: "terminal blocker move retains ordinary refusal", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, terminal: true, readAdmission: true, moveRefusal: true},
		{name: "dirty external blocker keeps the answer", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, worktreeState: "dirty"},
		{name: "unfinished external blocker keeps the answer", disposition: &tracker.NativeDisposition{Status: "in_progress", Blockers: true}},
		{name: "unfinished human action keeps the answer", disposition: &tracker.NativeDisposition{Status: "in_progress", HumanAction: true}},
		{name: "missing legacy disposition keeps the answer", readAdmission: true, exclusion: "untyped_cooldown"},
		{name: "aged attempt without disposition or report retries", aged: true, wantCandidate: true, readAdmission: true},
		{name: "uncertain push without a disposition is reconciled by any runner", retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "git_push", "pending", newNativeID("effect")
		}},
		{name: "legacy complete unpublished retained source is runnable", finalMessage: completeReport, legacy: true, retainedSource: true, wantCandidate: true},
		{name: "typed complete unpublished retained source is runnable", finalMessage: completeReport, retainedSource: true, wantCandidate: true},
		{name: "typed complete without legacy body is runnable", disposition: &tracker.NativeDisposition{Status: "complete"}, retainedSource: true, wantCandidate: true},
		{name: "legacy complete dirty retained source is runnable", finalMessage: completeReport, legacy: true, retainedSource: true, worktreeState: "dirty", wantCandidate: true},
		{name: "legacy retained source without report stays answered", legacy: true, retainedSource: true},
		{name: "aged legacy retained source without report retries", legacy: true, retainedSource: true, aged: true, wantCandidate: true},
		{name: "legacy instance report with retained source stays answered", finalMessage: instanceReport, legacy: true, retainedSource: true},
		{name: "legacy definition blocker with retained source stays answered", finalMessage: definitionReport, legacy: true, retainedSource: true},
		{name: "legacy human action with retained source stays answered", finalMessage: strings.Replace(completeReport, "human_action: null", "human_action: Approve the exception", 1), legacy: true, retainedSource: true},
		{name: "legacy external blocker with retained source stays answered", finalMessage: strings.Replace(completeReport, "blockers: []", "blockers:\n  - ref: '#42'\n    reason: Await dependency", 1), legacy: true, retainedSource: true},
		{name: "legacy malformed report with retained source stays answered", finalMessage: strings.Replace(completeReport, "schema: 1", "schema: 2", 1), legacy: true, retainedSource: true},
		{name: "legacy complete with invalid reason stays answered", finalMessage: strings.Replace(completeReport, "status: complete", "status: complete\nreason_code: permission_wait", 1), legacy: true, retainedSource: true},
		{name: "typed blocker overrides legacy complete", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, finalMessage: completeReport, legacy: true, retainedSource: true},
		{name: "typed human action overrides legacy complete", disposition: &tracker.NativeDisposition{Status: "complete", HumanAction: true}, finalMessage: completeReport, legacy: true, retainedSource: true},
		{name: "typed instance reason overrides legacy complete", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "instance_limitation"}, finalMessage: completeReport, legacy: true, retainedSource: true},
		{name: "legacy in progress retained source stays answered", finalMessage: strings.Replace(completeReport, "status: complete", "status: in_progress", 1), legacy: true, retainedSource: true},
		{name: "legacy complete with missing head stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) { c.HeadSHA = "" }},
		{name: "legacy complete with missing digest stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) { c.WorkspaceDigest = "" }},
		{name: "legacy complete with unavailable source stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) { c.Availability = "missing" }},
		{name: "legacy complete requiring manual recovery stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) { c.Resume = "manual_recovery" }},
		{name: "pending publication is reconciled by any runner", finalMessage: completeReport, legacy: true, retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "git_push", "pending", newNativeID("effect")
		}},
		{name: "ambiguous publication is reconciled by any runner", finalMessage: completeReport, legacy: true, retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "pr_create", "ambiguous", newNativeID("effect")
		}},
		{name: "typed complete published current version waits for review", reviewOutcome: "approved", finalMessage: completeReport, retainedSource: true, publication: "owned", readAdmission: true, exclusion: "automated_review_missing"},
		{name: "same head cannot borrow an unrelated version", otherVersion: true, finalMessage: completeReport, retainedSource: true, publication: "owned", wantCandidate: true},
		{name: "newer unrelated draft does not hide published review wait", newerChange: true, reviewOutcome: "approved", finalMessage: completeReport, retainedSource: true, publication: "owned", readAdmission: true, exclusion: "automated_review_missing"},
		{name: "expired review wait leaves promotion to orchestrator", expiredLease: true, reviewOutcome: "approved", finalMessage: completeReport, retainedSource: true, publication: "owned", exclusion: "automated_review_missing"},
		{name: "published version resumes implementation for actionable findings", finalMessage: completeReport, retainedSource: true, publication: "owned", reviewOutcome: "changes_requested"},
		{name: "published version retries implementation after failed validation", finalMessage: completeReport, retainedSource: true, publication: "owned", reviewOutcome: "failed_check", wantCandidate: true},
		{name: "completed rework does not repeat existing findings on the same head", finalMessage: completeReport, retainedSource: true, publication: "owned", reviewOutcome: "changes_requested", priorFindings: true, readAdmission: true, exclusion: "validator_rework"},
		{name: "completed rework does not repeat an existing failed check", finalMessage: completeReport, retainedSource: true, publication: "owned", reviewOutcome: "failed_check", priorFindings: true, readAdmission: true, exclusion: "validator_rework"},
		{name: "published version review preserves active claim owner", finalMessage: completeReport, retainedSource: true, publication: "owned", reviewOutcome: "active"},
		{name: "aged published current version still waits for review", finalMessage: completeReport, retainedSource: true, publication: "owned", aged: true, readAdmission: true, exclusion: "automated_review_missing"},
		{name: "legacy complete with genuine publication stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "owned"},
		{name: "legacy complete with published retained head stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "head"},
		{name: "unrelated historical publication preserves retained source", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "unrelated", wantCandidate: true},
		{name: "earlier published head from same attempt preserves retained source", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "earlier", wantCandidate: true},
		{name: "aged instance blocker report retries", finalMessage: instanceReport, aged: true, wantCandidate: true, readAdmission: true},
		{name: "aged validation failure retries", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "validation_failed"}, aged: true, wantCandidate: true},
		{name: "aged moved head retries", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "head_moved"}, aged: true, wantCandidate: true},
		{name: "confirmed push without a disposition continues", retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "git_push", "confirmed", newNativeID("effect")
		}},
		{name: "confirmed pull request without a disposition continues", retainedSource: true, wantCandidate: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "pr_create", "confirmed", newNativeID("effect")
		}},
		{name: "aged human action stays held", disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}, aged: true, readAdmission: true, exclusion: "human_action"},
		{name: "aged permission wait stays held", disposition: &tracker.NativeDisposition{Status: "blocked", ReasonCode: "permission_wait"}, aged: true, readAdmission: true, exclusion: "permission_wait"},
		{name: "aged external blocker stays held", finalMessage: strings.Replace(instanceReport, "human_action: null", "  - ref: '#42'\n    reason: Await dependency\nhuman_action: null", 1), aged: true},
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
			_, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,?,?,0,?,?,?)`, f.project.ID, "Fixing", "Fixing", !test.manualHold, formatHubTime(f.now), formatHubTime(f.now))
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Fixing') WHERE native_id=?`, f.project.ID, f.issue.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			if test.moveRefusal {
				states := append(append([]tracker.NativeState(nil), f.project.States...), tracker.NativeState{Name: "Fixing", Dispatchable: !test.manualHold, Terminal: test.terminal, Transitions: []string{"Blocked"}})
				raw, err := marshalNative(states)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", raw, f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			revision := f.reload(t).Revision
			f.finish(t, test.disposition)
			if test.accepted {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = json_set(data_json, '$.runtime.completion.acceptance_recorded', json('true')) WHERE id = ?`, f.attempt); err != nil {
					t.Fatal(err)
				}
			}
			if test.aged {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET updated_at = ? WHERE id = ?`, formatHubTime(f.now.Add(-2*time.Hour)), f.attempt); err != nil {
					t.Fatal(err)
				}
			}
			var published tracker.ChangeVersion
			var changePath string
			if test.reviewOutcome != "" {
				states := append(append([]tracker.NativeState(nil), f.project.States...), tracker.NativeState{Name: "Fixing", Dispatchable: true, Transitions: []string{"Merging", "Blocked"}}, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: []string{"Done"}})
				raw, err := marshalNative(states)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", raw, f.project.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT OR IGNORE INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,?,?,0,1,?,?)", f.project.ID, "Merging", "Merging", formatHubTime(f.now), formatHubTime(f.now)); err != nil {
					t.Fatal(err)
				}
			}
			if test.publication != "" {
				rules := tracker.ChangeReviewPolicy{PolicyID: f.policy, RequireReview: true}
				if test.reviewOutcome != "" {
					var principal string
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM api_tokens WHERE name = 'operator-conversation-worker'").Scan(&principal); err != nil {
						t.Fatal(err)
					}
					rules.RequiredChecks = []tracker.ChangeCheckSpec{{Name: "test", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: policy.Digest([]byte("trusted CI")), Source: "independent", MaxAgeSeconds: 3600}}
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rules"}, Policy: rules}), http.StatusOK)
				path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "/changes"
				response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.worker, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, Title: "Retained source", Body: "Owned source publication"})
				requireNativeStatus(t, response, http.StatusOK)
				var change tracker.ChangeRequest
				decodeHubResponse(t, response, &change)
				input := changeTestInput()
				if test.publication == "owned" || test.publication == "earlier" {
					input.RunID, input.AttemptID = f.run, f.attempt
				}
				if test.publication == "unrelated" || test.publication == "earlier" {
					input.HeadSHA = strings.Repeat("c", 40)
				}
				response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ChangeVersionInput: input})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &published)
				changePath = path + "/" + change.ID + "/versions/" + published.ID
				if test.publication == "owned" || test.publication == "earlier" {
					finalization := tracker.NativeFinalization{Settled: true, Changed: true, ChangeID: change.ID, VersionID: published.ID, HeadSHA: published.HeadSHA}
					if test.otherVersion {
						finalization.VersionID = "version_other"
					}
					raw, err := json.Marshal(finalization)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json, '$.finalization', json(?)) WHERE id=?", string(raw), f.attempt); err != nil {
						t.Fatal(err)
					}
				}
				if test.newerChange {
					response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.worker, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "unrelated-draft", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, Title: "Unrelated draft", Body: "No published source"})
					requireNativeStatus(t, response, http.StatusOK)
					var draft tracker.ChangeRequest
					decodeHubResponse(t, response, &draft)
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_requests SET record_json=json_set(record_json, '$.created_at', ?) WHERE id=?", formatHubTime(f.now.Add(time.Hour)), draft.ID); err != nil {
						t.Fatal(err)
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
			if test.priorFindings {
				if test.reviewOutcome == "changes_requested" {
					request := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "existing-findings"}, Decision: "changes_requested", Body: "Resolve the existing findings"}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/reviews", f.token, request), http.StatusOK)
				}
				f.advance(time.Second)
				started := f.now
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET started_at=?, updated_at=? WHERE id=?", formatHubTime(started), formatHubTime(started.Add(time.Nanosecond)), f.attempt); err != nil {
					t.Fatal(err)
				}
				f.advance(time.Second)
			}
			if test.terminal {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE workflow_states SET terminal=1 WHERE project_id=? AND detent_state='Fixing'", f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/attempts/"+f.attempt, f.worker, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var attempt tracker.NativeAttempt
			decodeHubResponse(t, response, &attempt)
			if attempt.Status != "succeeded" || attempt.Outcome != "succeeded" || attempt.CompletionBody != test.finalMessage || attempt.Checkpoint == nil || attempt.Checkpoint.WorktreeState != f.worktreeState || attempt.WorkItemRevision != revision || attempt.DispatchGeneration != 0 || test.legacy && test.disposition == nil && attempt.Disposition != nil {
				t.Fatalf("completion lost successful current-revision evidence: %#v", attempt)
			}
			claim := tracker.NativeClaim{PolicyID: f.policy, WorkItemID: f.issue.WorkItemID, MachineID: "conversation-machine", SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.worker, claim), http.StatusConflict)
			if test.expiredLease {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE leases SET expires_at=? WHERE lease_id=?", formatHubTime(f.now.Add(-time.Minute)), f.lease.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.release(t)
			}
			if f.reload(t).Revision != revision {
				t.Fatal("completion fabricated an item edit")
			}
			if (test.edited || test.continued) && f.candidate(t) {
				t.Fatal("completed blocker was offered before fresh context")
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
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT dispatch_generation FROM issues WHERE native_id = ?", f.issue.WorkItemID).Scan(&generation); err != nil || generation != wantGeneration {
				t.Fatalf("dispatch generation=%d, want %d, error=%v", generation, wantGeneration, err)
			}
			if got := f.candidate(t); got != test.wantCandidate {
				t.Fatalf("candidate=%t, want %t", got, test.wantCandidate)
			}
			if test.readAdmission {
				r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				r.enroll(t)
				context := tracker.NativeAdmissionContext{ObservedAt: f.now, PolicyID: f.policy, WorkflowStates: []string{"Fixing"}}
				payload := map[string]any{"display_name": "Runner", "capacity": 2, "version": "test", "backend_isolation": r.redemption.BackendIsolation, "admission": map[string]any{"context": context, "runner_revision": 1}}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, payload), http.StatusOK)
				if test.fullHost {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE machines SET capacity=0 WHERE id=?", r.binding.MachineID); err != nil {
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
							want := operatortool.ErrAccessDenied.Error()
							var result struct {
								Content []struct{ Text string } `json:"content"`
							}
							if err := json.Unmarshal(reply.Result, &result); err != nil || len(result.Content) != 1 || result.Content[0].Text != want || len(raw) != 0 {
								t.Fatalf("%s refusal changed: %s %s", move.name, reply.Result, reply.Error)
							}
							continue
						}
						var refusal operatortool.RequestError
						if !test.moveGuidance {
							if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Code != "transition_not_allowed" || refusal.Message != "Workflow transition is not allowed" {
								t.Fatalf("%s refusal changed: %s %s", move.name, reply.Result, reply.Error)
							}
							continue
						}
						if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Code != "invalid_request" {
							t.Fatalf("%s refusal=%s result=%s rpc_error=%s err=%v", move.name, raw, reply.Result, reply.Error, err)
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
			if test.wantCandidate {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", f.nativeFixture.worker(t, "other-worker"), claim), http.StatusNotFound)
				f.claim(t)
				if f.lease.FencingToken <= attempt.FencingToken {
					t.Fatal("reclaim did not grant newer fenced authority")
				}
				if test.edited || test.continued {
					f.succeed(t, test.disposition)
					if f.candidate(t) {
						t.Fatal("fresh context's completed blocker was offered again")
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
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/reviews", f.token, request), http.StatusOK)
				want := "Fixing"
				if current := f.reload(t); current.State != want {
					t.Fatalf("review result lane=%s want=%s", current.State, want)
				}
				if got := f.candidate(t); got != (decision == "changes_requested" || test.reviewOutcome == "failed_check" && !test.priorFindings) {
					t.Fatalf("review result candidate=%t lane=%s", got, want)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/reviews", f.token, request), http.StatusOK)
				if current := f.reload(t); current.State != want {
					t.Fatal("review replay changed lane")
				}
				if test.priorFindings && test.reviewOutcome == "failed_check" {
					check := changeTestResult(published)
					check.IdempotencyKey = "new-failed-check"
					check.Conclusion = "failure"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/checks", f.token, check), http.StatusOK)
					if f.candidate(t) {
						t.Fatal("replaying an existing failed check admitted unchanged rework")
					}
					input := published.ChangeVersionInput
					input.BaseSHA, input.MergeBaseSHA = strings.Repeat("c", 40), strings.Repeat("c", 40)
					path := strings.TrimSuffix(changePath, "/versions/"+published.ID) + "/versions"
					request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "moved-base"}, ExpectedVersionID: published.ID, ChangeVersionInput: input}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request), http.StatusOK)
					if !f.candidate(t) {
						t.Fatal("a moved source base did not admit rework")
					}
				}
			}
		})
	}
	f := newDispatchGuardFixture(t)

	if !f.candidate(t) {
		t.Fatal("a running attempt must not remove the item from the candidate query; the lease does that")
	}

	f.succeed(t)
	if f.candidate(t) {
		t.Fatal("a succeeded attempt on an unchanged item is still offered: this is the re-dispatch loop")
	}

	// A conversation continuation bumps no revision and moves no lane, so it
	// has to be written down as an explicit request (decisions section 10).
	f.continueConversation(t, "continue-1")
	if !f.candidate(t) {
		t.Fatal("a continuation must re-offer the item")
	}

	f.claim(t)
	f.succeed(t)
	if f.candidate(t) {
		t.Fatal("the continuation's own attempt must not re-offer the item again")
	}

	// An edit moves the item on, so the succeeded attempt answered an older
	// version of it.
	issue := f.reload(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(f.issue.WorkItemID), f.token, map[string]any{
		"idempotency_key": newNativeID("edit"), "expected_revision": fmt.Sprint(issue.Revision), "title": "Edited after the attempt",
	}), http.StatusOK)
	if !f.candidate(t) {
		t.Fatal("an edit must re-offer the item")
	}

	f.claim(t)
	f.succeed(t)
	if f.candidate(t) {
		t.Fatal("the edit's own attempt must not re-offer the item again")
	}

	// A person moving the item back into a dispatchable lane is a workflow
	// transition, which is also an edit of the item.
	issue = f.reload(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/workflow", f.token, map[string]any{
		"idempotency_key": newNativeID("move"), "expected_revision": fmt.Sprint(issue.Revision), "state": "In Progress", "reason": "user_requested",
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
		{name: "successful dirty source without disposition", outcome: "succeeded", checkpoint: "dirty"},
		{name: "successful finalized unpushed source", outcome: "succeeded", checkpoint: "unpushed"},
		{name: "successful clean source", outcome: "succeeded", checkpoint: "clean"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newDispatchGuardFixture(t)
			event := tracker.NativeRunEvent{
				Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{
					Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
					LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: test.outcome,
				},
			}
			if test.checkpoint != "" {
				checkpoint := event
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Outcome = "run.checkpointed", newNativeID("checkpoint"), ""
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				checkpoint.Data.Handoff.WorktreeState = test.checkpoint
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, checkpoint), http.StatusOK)
				event.Data.Sequence++
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
			event.IdempotencyKey = newNativeID("replay-finish")
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
		if err := f.service.database.db.QueryRowContext(t.Context(),
			"SELECT work_item_revision, dispatch_generation FROM native_attempts WHERE id = ?", f.attempt,
		).Scan(&revision, &generation); err != nil {
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

// TestClaimCandidatesIgnoreAnsweredOnProjectionProfile pins the guard to the
// native profile. On a github_compatible project the issues row is a
// projection of GitHub, and the webhook and reconcile writes do not touch
// revision, because revision fences native edits. Suppressing a projected item
// on a revision GitHub never advances would strand it forever, which is the
// worse failure.
func TestClaimCandidatesIgnoreAnsweredOnProjectionProfile(t *testing.T) {
	t.Parallel()
	f := newDispatchGuardFixture(t)
	f.succeed(t)
	if f.candidate(t) {
		t.Fatal("the native guard must suppress the answered item")
	}

	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET profile = 'github_compatible' WHERE id = ?", f.project.ID); err != nil {
		t.Fatalf("switch the project to the projection profile: %v", err)
	}
	tx := f.tx(t)
	defer tx.Rollback()
	ids, err := claimCandidateIDs(t.Context(), tx, claimCandidateQuery{}, nil, nil, nil, nil, nil, nil, nil, map[tracker.RepositoryID]struct{}{0: {}})
	if err != nil {
		t.Fatalf("claimCandidateIDs() error = %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("a projected item must stay claimable: its revision is not the tracker's")
	}
}
