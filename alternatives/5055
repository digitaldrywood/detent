package store

import (
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
