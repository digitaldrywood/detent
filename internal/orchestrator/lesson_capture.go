package orchestrator

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/lessons"
)

const reworkTransitionFailureKind = "rework_transition"

func (o *Orchestrator) captureReworkLesson(issue connector.Issue, at time.Time, reason string) {
	if o == nil || !o.cfg.Lessons.Enabled || strings.TrimSpace(o.cfg.Lessons.Path) == "" {
		return
	}
	if at.IsZero() {
		if o.now != nil {
			at = o.now()
		} else {
			at = time.Now()
		}
	}
	entry := reworkLessonEntry(o.workflowMetricsProjectID(), issue, at.UTC(), reason)
	appended, err := lessons.AppendUnique(o.cfg.Lessons.Path, entry, lessons.AppendOptions{
		Date:       at.UTC(),
		MaxEntries: o.cfg.Lessons.MaxEntries,
	})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn("rework lesson capture failed", "project_id", o.workflowMetricsProjectID(), "issue_id", issue.ID, "identifier", issue.Identifier, "reason", reason, "error", err)
		}
		return
	}
	if appended && o.logger != nil {
		o.logger.Info("rework lesson captured", "project_id", o.workflowMetricsProjectID(), "issue_id", issue.ID, "identifier", issue.Identifier, "failure_kind", entry.FailureKind)
	}
}

func reworkLessonEntry(projectID string, issue connector.Issue, at time.Time, reason string) lessons.Entry {
	failedChecks := autoPromoteFailedChecksFromPullRequest(issue.PullRequest)
	reviewSummary := reworkReviewSummary(issue.PullRequest)
	return lessons.Entry{
		IssueNumber: reworkIssueNumber(issue),
		IssueRef:    strings.TrimSpace(issue.Identifier),
		PullRequest: reworkPullRequestRef(issue),
		Title:       issue.Title,
		FailureKind: reworkFailureKind(reason, issue.PullRequest, failedChecks),
		Symptom:     reworkLessonSymptom(issue, reason, failedChecks, reviewSummary),
		Hypothesis:  "the previous attempt did not satisfy the workflow gate or review expectations",
		Hint:        "use the captured checks and review context to strengthen the next attempt and prevent the same bounce",
		CaptureKey:  reworkLessonCaptureKey(projectID, issue, at),
	}
}

func reworkIssueNumber(issue connector.Issue) string {
	if issue.Number <= 0 {
		return ""
	}
	return strconv.Itoa(issue.Number)
}

func reworkFailureKind(reason string, pullRequest *connector.PullRequest, failedChecks []string) string {
	if len(failedChecks) > 0 {
		return "ci_failure"
	}
	reason = strings.ToLower(strings.TrimSpace(reason))
	switch reason {
	case string(AutoPromoteReasonCINotGreen):
		return "ci_failure"
	case mergeWorkerRequiredChecksMissingReason, mergeWorkerFastPathNotReadyReason:
		return "ci_signal_missing"
	case string(AutoPromoteReasonP1Findings), "plan_review_decision":
		return "changes_requested"
	case string(AutoPromoteReasonValidatorRework), string(AutoPromoteReasonValidatorScoreBelowThreshold), string(AutoPromoteReasonValidatorBlockedSeverity):
		return "validator_rework"
	case string(AutoPromoteReasonArtifactStatusRework):
		return "artifact_rework"
	case string(AutoPromoteReasonMergeConflicts):
		return "merge_conflict"
	case string(AutoPromoteReasonWorkpadStatusInvalid):
		return "invalid_workpad_status"
	}
	if pullRequest != nil {
		reviewState := strings.ToUpper(strings.TrimSpace(pullRequest.CodexReviewState))
		if reviewState == "CHANGES_REQUESTED" || reviewState == "REQUESTED_CHANGES" || reviewState == "P1" {
			return "changes_requested"
		}
	}
	if reason == "" || reason == "tracker_state_observed" {
		return reworkTransitionFailureKind
	}
	return strings.ReplaceAll(reason, " ", "_")
}

func reworkLessonSymptom(issue connector.Issue, reason string, failedChecks []string, reviewSummary string) string {
	transition := reworkIssueRef(issue) + " was observed entering Rework"
	if sourceState := displayStateName(issue.State); sourceState != "" {
		transition = fmt.Sprintf("%s entered Rework from %s", reworkIssueRef(issue), sourceState)
	}
	parts := []string{transition}
	if reason = strings.TrimSpace(reason); reason != "" {
		parts = append(parts, "reason: "+reason)
	}
	if len(failedChecks) > 0 {
		parts = append(parts, "failed checks: "+strings.Join(failedChecks, ", "))
	}
	if reviewSummary != "" {
		parts = append(parts, "review: "+reviewSummary)
	}
	return strings.Join(parts, "; ")
}

func reworkReviewSummary(pullRequest *connector.PullRequest) string {
	if pullRequest == nil {
		return ""
	}
	parts := make([]string, 0, len(pullRequest.CodexReviewFindings)+1)
	if state := strings.TrimSpace(pullRequest.CodexReviewState); state != "" {
		parts = append(parts, state)
	}
	for _, finding := range pullRequest.CodexReviewFindings {
		if body := strings.Join(strings.Fields(finding.Body), " "); body != "" {
			parts = append(parts, body)
		}
		if len(parts) == 4 {
			break
		}
	}
	return strings.Join(parts, ": ")
}

func reworkPullRequestRef(issue connector.Issue) string {
	if issue.PullRequest != nil {
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			return url
		}
		if issue.PullRequest.Number > 0 {
			return fmt.Sprintf("PR #%d", issue.PullRequest.Number)
		}
	}
	if issue.PRNumber != nil && *issue.PRNumber > 0 {
		return fmt.Sprintf("PR #%d", *issue.PRNumber)
	}
	return ""
}

func reworkIssueRef(issue connector.Issue) string {
	if identifier := strings.TrimSpace(issue.Identifier); identifier != "" {
		return identifier
	}
	if issueID := strings.TrimSpace(issue.ID); issueID != "" {
		return issueID
	}
	return "issue"
}

func reworkLessonCaptureKey(projectID string, issue connector.Issue, at time.Time) string {
	return strings.Join([]string{
		"rework",
		strings.TrimSpace(projectID),
		reworkIssueRef(issue),
		at.UTC().Format(time.RFC3339Nano),
	}, "|")
}
