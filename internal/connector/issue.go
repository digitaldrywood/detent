package connector

import "time"

type Issue struct {
	ID               string       `json:"id,omitempty"`
	Identifier       string       `json:"identifier,omitempty"`
	Title            string       `json:"title,omitempty"`
	Description      string       `json:"description,omitempty"`
	Priority         *int         `json:"priority,omitempty"`
	State            string       `json:"state,omitempty"`
	BranchName       string       `json:"branch_name,omitempty"`
	URL              string       `json:"url,omitempty"`
	AssigneeID       string       `json:"assignee_id,omitempty"`
	BlockedBy        []BlockedRef `json:"blocked_by"`
	Labels           []string     `json:"labels"`
	AssignedToWorker bool         `json:"assigned_to_worker"`
	CreatedAt        *time.Time   `json:"created_at,omitempty"`
	UpdatedAt        *time.Time   `json:"updated_at,omitempty"`
	ModelOverride    string       `json:"model_override"`
}

type BlockedRef struct {
	ID         string `json:"id,omitempty"`
	Identifier string `json:"identifier"`
	State      string `json:"state,omitempty"`
}

func NewIssue() Issue {
	return Issue{
		BlockedBy:        []BlockedRef{},
		Labels:           []string{},
		AssignedToWorker: true,
	}
}
