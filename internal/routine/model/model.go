package model

import "time"

type RunRecord struct {
	ProjectID    string
	RoutineName  string
	ScheduledFor time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
	Proposed     int
	Filed        int
	Deduplicated int
	Limited      int
	Issues       []IssueRecord
	Error        string
}

type IssueRecord struct {
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	URL        string `json:"url,omitempty"`
}
