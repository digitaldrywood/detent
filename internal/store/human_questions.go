package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type HumanQuestion struct {
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
	ReserveHumanQuestion(context.Context, HumanQuestion) (bool, error)
	HumanQuestions(context.Context, string, string) ([]HumanQuestion, error)
	RecordHumanQuestionComment(context.Context, HumanQuestion) error
	RecordHumanQuestionAnswer(context.Context, HumanQuestion) error
	RecordHumanQuestionWork(context.Context, HumanQuestion) error
}

func (s *sqliteStore) ReserveHumanQuestion(ctx context.Context, q HumanQuestion) (bool, error) {
	if strings.TrimSpace(q.IssueID) == "" || strings.TrimSpace(q.Key) == "" || strings.TrimSpace(q.Body) == "" || strings.TrimSpace(q.Identifier) == "" {
		return false, errors.New("human question requires issue, key, identifier, and readable question")
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO human_questions (project_id, issue_id, question_key, issue_identifier, body, work_fingerprint) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, q.ProjectID, q.IssueID, q.Key, q.Identifier, q.Body, q.WorkFingerprint)
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
	_, err := s.db.ExecContext(ctx, `UPDATE human_questions SET question_comment_id = ? WHERE project_id = ? AND issue_id = ? AND question_key = ? AND question_comment_id = ''`, q.QuestionCommentID, q.ProjectID, q.IssueID, q.Key)
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
