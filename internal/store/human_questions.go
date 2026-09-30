package store

import (
	"context"
	"fmt"
	"time"
)

// HumanQuestion is a historical receipt. Workers no longer create or wait on it.
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

// HumanQuestions reads legacy receipts without treating them as dispatch state.
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
