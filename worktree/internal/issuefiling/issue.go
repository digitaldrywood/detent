package issuefiling

import "context"

type Issue struct {
	State      string `json:"state,omitempty"`
	Reused     bool   `json:"-"`
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	Number     int    `json:"number,omitempty"`
	URL        string `json:"url,omitempty"`
	Body       string `json:"-"`
	Closed     bool   `json:"-"`
}

type IssueDraft struct {
	Title    string
	Body     string
	Labels   []string
	Priority *int
}

type IssueStore interface {
	FindIntakeIssue(context.Context, string) (Issue, bool, error)
	CreateIntakeIssue(context.Context, IssueDraft) (Issue, error)
	UpdateIntakeIssue(context.Context, string, IssueDraft) (Issue, error)
	SetIntakeIssueState(context.Context, string, string) error
}
