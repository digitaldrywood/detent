package connector

import (
	"time"

	"gopkg.in/yaml.v3"
)

type Issue struct {
	ID               string            `json:"id,omitempty" yaml:"id,omitempty"`
	Identifier       string            `json:"identifier,omitempty" yaml:"identifier,omitempty"`
	Title            string            `json:"title,omitempty" yaml:"title,omitempty"`
	Description      string            `json:"description,omitempty" yaml:"description,omitempty"`
	Priority         *int              `json:"priority,omitempty" yaml:"priority,omitempty"`
	State            string            `json:"state,omitempty" yaml:"state,omitempty"`
	BranchName       string            `json:"branch_name,omitempty" yaml:"branch_name,omitempty"`
	URL              string            `json:"url,omitempty" yaml:"url,omitempty"`
	Closed           bool              `json:"closed,omitempty" yaml:"closed,omitempty"`
	ClosedReason     string            `json:"closed_reason,omitempty" yaml:"closed_reason,omitempty"`
	PRNumber         *int              `json:"pr_number,omitempty" yaml:"pr_number,omitempty"`
	PRRepository     string            `json:"pr_repository,omitempty" yaml:"pr_repository,omitempty"`
	PullRequest      *PullRequest      `json:"pull_request,omitempty" yaml:"pull_request,omitempty"`
	AuthorID         string            `json:"author_id,omitempty" yaml:"author_id,omitempty"`
	AssigneeID       string            `json:"assignee_id,omitempty" yaml:"assignee_id,omitempty"`
	Assignees        []string          `json:"assignees,omitempty" yaml:"assignees,omitempty"`
	BlockedBy        []BlockedRef      `json:"blocked_by" yaml:"blocked_by"`
	ChildIssues      []BlockedRef      `json:"child_issues,omitempty" yaml:"child_issues,omitempty"`
	BlockerReason    string            `json:"blocker_reason,omitempty" yaml:"blocker_reason,omitempty"`
	Labels           []string          `json:"labels" yaml:"labels"`
	Comments         []IssueComment    `json:"comments,omitempty" yaml:"comments,omitempty"`
	Fields           map[string]string `json:"fields,omitempty" yaml:"fields,omitempty"`
	AssignedToWorker bool              `json:"assigned_to_worker" yaml:"assigned_to_worker"`
	CreatedAt        *time.Time        `json:"created_at,omitempty" yaml:"created_at,omitempty"`
	UpdatedAt        *time.Time        `json:"updated_at,omitempty" yaml:"updated_at,omitempty"`
	StageUpdatedAt   *time.Time        `json:"stage_updated_at,omitempty" yaml:"stage_updated_at,omitempty"`
	ModelOverride    string            `json:"model_override" yaml:"model_override"`
}

type BlockedRef struct {
	ID         string `json:"id,omitempty" yaml:"id,omitempty"`
	Identifier string `json:"identifier" yaml:"identifier"`
	State      string `json:"state,omitempty" yaml:"state,omitempty"`
}

type PullRequest struct {
	Number                       int                  `json:"number,omitempty" yaml:"number,omitempty"`
	URL                          string               `json:"url,omitempty" yaml:"url,omitempty"`
	BranchName                   string               `json:"branch_name,omitempty" yaml:"branch_name,omitempty"`
	State                        string               `json:"state,omitempty" yaml:"state,omitempty"`
	MergeableState               string               `json:"mergeable_state,omitempty" yaml:"mergeable_state,omitempty"`
	Draft                        bool                 `json:"draft,omitempty" yaml:"draft,omitempty"`
	ActivityAt                   *time.Time           `json:"activity_at,omitempty" yaml:"activity_at,omitempty"`
	HeadSHA                      string               `json:"head_sha,omitempty" yaml:"head_sha,omitempty"`
	CIStatus                     string               `json:"ci_status,omitempty" yaml:"ci_status,omitempty"`
	CheckRunCount                int                  `json:"check_run_count,omitempty" yaml:"check_run_count,omitempty"`
	StatusContextCount           int                  `json:"status_context_count,omitempty" yaml:"status_context_count,omitempty"`
	CIDurationSeconds            int64                `json:"ci_duration_seconds,omitempty" yaml:"ci_duration_seconds,omitempty"`
	SlowChecks                   []PullRequestCheck   `json:"slow_checks,omitempty" yaml:"slow_checks,omitempty"`
	RunningChecks                []string             `json:"running_checks,omitempty" yaml:"running_checks,omitempty"`
	CodexReviewState             string               `json:"codex_review_state,omitempty" yaml:"codex_review_state,omitempty"`
	CodexReviewSubmittedAt       *time.Time           `json:"codex_review_submitted_at,omitempty" yaml:"codex_review_submitted_at,omitempty"`
	CodexReviewFindings          []PullRequestFinding `json:"codex_review_findings,omitempty" yaml:"codex_review_findings,omitempty"`
	LatestCodexReviewState       string               `json:"latest_codex_review_state,omitempty" yaml:"latest_codex_review_state,omitempty"`
	LatestCodexReviewCommitSHA   string               `json:"latest_codex_review_commit_sha,omitempty" yaml:"latest_codex_review_commit_sha,omitempty"`
	LatestCodexReviewSubmittedAt *time.Time           `json:"latest_codex_review_submitted_at,omitempty" yaml:"latest_codex_review_submitted_at,omitempty"`
}

type PullRequestCheck struct {
	Name            string `json:"name,omitempty" yaml:"name,omitempty"`
	Status          string `json:"status,omitempty" yaml:"status,omitempty"`
	Conclusion      string `json:"conclusion,omitempty" yaml:"conclusion,omitempty"`
	DurationSeconds int64  `json:"duration_seconds,omitempty" yaml:"duration_seconds,omitempty"`
}

type PullRequestFinding struct {
	Body string `json:"body,omitempty" yaml:"body,omitempty"`
	URL  string `json:"url,omitempty" yaml:"url,omitempty"`
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
	Line int    `json:"line,omitempty" yaml:"line,omitempty"`
}

type IssueComment struct {
	Body string `json:"body,omitempty" yaml:"body,omitempty"`
	URL  string `json:"url,omitempty" yaml:"url,omitempty"`
}

func NewIssue() Issue {
	return Issue{
		BlockedBy:        []BlockedRef{},
		Labels:           []string{},
		Assignees:        []string{},
		Fields:           map[string]string{},
		AssignedToWorker: true,
	}
}

func (i *Issue) UnmarshalYAML(value *yaml.Node) error {
	type issue Issue

	defaults := issue(NewIssue())
	if err := value.Decode(&defaults); err != nil {
		return err
	}

	*i = Issue(defaults)
	return nil
}
