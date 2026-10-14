package connector

// Capabilities summarizes which write operations a connector supports.
// Serializable so it can flow into template data and telemetry snapshots.
type Capabilities struct {
	UpdateIssueState      bool `json:"update_issue_state"`
	SetAssignee           bool `json:"set_assignee"`
	SetField              bool `json:"set_field"`
	CreateComment         bool `json:"create_comment"`
	CreateWorkItems       bool `json:"create_work_items"`
	CloseIssues           bool `json:"close_issues"`
	RemoveFromProject     bool `json:"remove_from_project"`
	SetIssueFields        bool `json:"set_issue_fields"`
	ClearIssueFields      bool `json:"clear_issue_fields"`
	CommentOnPullRequests bool `json:"comment_on_pull_requests"`
	UpdateComments        bool `json:"update_comments"`
	DeleteComments        bool `json:"delete_comments"`
}

// CapabilityReporter lets a backend that only partially implements the base
// Connector write methods declare what actually works. When implemented, the
// reported value is authoritative and DetectCapabilities returns it verbatim.
type CapabilityReporter interface {
	Capabilities() Capabilities
}

func DetectCapabilities(c Connector) Capabilities {
	if c == nil {
		return Capabilities{}
	}
	if reporter, ok := c.(CapabilityReporter); ok {
		return reporter.Capabilities()
	}

	_, canCreateWorkItems := c.(IssueUpserter)
	_, canCloseIssues := c.(IssueCloser)
	_, canRemoveFromProject := c.(ProjectRemover)
	_, canSetIssueFields := c.(IssueFieldSetter)
	_, canClearIssueFields := c.(IssueFieldClearer)
	_, canCommentOnPullRequests := c.(PullRequestCommenter)
	_, canUpdateComments := c.(IssueCommentUpdater)
	_, canDeleteComments := c.(IssueCommentDeleter)

	return Capabilities{
		UpdateIssueState:      true,
		SetAssignee:           true,
		SetField:              true,
		CreateComment:         true,
		CreateWorkItems:       canCreateWorkItems,
		CloseIssues:           canCloseIssues,
		RemoveFromProject:     canRemoveFromProject,
		SetIssueFields:        canSetIssueFields,
		ClearIssueFields:      canClearIssueFields,
		CommentOnPullRequests: canCommentOnPullRequests,
		UpdateComments:        canUpdateComments,
		DeleteComments:        canDeleteComments,
	}
}
