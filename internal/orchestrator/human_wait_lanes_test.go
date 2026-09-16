package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestHumanWaitLaneReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, lane, want  string
		question, blocker bool
	}{
		{"2159", "Rework", "Human Review", true, false},
		{"2160", "Todo", "Human Review", true, false},
		{"2007", "Rework", "Blocked", false, true},
		{"2129", "Human Review", "Human Review", false, false},
		{"in progress question", "In Progress", "Human Review", true, false},
		{"merging question", "Merging", "Human Review", true, false},
		{"todo blocker", "Todo", "Blocked", false, true},
		{"merging blocker", "Merging", "Blocked", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openWorkAttemptRecoveryStore(t, t.Context())
			issue := recoveryTestIssue()
			issue.State = tc.lane
			if tc.blocker {
				issue.WorkpadSignal = &workpad.Signal{Status: workpad.StatusBlocked, HumanAction: "Check the hardware", Blockers: []workpad.Blocker{{Owner: workpad.BlockerOwnerHuman, Reason: "physical check"}}}
			}
			tracker := &questionTracker{Connector: memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})}
			o := newWorkAttemptRecoveryOrchestratorWithConnector(t, db, nil, tracker)
			now := time.Now().UTC()
			if tc.question {
				q := store.HumanQuestion{ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, Key: "disposition", Body: "Where should this defect go?", QuestionCommentID: "question"}
				questions := db.(store.HumanQuestionStore)
				if _, err := questions.ReserveHumanQuestion(t.Context(), q); err != nil {
					t.Fatal(err)
				}
				if err := questions.RecordHumanQuestionComment(t.Context(), q); err != nil {
					t.Fatal(err)
				}
				at := now.Add(-10 * time.Hour)
				tracker.comments = []connector.IssueComment{{ID: "question", Body: q.Body, CreatedAt: &at}}
			}
			state := newState(o.cfg)
			changed := o.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now)
			got := tc.lane
			for _, event := range tracker.Events() {
				if event.Kind == memory.EventKindStateUpdate {
					got = event.State
				}
			}
			if got != tc.want {
				t.Fatalf("lane = %s, want %s (transitions %v)", got, tc.want, changed)
			}
			if !tc.question && !tc.blocker {
				return
			}
			blocked := state.Blocked[issue.ID]
			if protectedRecoveryPark(*blocked.Recovery) {
				t.Fatal("ordinary human wait became a protected failure park")
			}
			snapshot := state.Snapshot(now)
			if len(snapshot.Blocked) != 1 || snapshot.Blocked[0].Error != blocked.Reason || snapshot.Blocked[0].State != tc.want {
				t.Fatalf("board wait = %+v", snapshot.Blocked)
			}
			if blocked.Reason == "" || (tc.question && !strings.Contains(blocked.Reason, "10h0m0s")) {
				t.Fatalf("missing reason/age: %+v", blocked)
			}
			current, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(current) != 1 {
				t.Fatalf("fetch: %v, %v", current, err)
			}
			issue = current[0]
			// Reconstruct the orchestrator to prove the return target is durable.
			o = newWorkAttemptRecoveryOrchestratorWithConnector(t, db, nil, tracker)
			state = newState(o.cfg)
			if tc.blocker {
				tracker.comments = []connector.IssueComment{{ID: "workpad", Body: "## Codex Workpad\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - owner: human\n    reason: physical check\nhuman_action: Check the hardware\n```"}}
			}
			if changed := o.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(time.Minute)); len(changed) != 0 {
				t.Fatalf("unanswered wait returned: %v", changed)
			}
			if tc.question {
				at := now.Add(time.Minute)
				tracker.comments = append(tracker.comments, connector.IssueComment{ID: "answer", Body: "Route it to Backlog", CreatedAt: &at, AuthorAuthorized: true})
			} else {
				tracker.comments = []connector.IssueComment{{ID: "workpad", Body: "## Codex Workpad\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"}}
			}
			if changed := o.recoverBlockedIssues(t.Context(), &state, []connector.Issue{issue}, now.Add(2*time.Minute)); len(changed) != 1 {
				t.Fatalf("cleared wait did not return: %v", changed)
			}
			current, err = tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || current[0].State != tc.lane {
				t.Fatalf("return lane: %v, %v", current, err)
			}
			if _, held := state.Blocked[issue.ID]; held {
				t.Fatal("cleared wait still holds dispatch")
			}

		})
	}
}

type humanWaitWriteFailureTracker struct{ *questionTracker }

func (c *humanWaitWriteFailureTracker) UpdateIssueState(context.Context, string, string) error {
	return connector.ErrStateUpdateBlocked
}

func TestHumanWaitFailureKeepsLane(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"question store", "tracker write"} {
		t.Run(failure, func(t *testing.T) {
			db := openWorkAttemptRecoveryStore(t, t.Context())
			issue := recoveryTestIssue()
			issue.State = "Rework"
			issue.WorkpadSignal = &workpad.Signal{Status: workpad.StatusBlocked, HumanAction: "Inspect hardware", Blockers: []workpad.Blocker{{Owner: workpad.BlockerOwnerHuman}}}
			tracker := &questionTracker{Connector: memory.New(memory.Config{Issues: []connector.Issue{issue}, Stateful: true})}
			o := newWorkAttemptRecoveryOrchestratorWithConnector(t, db, nil, tracker)
			if failure == "question store" {
				o.workAttempts = unavailableHumanQuestionStore{Store: db, HumanQuestionStore: db.(store.HumanQuestionStore)}
			} else {
				o.connector = &humanWaitWriteFailureTracker{tracker}
			}
			state := newState(o.cfg)
			handled, changed, err := o.reconcileHumanWaitLane(t.Context(), &state, &issue, time.Now())
			if !handled || changed || err == nil {
				t.Fatalf("failure result: %v, %v, %v", handled, changed, err)
			}
			if failure == "tracker write" && !errors.Is(err, connector.ErrStateUpdateBlocked) {
				t.Fatalf("write error = %v", err)
			}
			if len(state.Blocked) != 0 {
				t.Fatal("infrastructure failure attributed to human")
			}
			current, err := tracker.FetchIssueStatesByIDs(t.Context(), []string{issue.ID})
			if err != nil || len(current) != 1 || current[0].State != "Rework" {
				t.Fatalf("failure changed lane: %v, %v", current, err)
			}
		})
	}
}

func TestHumanWaitCompletionUsesLaneRecovery(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, message, lane string
		question            bool
	}{
		{"repeated recorded question", "May I deploy this release?", "Human Review", true},
		{"human blocker", "## Codex Workpad\n```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - owner: human\n    reason: physical check\nhuman_action: Check the hardware\n```", "Blocked", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := openWorkAttemptRecoveryStore(t, t.Context())
			o := newWorkAttemptRecoveryOrchestrator(t, db, nil)
			issue := recoveryTestIssue()
			issue.State = "Rework"
			now := time.Now()
			attempt := startRecoveryWorkAttempt(t, t.Context(), db, issue, store.WorkAttemptStatusActive, "", now)
			if tc.question {
				q := store.HumanQuestion{ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, Key: "approval", Body: tc.message}
				if _, err := db.(store.HumanQuestionStore).ReserveHumanQuestion(t.Context(), q); err != nil {
					t.Fatal(err)
				}
			}
			state := newState(o.cfg)
			state.Claimed[issue.ID] = Claimed{Issue: issue}
			event := runner.Completion{IssueID: issue.ID, CompletedAt: now.Add(time.Second), Result: runner.RunResult{FinalMessage: tc.message}}
			if !o.handlePermissionWaitCompletion(t.Context(), &state, event, Running{Issue: issue, WorkAttemptID: attempt, StartedAt: now}) {
				t.Fatal("explicit wait not handled")
			}
			blocked := state.Blocked[issue.ID]
			if blocked.Issue.State != tc.lane || blocked.Recovery == nil || protectedRecoveryPark(*blocked.Recovery) {
				t.Fatalf("wait lane = %+v", blocked)
			}
			receipt, err := db.WorkAttempt(t.Context(), attempt)
			if err != nil || receipt.TerminalState != store.WorkAttemptTerminalSuccess || receipt.Phase != "waiting" {
				t.Fatalf("wait receipt = %+v, %v", receipt, err)
			}
			if len(state.Claimed) != 0 {
				t.Fatal("wait retained worker claim")
			}
		})
	}
}

func TestHumanWaitReplyReachesDispatchIssue(t *testing.T) {
	t.Parallel()
	db := openWorkAttemptRecoveryStore(t, t.Context())
	o := newWorkAttemptRecoveryOrchestrator(t, db, nil)
	issue := recoveryTestIssue()
	issue.State = "Rework"
	q := store.HumanQuestion{ProjectID: "detent", IssueID: issue.ID, Identifier: issue.Identifier, Key: "disposition", Body: "Where should it go?", QuestionCommentID: "question", AnswerCommentID: "answer", AnswerBody: "Use the Backlog"}
	questions := db.(store.HumanQuestionStore)
	if _, err := questions.ReserveHumanQuestion(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if err := questions.RecordHumanQuestionComment(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	if err := questions.RecordHumanQuestionAnswer(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	state := newState(o.cfg)
	if held, changed, err := o.reconcileHumanWaitLane(t.Context(), &state, &issue, time.Now()); held || changed || err != nil {
		t.Fatalf("answered issue held: %v %v %v", held, changed, err)
	}
	if len(issue.Comments) != 1 || issue.Comments[0].Body != q.AnswerBody || !issue.Comments[0].AuthorAuthorized {
		t.Fatalf("dispatch lost authorized reply: %+v", issue.Comments)
	}
}
