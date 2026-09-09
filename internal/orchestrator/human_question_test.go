package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

type questionTracker struct {
	*memory.Connector
	mu        sync.Mutex
	comments  []connector.IssueComment
	posts     int
	postError bool
}

func (c *questionTracker) CreateComment(_ context.Context, _ string, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.posts++
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	c.comments = append(c.comments, connector.IssueComment{ID: strconv.Itoa(c.posts), Body: body, AuthorLogin: "worker", CreatedAt: &at})
	if c.postError {
		return errors.New("response lost after posting")
	}
	return nil
}

func (c *questionTracker) FetchIssueComments(context.Context, connector.Issue) ([]connector.IssueComment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]connector.IssueComment(nil), c.comments...), nil
}

func (c *questionTracker) IsIssueCommentAuthorAuthorized(_ context.Context, _ connector.Issue, comment connector.IssueComment) (bool, error) {
	return comment.AuthorAuthorized, nil
}

func TestHumanQuestionRestartAndConcurrentRequests(t *testing.T) {
	t.Parallel()
	for _, lostResponse := range []bool{false, true} {
		t.Run(strconv.FormatBool(lostResponse), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "questions.db")
			db, err := store.Open(t.Context(), store.Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			tracker := &questionTracker{Connector: memory.New(memory.Config{}), postError: lostResponse}
			o := &Orchestrator{connector: tracker, workAttempts: db}
			request := RunRequest{Issue: connector.Issue{ID: "issue", Identifier: "owner/repo#647", State: "Rework"}}
			o.attachHumanQuestionTool(&request)
			call := runner.AgentToolCall{Name: "ask_human_question", Arguments: json.RawMessage(`{"key":"delivery","question":"Should delivery remain manual? I recommend a reviewed manual handoff until approved payloads can be pinned."}`)}
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					if _, err := request.AgentToolHandler(t.Context(), call); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if tracker.posts != 1 {
				t.Fatalf("posted %d questions", tracker.posts)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = store.Open(t.Context(), store.Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			})
			o.workAttempts = db
			o.attachHumanQuestionTool(&request)
			result, err := request.AgentToolHandler(t.Context(), call)
			if err != nil || !result.Success || tracker.posts != 1 {
				t.Fatalf("restart = %+v, %v, posts %d", result, err, tracker.posts)
			}
			for range 3 {
				waiting, err := o.humanQuestionWaiting(t.Context(), &request.Issue)
				if err != nil || !waiting || request.Issue.State != "Rework" {
					t.Fatalf("wait = %v, %v", waiting, err)
				}
			}
			at := time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC)
			tracker.comments = append(tracker.comments, connector.IssueComment{ID: "answer", Body: "Yes, use a manual handoff. This does not authorize live sends.", AuthorLogin: "cory", AuthorAuthorized: true, CreatedAt: &at})
			waiting, err := o.humanQuestionWaiting(t.Context(), &request.Issue)
			if err != nil || waiting {
				t.Fatalf("reply wait = %v, %v", waiting, err)
			}
			records, err := db.(store.HumanQuestionStore).HumanQuestions(t.Context(), "", "issue")
			if err != nil || len(records) != 1 || records[0].AnswerCommentID != "answer" || records[0].QuestionCommentID != "1" {
				t.Fatalf("records = %+v, %v", records, err)
			}
		})
	}
}

func TestHumanQuestionReplyAuthorization(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, login, kind, body    string
		authorized, after, resumes bool
	}{
		{name: "authorized ordinary reply", login: "cory", body: "Use manual delivery", authorized: true, after: true, resumes: true},
		{name: "ambiguous reply resumes interpretation", login: "cory", body: "Maybe, explain the alternative", authorized: true, after: true, resumes: true},
		{name: "unauthorized", login: "outsider", body: "Approved", after: true},
		{name: "bot", login: "bot", kind: "Bot", body: "Approved", authorized: true, after: true},
		{name: "same account human reply", login: "worker", body: "Use manual delivery", authorized: true, after: true, resumes: true},
		{name: "old reply", login: "cory", body: "Approved", authorized: true},
		{name: "workpad", login: "cory", body: "## Codex Workpad", authorized: true, after: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "questions.db")})
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tracker := &questionTracker{Connector: memory.New(memory.Config{})}
			o := &Orchestrator{connector: tracker, workAttempts: db}
			request := RunRequest{Issue: connector.Issue{ID: "issue", Identifier: "owner/repo#1"}}
			o.attachHumanQuestionTool(&request)
			result, err := request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "ask_human_question", Arguments: json.RawMessage(`{"key":"decision","question":"Use manual delivery?"}`)})
			if err != nil || !result.Success {
				t.Fatalf("ask = %+v, %v", result, err)
			}
			at := *tracker.comments[0].CreatedAt
			if tt.after {
				at = at.Add(time.Second)
			} else {
				at = at.Add(-time.Second)
			}
			tracker.comments = append(tracker.comments, connector.IssueComment{ID: "reply", AuthorLogin: tt.login, AuthorKind: tt.kind, Body: tt.body, AuthorAuthorized: tt.authorized, CreatedAt: &at})
			waiting, err := o.humanQuestionWaiting(t.Context(), &request.Issue)
			if err != nil || waiting == tt.resumes {
				t.Fatalf("waiting = %v, %v", waiting, err)
			}
		})
	}
}

func TestHumanQuestionIndependentRework(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(t.TempDir(), "questions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tracker := &questionTracker{Connector: memory.New(memory.Config{})}
	o := &Orchestrator{connector: tracker, workAttempts: db}
	request := RunRequest{Issue: connector.Issue{ID: "issue", Identifier: "owner/repo#1", State: "Rework"}}
	o.attachHumanQuestionTool(&request)
	result, err := request.AgentToolHandler(t.Context(), runner.AgentToolCall{Name: "ask_human_question", Arguments: json.RawMessage(`{"key":"sender","question":"Use manual delivery?"}`)})
	if err != nil || !result.Success {
		t.Fatalf("question = %+v, %v", result, err)
	}
	request.Issue.PullRequest = &connector.PullRequest{HeadSHA: "head", BaseSHA: "base", MergeableState: "dirty"}
	if waiting, err := o.humanQuestionWaiting(t.Context(), &request.Issue); err != nil || waiting {
		t.Fatalf("independent rework blocked: %v, %v", waiting, err)
	}
	questions := db.(store.HumanQuestionStore)
	records, err := questions.HumanQuestions(t.Context(), "", "issue")
	if err != nil {
		t.Fatal(err)
	}
	q := records[0]
	q.WorkFingerprint = humanQuestionWorkFingerprint(request.Issue)
	if err := questions.RecordHumanQuestionWork(t.Context(), q); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if waiting, err := o.humanQuestionWaiting(t.Context(), &request.Issue); err != nil || !waiting {
			t.Fatalf("unchanged PR repeatedly dispatched: %v, %v", waiting, err)
		}
	}
	if q.AnswerCommentID != "" {
		t.Fatal("independent rework approved decision")
	}
	request.Issue.PullRequest.BaseSHA = "new-base"
	if waiting, err := o.humanQuestionWaiting(t.Context(), &request.Issue); err != nil || waiting {
		t.Fatalf("new rework blocked: %v, %v", waiting, err)
	}
}
