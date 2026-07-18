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
	return stopRunDialogPath(projectID, issueID, running.Attempt, running.WorkAttemptID, running.DetentSessionID, running.SessionID)
}

func StopRunRetryDialogPath(blocked telemetry.Blocked, fallbackProjectID string) string {
	if !operatorStopTransitionFailed(blocked) {
		return ""
	}
	projectID := strings.TrimSpace(blocked.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(fallbackProjectID)
	}
	return stopRunDialogPath(projectID, strings.TrimSpace(blocked.ID), blocked.Attempt, blocked.WorkAttemptID, blocked.DetentSessionID, blocked.SessionID)
}

func stopRunDialogPath(projectID string, issueID string, attempt int, workAttemptID int64, detentSessionID int64, providerSessionID string) string {
	if projectID == "" || issueID == "" {
		return ""
	}
	values := url.Values{}
	values.Set("issue_id", issueID)
	if workAttemptID > 0 {
		values.Set("work_attempt_id", strconv.FormatInt(workAttemptID, 10))
	}
	if detentSessionID > 0 {
		values.Set("detent_session_id", strconv.FormatInt(detentSessionID, 10))
	}
	if sessionID := strings.TrimSpace(providerSessionID); sessionID != "" {
		values.Set("provider_session_id", sessionID)
	}
	return "/api/v1/projects/" + url.PathEscape(projectID) + "/runs/" + strconv.Itoa(attempt) + "/stop?" + values.Encode()
}

func operatorStopTransitionFailed(blocked telemetry.Blocked) bool {
	if blocked.Source != telemetry.BlockedSourceOperatorStop {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(blocked.RecoveryReason), "transition_failed") {
		return true
	}
	return strings.Contains(strings.ToLower(strings.TrimSpace(blocked.Error)), "retry the transition")
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
