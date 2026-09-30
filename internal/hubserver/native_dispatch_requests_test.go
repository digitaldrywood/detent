package hubserver

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// dispatchGuardFixture is the conversation worker fixture with the few
// helpers the claim guard needs: read the candidate set, finish an attempt,
// and move the item the ways a person or the orchestrator would.
type dispatchGuardFixture struct {
	*conversationWorkerFixture
}

func newDispatchGuardFixture(t *testing.T) dispatchGuardFixture {
	t.Helper()
	f := dispatchGuardFixture{conversationWorkerFixture: newConversationWorkerFixture(t)}
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
func (f dispatchGuardFixture) succeed(t *testing.T) {
	t.Helper()
	event := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1,
		Data: tracker.NativeRunData{
			Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
			LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: "succeeded",
		},
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
	f.release(t)
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

// TestClaimCandidatesKeepOfferingUnsuccessfulAttempts pins the other half of
// the guard: only a succeeded attempt suppresses anything, so a retry after a
// failure is untouched.
func TestClaimCandidatesKeepOfferingUnsuccessfulAttempts(t *testing.T) {
	t.Parallel()

	for _, outcome := range []string{"failed", "cancelled", "interrupted"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			f := newDispatchGuardFixture(t)
			event := tracker.NativeRunEvent{
				Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1,
				Data: tracker.NativeRunData{
					Sequence: 2, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"},
					LeaseID: f.lease.ID, FencingToken: f.lease.FencingToken, RunID: f.run, AttemptID: f.attempt, PolicyID: f.policy, Outcome: outcome,
				},
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(f.issue.WorkItemID)+"/events", f.worker, event), http.StatusOK)
			f.release(t)
			if !f.candidate(t) {
				t.Fatalf("an attempt that ended %s must still be retried", outcome)
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
