package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func checkDoctorStrandedActive(ctx context.Context, boot BootConfig, projectID string, deps doctorDeps) doctorCheck {
	check := doctorCheck{
		Name:   "Stranded active work",
		Status: doctorOK,
		Detail: "no active issues are stranded without a live worker",
	}
	probe, err := probeDoctorHealth(ctx, doctorLiveBoot(boot, &boot.Global), deps)
	if err != nil {
		check.Detail = "live stranded-work check skipped because no healthy Detent instance was reachable"
		return check
	}
	projectID = strings.TrimSpace(projectID)
	issues := make([]telemetry.StrandedIssue, 0, len(probe.Health.StrandedIssues))
	for _, issue := range probe.Health.StrandedIssues {
		if projectID == "" || strings.TrimSpace(issue.ProjectID) == projectID {
			issues = append(issues, issue)
		}
	}
	if len(issues) == 0 {
		return check
	}
	check.Status = doctorWarn
	check.StrandedIssues = issues
	details := make([]string, 0, len(issues))
	for _, issue := range issues {
		details = append(details, doctorStrandedActiveDetail(issue))
	}
	check.Detail = fmt.Sprintf("%d active issue(s) without a live worker: %s", len(issues), strings.Join(details, "; "))
	check.Hint = "Review the last dispatch refusal and project capacity, then rerun detent doctor after dispatch resumes."
	return check
}

func doctorStrandedActiveDetail(issue telemetry.StrandedIssue) string {
	target := strings.TrimSpace(issue.Identifier)
	if target == "" {
		target = strings.TrimSpace(issue.IssueID)
	}
	if target == "" {
		target = strings.TrimSpace(issue.IssueURL)
	}
	if target == "" {
		target = "issue"
	}
	reason := strings.TrimSpace(issue.LastRefusalReason)
	if reason == "" {
		reason = "none recorded"
	}
	return target + " stranded for " + (time.Duration(issue.DurationSeconds) * time.Second).String() + "; last refusal: " + reason
}
