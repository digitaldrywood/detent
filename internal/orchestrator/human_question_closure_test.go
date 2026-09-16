package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/operations"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestRefreshResolvesTerminalHumanQuestions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, state      string
		closed, resolved bool
		fetchFails       bool
	}{
		{"closed completed", "Todo", true, true, false},
		{"done", "Done", false, true, false},
		{"cancelled", "Cancelled", false, true, false},
		{"active", "Todo", false, false, false},
		{"unavailable tracker", "Done", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openWorkAttemptRecoveryStore(t, t.Context())
			questions := db.(store.HumanQuestionStore)
			q := store.HumanQuestion{ProjectID: "detent", IssueID: "issue", Identifier: "owner/repo#1", Key: "target", Body: "Which target?", QuestionCommentID: "123"}
			if _, err := questions.ReserveHumanQuestion(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			if err := questions.RecordHumanQuestionComment(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			reader := db.(interface {
				OpenHumanQuestions(context.Context) ([]operations.Decision, error)
			})
			issue := connector.Issue{ID: q.IssueID, Identifier: q.Identifier, State: tc.state, Closed: tc.closed}
			o := newWorkAttemptRecoveryOrchestratorWithConnector(t, db, nil, memory.New(memory.Config{Issues: []connector.Issue{issue}}))
			if tc.fetchFails {
				o.connector = unavailableQuestionIssueTracker{Connector: o.connector}
			}
			state := newState(o.cfg)
			// No previous board state: old questions must also resolve after a restart.
			o.refreshTransitionSets(t.Context(), &state, tickFetchedIssues{}, tickPreviousState{})
			got, err := reader.OpenHumanQuestions(t.Context())
			if err != nil || (len(got) == 0) != tc.resolved {
				t.Fatalf("open questions = %+v, %v; resolved=%v", got, err, tc.resolved)
			}
			if !tc.resolved {
				return
			}
			issue.Closed, issue.State = false, "Todo"
			o.connector = memory.New(memory.Config{Issues: []connector.Issue{issue}})
			o.refreshTransitionSets(t.Context(), &state, tickFetchedIssues{}, tickPreviousState{})
			if waiting, err := o.humanQuestionWaiting(t.Context(), &issue); err != nil || waiting {
				t.Fatalf("reopened wait = %v, %v", waiting, err)
			}
			if len(issue.Comments) != 0 {
				t.Fatal("closure was presented as an authorized human reply")
			}
			got, err = reader.OpenHumanQuestions(t.Context())
			if err != nil || len(got) != 0 {
				t.Fatalf("reopened questions = %+v, %v", got, err)
			}
			o.connector = &questionTracker{Connector: memory.New(memory.Config{})}
			request := RunRequest{Issue: issue}
			o.attachHumanQuestionTool(&request)
			result, err := request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "ask_human_question", Arguments: json.RawMessage(`{"key":"target","question":"Which target?"}`)})
			if err != nil || !result.Success || strings.Contains(result.Content, "Authorized reply") || !strings.Contains(result.Content, "No human reply or approval") {
				t.Fatalf("closure tool reply = %+v, %v", result, err)
			}

			q.Key = "new-target"
			if created, err := questions.ReserveHumanQuestion(t.Context(), q); err != nil || !created {
				t.Fatalf("new question = %v, %v", created, err)
			}
			if err := questions.RecordHumanQuestionComment(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			got, err = reader.OpenHumanQuestions(t.Context())
			if err != nil || len(got) != 1 {
				t.Fatalf("new open question = %+v, %v", got, err)
			}
		})
	}
}

type unavailableQuestionIssueTracker struct{ connector.Connector }

func (unavailableQuestionIssueTracker) FetchIssueStatesByIDs(context.Context, []string) ([]connector.Issue, error) {
	return nil, errors.New("tracker unavailable")
}
