package hubserver

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
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
		legacy         bool
		retainedSource bool
		checkpointEdit func(*tracker.NativeCheckpoint)
		publication    string
		terminal       bool
	}{
		{name: "completed instance report is answered", finalMessage: instanceReport},
		{name: "completed definition blocker is answered with clean source", finalMessage: definitionReport},
		{name: "completed definition blocker is answered with dirty source", finalMessage: definitionReport, worktreeState: "dirty"},
		{name: "ordinary comment does not resume dirty definition blocker", finalMessage: definitionReport, worktreeState: "dirty", commented: true},
		{name: "instance report remains runnable after revision edit", finalMessage: instanceReport, edited: true, wantCandidate: true},
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
		{name: "completed custom Rework is answered", disposition: &tracker.NativeDisposition{Status: "complete"}},
		{name: "completed dirty source is answered", disposition: &tracker.NativeDisposition{Status: "complete"}, worktreeState: "dirty"},
		{name: "explicit blocker keeps the answer", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}},
		{name: "dirty external blocker keeps the answer", disposition: &tracker.NativeDisposition{Status: "blocked", Blockers: true}, worktreeState: "dirty"},
		{name: "unfinished external blocker keeps the answer", disposition: &tracker.NativeDisposition{Status: "in_progress", Blockers: true}},
		{name: "unfinished human action keeps the answer", disposition: &tracker.NativeDisposition{Status: "in_progress", HumanAction: true}},
		{name: "missing legacy disposition keeps the answer"},
		{name: "legacy complete unpublished retained source is runnable", finalMessage: completeReport, legacy: true, retainedSource: true, wantCandidate: true},
		{name: "typed complete unpublished retained source is runnable", finalMessage: completeReport, retainedSource: true, wantCandidate: true},
		{name: "typed complete without legacy body is runnable", disposition: &tracker.NativeDisposition{Status: "complete"}, retainedSource: true, wantCandidate: true},
		{name: "legacy complete dirty retained source is runnable", finalMessage: completeReport, legacy: true, retainedSource: true, worktreeState: "dirty", wantCandidate: true},
		{name: "legacy retained source without report stays answered", legacy: true, retainedSource: true},
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
		{name: "legacy complete with pending publication stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "git_push", "pending", newNativeID("effect")
		}},
		{name: "legacy complete with ambiguous publication stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, checkpointEdit: func(c *tracker.NativeCheckpoint) {
			c.ExternalEffect, c.EffectState, c.EffectID = "pr_create", "ambiguous", newNativeID("effect")
		}},
		{name: "legacy complete with genuine publication stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "owned"},
		{name: "legacy complete with published retained head stays answered", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "head"},
		{name: "unrelated historical publication preserves retained source", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "unrelated", wantCandidate: true},
		{name: "earlier published head from same attempt preserves retained source", finalMessage: completeReport, legacy: true, retainedSource: true, publication: "earlier", wantCandidate: true},
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
			revision := f.reload(t).Revision
			f.finish(t, test.disposition)
			if test.publication != "" {
				rules := tracker.ChangeReviewPolicy{PolicyID: f.policy, RequireReview: true}
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
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/"+change.ID+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "version", LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken}, ChangeVersionInput: input}), http.StatusOK)
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
			f.release(t)
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
