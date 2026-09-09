package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
)

func humanQuestionMarker(q store.HumanQuestion) string {
	sum := sha256.Sum256([]byte(q.ProjectID + "\x00" + q.IssueID + "\x00" + q.Key))
	return "<!-- detent-question:" + hex.EncodeToString(sum[:]) + " -->"
}

func (o *Orchestrator) attachHumanQuestionTool(request *RunRequest) {
	questions, ok := o.workAttempts.(store.HumanQuestionStore)
	if !ok {
		return
	}
	reader, ok := o.connector.(connector.IssueCommentReader)
	if !ok {
		return
	}
	if _, ok := o.connector.(connector.IssueCommentAuthorizer); !ok {
		return
	}
	issue := request.Issue
	request.AgentTools = append(request.AgentTools, runner.AgentTool{
		Name:        "ask_human_question",
		Description: "Ask one concise researched question in a comment on your assigned issue. Finish all independent work first. Detent persists the wait without changing the lane or creating dependencies. Reuse the stable key on retries; use a new key only for a focused follow-up after an ambiguous reply. Replies authorize only what they actually say.",
		InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["key","question"],"properties":{"key":{"type":"string","minLength":1},"question":{"type":"string","minLength":1}}}`),
	})
	request.AgentToolHandler = func(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
		fail := func(err error) (runner.AgentToolResult, error) {
			return runner.AgentToolResult{Content: err.Error()}, nil
		}
		if call.Name != "ask_human_question" {
			return fail(errors.New("unsupported tool"))
		}
		var input struct {
			Key      string `json:"key"`
			Question string `json:"question"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(call.Arguments)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return fail(err)
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			return fail(errors.New("expected one question request"))
		}
		q := store.HumanQuestion{ProjectID: o.cfg.Project.ID, IssueID: issue.ID, Identifier: issue.Identifier, Key: strings.TrimSpace(input.Key), Body: strings.TrimSpace(input.Question), WorkFingerprint: humanQuestionWorkFingerprint(issue)}
		if strings.HasPrefix(q.Key, "migration:") {
			q.WorkFingerprint = ""
		}
		created, err := questions.ReserveHumanQuestion(ctx, q)
		if err != nil {
			return fail(err)
		}
		if created {
			if err := o.connector.CreateComment(ctx, issue.ID, q.Body+"\n\n"+humanQuestionMarker(q)); err != nil {
				return fail(err)
			}
		}
		records, err := questions.HumanQuestions(ctx, q.ProjectID, q.IssueID)
		if err != nil {
			return fail(err)
		}
		for _, existing := range records {
			if existing.Key != q.Key {
				continue
			}
			if existing.Body != q.Body {
				return fail(errors.New("question key already has different text; reuse the original question"))
			}
			if existing.AnswerCommentID != "" {
				return runner.AgentToolResult{Success: true, Content: "Authorized reply to interpret (not blanket approval):\n" + existing.AnswerBody}, nil
			}
			comments, err := reader.FetchIssueComments(ctx, issue)
			if err != nil {
				return fail(err)
			}
			for _, comment := range comments {
				if comment.ID != "" && strings.Contains(comment.Body, humanQuestionMarker(existing)) {
					existing.QuestionCommentID = comment.ID
					if err := questions.RecordHumanQuestionComment(ctx, existing); err != nil {
						return fail(err)
					}
					return runner.AgentToolResult{Success: true, Content: "Question recorded on " + issue.Identifier + " (comment " + comment.ID + "). Keep the lane and PR intact. End with an in_progress Workpad; Detent waits for an authorized reply. Do not create a dependency or claim completion."}, nil
				}
			}
			return fail(errors.New("question posting outcome is uncertain; Detent will reconcile the original thread without posting a duplicate"))
		}
		return fail(errors.New("another question is already awaiting a reply on this issue; reuse it"))
	}
}

func (o *Orchestrator) humanQuestionWaiting(ctx context.Context, issue *connector.Issue) (bool, error) {
	questions, ok := o.workAttempts.(store.HumanQuestionStore)
	if !ok {
		return false, nil
	}
	records, err := questions.HumanQuestions(ctx, o.cfg.Project.ID, issue.ID)
	if err != nil {
		return false, err
	}
	for _, q := range records {
		if q.AnswerCommentID != "" {
			issue.Comments = append(issue.Comments, connector.IssueComment{ID: q.AnswerCommentID, Body: q.AnswerBody, AuthorAuthorized: true})
			continue
		}
		if fingerprint := humanQuestionWorkFingerprint(*issue); fingerprint != "" && fingerprint != q.WorkFingerprint {
			return false, nil
		}
		reader, ok := o.connector.(connector.IssueCommentReader)
		if !ok {
			return true, errors.New("human question comment reader unavailable")
		}
		authorizer, ok := o.connector.(connector.IssueCommentAuthorizer)
		if !ok {
			return true, errors.New("human reply authorization unavailable")
		}
		comments, err := reader.FetchIssueComments(ctx, *issue)
		if err != nil {
			return true, err
		}
		questionIndex := -1
		for i, comment := range comments {
			if comment.ID != "" && (comment.ID == q.QuestionCommentID || strings.Contains(comment.Body, humanQuestionMarker(q))) {
				questionIndex = i
				q.QuestionCommentID = comment.ID
				if err := questions.RecordHumanQuestionComment(ctx, q); err != nil {
					return true, err
				}
				break
			}
		}
		if questionIndex < 0 {
			return true, nil
		}
		question := comments[questionIndex]
		for index, comment := range comments {
			if comment.ID == "" || comment.ID == question.ID || comment.CreatedAt == nil || question.CreatedAt == nil || (comment.CreatedAt.Before(*question.CreatedAt) || (comment.CreatedAt.Equal(*question.CreatedAt) && index <= questionIndex)) || strings.EqualFold(comment.AuthorKind, "bot") || strings.TrimSpace(comment.Body) == "" || strings.Contains(comment.Body, "<!-- detent-") || autoPromoteIsWorkpadComment(comment.Body) {
				continue
			}
			authorized, err := authorizer.IsIssueCommentAuthorAuthorized(ctx, *issue, comment)
			if err != nil {
				return true, err
			}
			if !authorized {
				continue
			}
			q.AnswerCommentID, q.AnswerBody = comment.ID, comment.Body
			if err := questions.RecordHumanQuestionAnswer(ctx, q); err != nil {
				return true, err
			}
			issue.Comments = comments
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

func (o *Orchestrator) completeHumanQuestionWait(ctx context.Context, state *State, event runner.Completion, running Running) bool {
	questions, ok := o.workAttempts.(store.HumanQuestionStore)
	if !ok {
		return false
	}
	records, err := questions.HumanQuestions(ctx, o.cfg.Project.ID, running.Issue.ID)
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("read human question state before completion failed", "issue_id", running.Issue.ID, "error", err)
		}
		return true
	}
	for _, q := range records {
		if q.AnswerCommentID != "" {
			continue
		}
		q.WorkFingerprint = humanQuestionWorkFingerprint(running.Issue)
		if err := questions.RecordHumanQuestionWork(ctx, q); err != nil {
			return true
		}
		if event.Result.Tokens != (TokenTotals{}) {
			running.Tokens = event.Result.Tokens
		}
		if !o.completeDurableWorkAttemptWithMetadata(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "waiting", "waiting for a human reply on the original issue", nil) {
			return true
		}
		o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalSuccess, nil, "", "")
		o.releaseTerminalAttemptClaim(ctx, state, running.Issue, event.CompletedAt)
		delete(state.Retry, running.Issue.ID)
		return true
	}
	return false
}

func humanQuestionWorkFingerprint(issue connector.Issue) string {
	pr := issue.PullRequest
	if pr == nil || (pr.MergeableState != "dirty" && len(pr.UnresolvedReviewThreads) == 0 && len(pr.RequiredCheckFailures) == 0) {
		return ""
	}
	data, err := json.Marshal(struct {
		Head, Base, Mergeable string
		Threads               []connector.PullRequestReviewThread
		Failures              []connector.PullRequestCheck
	}{pr.HeadSHA, pr.BaseSHA, pr.MergeableState, pr.UnresolvedReviewThreads, pr.RequiredCheckFailures})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
