package templates

import (
	"net/url"
	"slices"
	"strconv"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type NativeWorkData struct {
	Issue           tracker.NativeIssue
	Project         tracker.NativeProject
	Comments        tracker.Page[tracker.NativeComment]
	Attempts        []tracker.NativeAttempt
	Policy          *policy.Approval
	Eligibility     *runnerauth.ProjectEligibility
	DiscussionError string
	ExecutionError  string
	PolicyError     string
}

type NativeFormData struct {
	FormToken string
	Dashboard DashboardData
	Project   tracker.NativeProject
	Issue     tracker.NativeIssue
	Action    string
	Revision  string
	Key       string
	Title     string
	Body      string
	State     string
	Priority  string
	Related   string
	Operation string
	CommentID string
	Error     string
	Conflict  bool
}

func NativeNewIssuePath(projectID string) string {
	return "/projects/" + url.PathEscape(projectID) + "/issues/new"
}

func nativeFormPath(data NativeFormData) string {
	if data.Action == "create" {
		return NativeNewIssuePath(data.Dashboard.ProjectID)
	}
	return NativeIssuePath(data.Dashboard.ProjectID, data.Issue.WorkItemID) + "/edit"
}

func nativeRevision(revision tracker.Revision) string {
	return strconv.FormatInt(int64(revision), 10)
}

func nativeEditPath(projectID string, id tracker.NativeWorkItemID, action string) string {
	return NativeIssuePath(projectID, id) + "/edit?action=" + url.QueryEscape(action)
}

func nativeFormTitle(action string) string {
	switch action {
	case "create":
		return "New issue"
	case "comment":
		return "Add comment"
	case "comment_edit":
		return "Edit comment"
	case "transition":
		return "Change workflow state"
	case "dependency":
		return "Adjust dependencies"
	case "change":
		return "Create linked Change"
	default:
		return "Edit issue"
	}
}

func nativeAllowedStates(data NativeFormData) []tracker.NativeState {
	if data.Action == "create" || data.Error != "" {
		return data.Project.States
	}
	var allowed []string
	for _, state := range data.Project.States {
		if state.Name == data.Issue.State {
			allowed = state.Transitions
		}
	}
	var result []tracker.NativeState
	for _, state := range data.Project.States {
		if slices.Contains(allowed, state.Name) {
			result = append(result, state)
		}
	}
	return result
}

func nativeBodyLabel(action string) string {
	if action == "comment" || action == "comment_edit" {
		return "Comment"
	}
	return "Body"
}
func nativeRelatedLabel(action string) string {
	if action == "change" {
		return "Additional native issue IDs (space separated, optional)"
	}
	return "Related native issue ID"
}

func nativeBodyLimit(action string) string {
	if action == "create" || action == "edit" {
		return "262144"
	}
	return "65536"
}
