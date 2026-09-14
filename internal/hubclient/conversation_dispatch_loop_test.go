package hubclient

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// TestConversationDispatchLoopSettles is the September 2026 dogfood loop, end
// to end against a real hub, a real database, the production worker client and
// scheduler, and the production runner turn path: a chat is linked to an issue
// on a three-state project (Todo / In Progress / Done), one attempt succeeds,
// and the work stops there.
//
// The run it reproduces claimed one issue four times before it was moved to
// Done by hand (operations.md section 7). Two brakes have to hold together,
// and this asserts both against the same hub:
//
//   - the hub no longer offers an item whose latest attempt succeeded at the
//     item's current revision with nothing asking for another one, so the
//     claim poll that produced attempt two now returns nothing;
//   - the orchestrator's promotion lands in a lane this project has. The
//     configured review state is "Human Review", which a three-state project
//     does not have, so the target is resolved against the project's own
//     workflow -- read here through the production native connector -- and the
//     item reaches Done instead of staying In Progress.
func TestConversationDispatchLoopSettles(t *testing.T) {
	f := newConversationFixture(t)

	created := f.createConversation(t, f.owner, "", "loop-first", "Make the unit test pass.")
	id := created.Conversation.ID
	stream := f.stream(t, f.owner, id, 0)
	stream.await(t, "the coordinator reply", func(frames []sseFrame) bool {
		return conversationSettled(t, frames, "Make the unit test pass.")
	})

	var link wireLinkResult
	f.expect(t, f.owner, "POST", f.base+"/conversations/"+id+"/link", map[string]any{
		"key": "loop-link", "share_history": true,
		"issue": map[string]any{"title": "Make the unit test pass", "description": "Append the exclamation mark."},
	}, 200, &link)
	issueID := link.Issue.ID
	if link.Scheduling.Lane != "Todo" {
		t.Fatalf("linked issue lane = %q, want Todo", link.Scheduling.Lane)
	}

	// The orchestrator's admission moves the item into its active lane
	// before it dispatches, which is where the dogfood run's item sat.
	tracker := f.nativeConnector(t)
	if err := tracker.UpdateIssueState(t.Context(), issueID, "In Progress"); err != nil {
		t.Fatalf("move the item into its active lane: %v", err)
	}

	// One attempt, claimed and run through the production path.
	backend := &scriptedLiveBackend{}
	scheduler := f.newWorkerScheduler(t, "machine-dispatch-loop")
	candidates := f.dispatchCandidates(t, scheduler)
	if len(candidates) != 1 || candidates[0].ID != issueID {
		t.Fatalf("candidates = %#v, want the linked issue %s", candidates, issueID)
	}
	if _, err := scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	attempt := f.startAttemptInWorkspace(t, scheduler, backend, candidates[0], newDogfoodWorktree(t))
	waiting := f.awaitSnapshot(t, f.owner, id, "the attempt to ask", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionWaitingInput
	})
	question := waiting.Questions[len(waiting.Questions)-1]
	f.command(t, f.owner, id, conversation.Command{
		Key: "loop-answer", Kind: conversation.CommandAnswer, QuestionID: question.ID,
		Answers:  map[string][]string{"approach": {"Incremental"}},
		Expected: conversation.Expected{AttemptID: *waiting.Conversation.Execution.AttemptID},
	})
	f.command(t, f.owner, id, conversation.Command{Key: "loop-steer", Kind: conversation.CommandMessage, Text: "Keep it to one line."})
	result := awaitAttempt(t, attempt)
	if result.FinalState != runner.FinalStateCompleted {
		t.Fatalf("attempt final state = %q, want completed", result.FinalState)
	}
	if err := scheduler.ReleaseClaim(t.Context(), issueID, "completed"); err != nil {
		t.Fatal(err)
	}

	// Brake one. The item is still In Progress -- non-terminal, dispatchable
	// -- and the hub offers it to nobody, because its latest attempt
	// succeeded at the revision the item still carries and nothing asked for
	// another turn. This is the poll that used to produce attempt two.
	if again := f.dispatchCandidates(t, scheduler); len(again) != 0 {
		t.Fatalf("second claim poll = %#v, want no candidate: the item was already answered", again)
	}

	// Brake two, first half: readiness. This is where the seventh dogfood run
	// stopped. The project runs a command gate, which requires a pull
	// request, and the issue the orchestrator holds is the one it was handed
	// when the attempt was dispatched -- a hub-native item with no pull
	// request at all -- so the item was judged not ready, the promotion was
	// never attempted, and nothing was even logged.
	dispatched := candidates[0]
	commandGate := gate.Config{Kind: gate.KindCommand}
	if dispatched.PullRequest != nil {
		t.Fatalf("dispatched issue pull request = %#v, want none: a hub-native item has no pull request", dispatched.PullRequest)
	}
	if ready, missing := orchestrator.CompletedReviewReady(dispatched, commandGate); ready || missing != "pull_request" {
		t.Fatalf("readiness before hydration = (%t, %q), want (false, \"pull_request\")", ready, missing)
	}

	// The item is judged on the hub's own facts instead. The change review
	// surface is read now, because the change the attempt produced is newer
	// than the issue the orchestrator holds.
	hydrated, err := tracker.HydrateChangeReview(t.Context(), dispatched)
	if err != nil {
		t.Fatalf("HydrateChangeReview() error = %v", err)
	}
	review := hydrated.ChangeReview
	if review == nil {
		t.Fatal("hydrated issue carries no change review surface")
	}
	// This project has no GitHub connector, so no pull request can ever
	// mirror the change: the fact is stated, not inferred from the absence.
	if review.PullRequestsAvailable || review.Provider != "" {
		t.Fatalf("change review = %#v, want no pull request connector on a native project", *review)
	}
	// The attempt posted its diff before run.finished (decisions section
	// 18.5), recorded against the revision the item still carries.
	if !review.ChangeAtCurrentRevision || review.ChangeRevision != review.Revision {
		t.Fatalf("change review = %#v, want a change recorded at the item's current revision", *review)
	}
	if ready, missing := orchestrator.CompletedReviewReady(hydrated, commandGate); !ready {
		t.Fatalf("readiness after hydration = (%t, %q), want ready", ready, missing)
	}

	// Brake two, second half: the lane the orchestrator promotes into is
	// resolved against this project's own workflow rather than the configured
	// "Human Review" that it does not have.
	states, err := tracker.ListWorkflowStates(t.Context())
	if err != nil {
		t.Fatalf("ListWorkflowStates() error = %v", err)
	}
	target := orchestrator.CompletedReviewTarget(states, "Human Review")
	if target != "Done" {
		t.Fatalf("promotion target = %q, want Done on a Todo/In Progress/Done project", target)
	}
	if err := tracker.UpdateIssueState(t.Context(), issueID, target); err != nil {
		t.Fatalf("promote the completed item: %v", err)
	}

	promoted := f.workItem(t, issueID)
	if promoted.State != "Done" || !promoted.Terminal {
		t.Fatalf("promoted item = state %q terminal %t, want Done and terminal", promoted.State, promoted.Terminal)
	}

	// And with the item terminal, the lane brake holds on its own: nothing
	// depends on the revision guard any more.
	if again := f.dispatchCandidates(t, scheduler); len(again) != 0 {
		t.Fatalf("claim poll after promotion = %#v, want no candidate", again)
	}

	// The seventh run's whole scenario, in the numbers it recorded: one
	// attempt on the item, succeeded, and the item promoted out of its
	// active lane rather than left in it.
	attempts := f.attempts(t, issueID)
	if len(attempts) != 1 || attempts[0].Status != "succeeded" {
		t.Fatalf("attempts = %#v, want exactly one succeeded attempt", attempts)
	}
}

// attempts reads an item's attempts through the operator API.
func (f *conversationFixture) attempts(t *testing.T, id string) []tracker.NativeAttempt {
	t.Helper()
	var page tracker.Page[tracker.NativeAttempt]
	f.expect(t, f.owner, "GET", f.base+"/work-items/"+id+"/attempts", nil, 200, &page)
	return page.Items
}

// dispatchCandidates runs one production claim poll over every dispatchable
// lane of the project, which is the set the orchestrator asks for.
func (f *conversationFixture) dispatchCandidates(t *testing.T, scheduler *Scheduler) []connector.Issue {
	t.Helper()
	issues, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{
		Policy: f.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return issues
}

// nativeConnector builds the production native connector the orchestrator
// drives the tracker through.
func (f *conversationFixture) nativeConnector(t *testing.T) *NativeConnector {
	t.Helper()
	native, err := f.admin.Native(f.organization, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	tracker, err := NewNativeConnector(native)
	if err != nil {
		t.Fatal(err)
	}
	return tracker
}

// workItem reads one work item through the operator API.
func (f *conversationFixture) workItem(t *testing.T, id string) tracker.NativeIssue {
	t.Helper()
	var issue tracker.NativeIssue
	f.expect(t, f.owner, "GET", f.base+"/work-items/"+id, nil, 200, &issue)
	return issue
}

// TestConversationDispatchLoopSettlesWithAGitHubConnector is the eighth
// dogfood run's hub: the same loop on a project that has a GitHub repository
// bound and enabled, the way the browser preview fixture binds one. That run
// recorded one warn line and an item that never moved:
//
//	completed issue not ready for promotion ... missing=pull_request
//	change_review_provider=github pull_requests_available=true
//	change_revision=2 issue_revision=2
//
// The change covered the item as it stood; what the item did not have was a
// pull request, and nothing in a conversation-driven attempt opens one --
// opening one is the explicit POST {nativeBase}/work-items/:id/pull-requests/
// actions {action: open} of decisions section 18.6, executed by the merge
// lane. So the readiness rule judges a connector project on its own change
// exactly as it judges a connectorless one, and the item reaches Done.
func TestConversationDispatchLoopSettlesWithAGitHubConnector(t *testing.T) {
	f := newGitHubConnectorConversationFixture(t)

	created := f.createConversation(t, f.owner, "", "connector-first", "Make the unit test pass.")
	id := created.Conversation.ID
	stream := f.stream(t, f.owner, id, 0)
	stream.await(t, "the coordinator reply", func(frames []sseFrame) bool {
		return conversationSettled(t, frames, "Make the unit test pass.")
	})

	var link wireLinkResult
	f.expect(t, f.owner, "POST", f.base+"/conversations/"+id+"/link", map[string]any{
		"key": "connector-link", "share_history": true,
		"issue": map[string]any{"title": "Make the unit test pass", "description": "Append the exclamation mark."},
	}, 200, &link)
	issueID := link.Issue.ID

	tracker := f.nativeConnector(t)
	if err := tracker.UpdateIssueState(t.Context(), issueID, "In Progress"); err != nil {
		t.Fatalf("move the item into its active lane: %v", err)
	}

	backend := &scriptedLiveBackend{}
	scheduler := f.newWorkerScheduler(t, "machine-connector-loop")
	candidates := f.dispatchCandidates(t, scheduler)
	if len(candidates) != 1 || candidates[0].ID != issueID {
		t.Fatalf("candidates = %#v, want the linked issue %s", candidates, issueID)
	}
	if _, err := scheduler.AdoptClaim(t.Context(), candidates[0], time.Now()); err != nil {
		t.Fatal(err)
	}
	attempt := f.startAttemptInWorkspace(t, scheduler, backend, candidates[0], newDogfoodWorktree(t))
	waiting := f.awaitSnapshot(t, f.owner, id, "the attempt to ask", func(s wireSnapshot) bool {
		return s.Conversation.Execution.Status == conversation.ExecutionWaitingInput
	})
	question := waiting.Questions[len(waiting.Questions)-1]
	f.command(t, f.owner, id, conversation.Command{
		Key: "connector-answer", Kind: conversation.CommandAnswer, QuestionID: question.ID,
		Answers:  map[string][]string{"approach": {"Incremental"}},
		Expected: conversation.Expected{AttemptID: *waiting.Conversation.Execution.AttemptID},
	})
	f.command(t, f.owner, id, conversation.Command{Key: "connector-steer", Kind: conversation.CommandMessage, Text: "Keep it to one line."})
	if result := awaitAttempt(t, attempt); result.FinalState != runner.FinalStateCompleted {
		t.Fatalf("attempt final state = %q, want completed", result.FinalState)
	}
	if err := scheduler.ReleaseClaim(t.Context(), issueID, "completed"); err != nil {
		t.Fatal(err)
	}

	commandGate := gate.Config{Kind: gate.KindCommand}
	hydrated, err := tracker.HydrateChangeReview(t.Context(), candidates[0])
	if err != nil {
		t.Fatalf("HydrateChangeReview() error = %v", err)
	}
	review := hydrated.ChangeReview
	if review == nil {
		t.Fatal("hydrated issue carries no change review surface")
	}
	// The two facts the eighth run's warn line reported: a connector that
	// could mirror a change, and a change that covers the item as it stands.
	if !review.PullRequestsAvailable || review.Provider != "github" {
		t.Fatalf("change review = %#v, want the project's GitHub connector", *review)
	}
	if !review.ChangeAtCurrentRevision || review.ChangeRevision != review.Revision {
		t.Fatalf("change review = %#v, want a change recorded at the item's current revision", *review)
	}
	// And the fact that used to stop the promotion: no pull request was
	// opened, and nothing in this loop was ever going to open one.
	if hydrated.PullRequest != nil {
		t.Fatalf("pull request = %#v, want none: opening one is an explicit action", hydrated.PullRequest)
	}
	if ready, missing := orchestrator.CompletedReviewReady(hydrated, commandGate); !ready {
		t.Fatalf("readiness after hydration = (%t, %q), want ready on the item's own change", ready, missing)
	}

	states, err := tracker.ListWorkflowStates(t.Context())
	if err != nil {
		t.Fatalf("ListWorkflowStates() error = %v", err)
	}
	target := orchestrator.CompletedReviewTarget(states, "Human Review")
	if target != "Done" {
		t.Fatalf("promotion target = %q, want Done on a Todo/In Progress/Done project", target)
	}
	if err := tracker.UpdateIssueState(t.Context(), issueID, target); err != nil {
		t.Fatalf("promote the completed item: %v", err)
	}
	promoted := f.workItem(t, issueID)
	if promoted.State != "Done" || !promoted.Terminal {
		t.Fatalf("promoted item = state %q terminal %t, want Done and terminal", promoted.State, promoted.Terminal)
	}
	if again := f.dispatchCandidates(t, scheduler); len(again) != 0 {
		t.Fatalf("claim poll after promotion = %#v, want no candidate", again)
	}
}
