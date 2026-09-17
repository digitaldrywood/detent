package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operations"
)

// HumanQuestionResolvedByClosure distinguishes issue resolution from a human reply.
const HumanQuestionResolvedByClosure = "detent:issue-closed"

type HumanQuestion struct {
	AskedAt           *time.Time
	ProjectID         string
	IssueID           string
	Key               string
	Identifier        string
	Body              string
	QuestionCommentID string
	AnswerCommentID   string
	AnswerBody        string
	WorkFingerprint   string
}

type HumanQuestionStore interface {
	OpenHumanQuestionIssueIDs(context.Context, string) ([]string, error)
	ResolveHumanQuestionsByClosure(context.Context, string, string) error
	ReserveHumanQuestion(context.Context, HumanQuestion) (bool, error)
	HumanQuestions(context.Context, string, string) ([]HumanQuestion, error)
	RecordHumanQuestionComment(context.Context, HumanQuestion) error
	RecordHumanQuestionAnswer(context.Context, HumanQuestion) error
	RecordHumanQuestionWork(context.Context, HumanQuestion) error
	ReleaseHumanQuestionReservation(context.Context, HumanQuestion) error
}

func (s *sqliteStore) ReserveHumanQuestion(ctx context.Context, q HumanQuestion) (bool, error) {
	if strings.TrimSpace(q.IssueID) == "" || strings.TrimSpace(q.Key) == "" || strings.TrimSpace(q.Body) == "" || strings.TrimSpace(q.Identifier) == "" {
		return false, errors.New("human question requires issue, key, identifier, and readable question")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO human_questions (project_id, issue_id, question_key, issue_identifier, body, work_fingerprint, asked_at) VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, q.ProjectID, q.IssueID, q.Key, q.Identifier, q.Body, q.WorkFingerprint, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, fmt.Errorf("reserve human question: %w", err)
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func (s *sqliteStore) HumanQuestions(ctx context.Context, projectID, issueID string) ([]HumanQuestion, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, issue_id, question_key, issue_identifier, body, question_comment_id, answer_comment_id, answer_body, work_fingerprint FROM human_questions WHERE project_id = ? AND issue_id = ? ORDER BY rowid`, projectID, issueID)
	if err != nil {
		return nil, fmt.Errorf("read human questions: %w", err)
	}
	defer rows.Close()
	var questions []HumanQuestion
	for rows.Next() {
		var q HumanQuestion
		if err := rows.Scan(&q.ProjectID, &q.IssueID, &q.Key, &q.Identifier, &q.Body, &q.QuestionCommentID, &q.AnswerCommentID, &q.AnswerBody, &q.WorkFingerprint); err != nil {
			return nil, err
		}
		questions = append(questions, q)
	}
	return questions, rows.Err()
}

func (s *sqliteStore) RecordHumanQuestionComment(ctx context.Context, q HumanQuestion) error {
	if q.QuestionCommentID == "" {
		return errors.New("question comment ID is required")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE human_questions SET question_comment_id = ?, asked_at = COALESCE(?, asked_at) WHERE project_id = ? AND issue_id = ? AND question_key = ? AND (question_comment_id = '' OR question_comment_id = ?)`, q.QuestionCommentID, questionTime(q.AskedAt), q.ProjectID, q.IssueID, q.Key, q.QuestionCommentID)
	return err
}

func (s *sqliteStore) RecordHumanQuestionAnswer(ctx context.Context, q HumanQuestion) error {
	if q.QuestionCommentID == "" || q.AnswerCommentID == "" || strings.TrimSpace(q.AnswerBody) == "" {
		return errors.New("question and answer comment IDs and answer text are required")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE human_questions SET answer_comment_id = ?, answer_body = ? WHERE project_id = ? AND issue_id = ? AND question_key = ? AND question_comment_id = ? AND answer_comment_id = ''`, q.AnswerCommentID, q.AnswerBody, q.ProjectID, q.IssueID, q.Key, q.QuestionCommentID)
	return err
}

func (s *sqliteStore) RecordHumanQuestionWork(ctx context.Context, q HumanQuestion) error {
	_, err := s.db.ExecContext(ctx, `UPDATE human_questions SET work_fingerprint = ? WHERE project_id = ? AND issue_id = ? AND question_key = ? AND answer_comment_id = ''`, q.WorkFingerprint, q.ProjectID, q.IssueID, q.Key)
	return err
}

func (s *sqliteStore) ReleaseHumanQuestionReservation(ctx context.Context, q HumanQuestion) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM human_questions WHERE project_id = ? AND issue_id = ? AND question_key = ? AND body = ? AND question_comment_id = '' AND answer_comment_id = ''`, q.ProjectID, q.IssueID, q.Key, q.Body)
	return err
}

func questionTime(at *time.Time) sql.NullString {
	if at == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: at.UTC().Format(time.RFC3339Nano), Valid: true}
}

// OpenHumanQuestions reads the same durable unanswered records used by dispatch.
func (s *sqliteStore) OpenHumanQuestions(ctx context.Context) ([]operations.Decision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project_id, issue_identifier, body, question_comment_id, work_fingerprint, asked_at FROM human_questions WHERE answer_comment_id = '' AND question_comment_id <> '' ORDER BY julianday(asked_at), project_id, issue_identifier`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	decisions := []operations.Decision{}
	for rows.Next() {
		var d operations.Decision
		var comment string
		var at sql.NullString
		if err := rows.Scan(&d.ProjectID, &d.Issue, &d.Question, &comment, &d.WorkFingerprint, &at); err != nil {
			return nil, err
		}
		d.Kind = "question"
		if at.Valid {
			parsed, err := parseStoredTime(at.String)
			if err != nil {
				return nil, err
			}
			d.AskedAt = &parsed
		}
		if repo, number, ok := strings.Cut(d.Issue, "#"); ok && strings.Count(repo, "/") == 1 {
			d.URL = "https://github.com/" + repo + "/issues/" + number
			if _, err := strconv.ParseUint(comment, 10, 64); err == nil {
				d.URL += "#issuecomment-" + comment
			}
		}
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

// OpenHumanQuestionIssueIDs includes unconfirmed posts so closure also releases
// reservations that would otherwise prevent a new question after reopening.
func (s *sqliteStore) OpenHumanQuestionIssueIDs(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT issue_id FROM human_questions WHERE project_id = ? AND answer_comment_id = '' ORDER BY issue_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ResolveHumanQuestionsByClosure retains the question and any real human answer.
// The reserved marker is not a comment ID and must never imply human approval.
func (s *sqliteStore) ResolveHumanQuestionsByClosure(ctx context.Context, projectID, issueID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE human_questions SET answer_comment_id = ?, answer_body = ? WHERE project_id = ? AND issue_id = ? AND answer_comment_id = ''`, HumanQuestionResolvedByClosure, "Question resolved because the issue is closed or in a terminal tracker state. No human reply or approval was recorded.", projectID, issueID)
	return err
}
