package templates

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

type StopRunDialogData struct {
	ProjectID         string
	Repository        string
	IssueID           string
	Identifier        string
	IssueURL          string
	Title             string
	Role              string
	Stage             string
	Destination       string
	Priority          int
	PriorityName      string
	PriorityOptions   []telemetry.StopRunPriorityOption
	Reason            string
	Attempt           int
	WorkAttemptID     int64
	DetentSessionID   int64
	ProviderSessionID string
	Outcome           string
	Error             string
	CanSubmit         bool
	RetryTransition   bool
}

func StopRunDialogPath(running telemetry.Running) string {
	projectID := strings.TrimSpace(running.ProjectID)
	issueID := strings.TrimSpace(running.ID)
	if projectID == "" || issueID == "" || strings.TrimSpace(running.StopDestination) == "" {
		return ""
	}
	values := url.Values{}
	values.Set("issue_id", issueID)
	if running.WorkAttemptID > 0 {
		values.Set("work_attempt_id", strconv.FormatInt(running.WorkAttemptID, 10))
	}
	if running.DetentSessionID > 0 {
		values.Set("detent_session_id", strconv.FormatInt(running.DetentSessionID, 10))
	}
	if sessionID := strings.TrimSpace(running.SessionID); sessionID != "" {
		values.Set("provider_session_id", sessionID)
	}
	return "/api/v1/projects/" + url.PathEscape(projectID) + "/runs/" + strconv.Itoa(running.Attempt) + "/stop?" + values.Encode()
}

func stopRunPostPath(data StopRunDialogData) string {
	return "/api/v1/projects/" + url.PathEscape(strings.TrimSpace(data.ProjectID)) + "/runs/" + strconv.Itoa(data.Attempt) + "/stop"
}

func stopRunSessionLabel(data StopRunDialogData) string {
	parts := make([]string, 0, 3)
	if data.DetentSessionID > 0 {
		parts = append(parts, "Detent "+strconv.FormatInt(data.DetentSessionID, 10))
	}
	if data.ProviderSessionID != "" {
		parts = append(parts, "provider "+data.ProviderSessionID)
	}
	parts = append(parts, "attempt "+strconv.Itoa(data.Attempt))
	return strings.Join(parts, " · ")
}

func stopRunSubmitLabel(data StopRunDialogData) string {
	if data.RetryTransition {
		return "Retry move to " + data.Destination
	}
	return "Stop run"
}

func stopRunPriorityLabel(option telemetry.StopRunPriorityOption) string {
	return option.Name + " · rank " + strconv.Itoa(option.Rank)
}

func stopRunResultDetail(data StopRunDialogData) string {
	detail := data.Destination
	if data.PriorityName != "" {
		detail += " at " + data.PriorityName + " priority"
	}
	return detail
}

func stopRunPickerDestination(destination string) bool {
	for _, value := range []string{"Blocked", "Backlog", "Cancelled", "Todo"} {
		if strings.EqualFold(strings.TrimSpace(destination), value) {
			return true
		}
	}
	return false
}
