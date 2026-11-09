package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

func TestINV14DispatchQuestionTool(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, gateKind, passState string
		enabled                   bool
	}{
		{name: "command auto merge", gateKind: gate.KindCommand, passState: "Merging", enabled: true},
		{name: "artifact auto merge", gateKind: gate.KindArtifact, passState: "Merging", enabled: true},
		{name: "human review gate", gateKind: gate.KindHumanReview, passState: "Merging", enabled: true},
		{name: "auto promote disabled", gateKind: gate.KindCommand, passState: "Merging"},
		{name: "different pass state", gateKind: gate.KindCommand, passState: "Human Review", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := normalizeConfig(Config{
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo"},
				TerminalStates:      []string{"Done"},
				Project:             scheduler.ProjectCandidate{ID: "detent"},
				AutoPromote:         AutoPromoteConfig{Enabled: tc.enabled, PassState: tc.passState, Gate: gate.Config{Kind: tc.gateKind}},
			})
			worker := newWorkerHostRunner()
			o := Orchestrator{cfg: cfg, connector: fakehub.New(fakehub.Config{}), workAttempts: &recordingWorkAttemptStore{}, supervisor: newTestSupervisor(t, worker, cfg), runResults: make(chan runner.Completion, 1)}
			state := newState(cfg)
			if !o.dispatchIssue(t.Context(), &state, dispatchTestIssue("issue", "Todo"), 1, time.Now(), "") {
				t.Fatal("dispatch did not start")
			}
			request := receiveWorkerHostRunRequest(t, worker.started)
			for _, tool := range request.AgentTools {
				if tool.Name == "ask_human_question" {
					t.Fatal("worker received ask_human_question")
				}
			}
		})
	}
}

func TestINV14LegacyQuestionRowDispatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, gateKind, commentID string
	}{
		{name: "command published", gateKind: gate.KindCommand, commentID: "123"},
		{name: "human review published", gateKind: gate.KindHumanReview, commentID: "123"},
		{name: "human review unconfirmed", gateKind: gate.KindHumanReview},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := &historicalQuestionAttemptStore{questions: []store.HumanQuestion{{
				ProjectID: "detent", IssueID: "issue", Key: "decision", Identifier: "owner/repository#3035",
				Body: "What next?", QuestionCommentID: tc.commentID,
			}}}
			cfg := normalizeConfig(Config{MaxConcurrentAgents: 1, ActiveStates: []string{"Todo"}, TerminalStates: []string{"Done"}, Project: scheduler.ProjectCandidate{ID: "detent"}, AutoPromote: AutoPromoteConfig{Enabled: true, PassState: "Merging", Gate: gate.Config{Kind: tc.gateKind}}})
			worker := newWorkerHostRunner()
			o := Orchestrator{cfg: cfg, connector: fakehub.New(fakehub.Config{}), workAttempts: db, supervisor: newTestSupervisor(t, worker, cfg), runResults: make(chan runner.Completion, 1)}
			state := newState(cfg)
			if !o.dispatchIssue(t.Context(), &state, dispatchTestIssue("issue", "Todo"), 1, time.Now(), "") {
				t.Fatal("legacy unanswered question prevented dispatch")
			}
			request := receiveWorkerHostRunRequest(t, worker.started)
			if request.Issue.ID != "issue" {
				t.Fatalf("dispatched issue = %q", request.Issue.ID)
			}
			if db.reads != 0 {
				t.Fatal("dispatch consulted historical questions")
			}
			receipts, err := db.HumanQuestions(t.Context(), "detent", "issue")
			if err != nil || len(receipts) != 1 || receipts[0].AnswerCommentID != "" {
				t.Fatalf("historical receipt changed: %+v, %v", receipts, err)
			}
		})
	}
}

type historicalQuestionAttemptStore struct {
	recordingWorkAttemptStore
	questions []store.HumanQuestion
	reads     int
}

func (s *historicalQuestionAttemptStore) HumanQuestions(_ context.Context, projectID, issueID string) ([]store.HumanQuestion, error) {
	s.reads++
	var questions []store.HumanQuestion
	for _, question := range s.questions {
		if question.ProjectID == projectID && question.IssueID == issueID {
			questions = append(questions, question)
		}
	}
	return questions, nil
}
