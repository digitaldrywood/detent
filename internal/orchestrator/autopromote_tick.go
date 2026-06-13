package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	autoPromoteSourceState  = "Human Review"
	autoPromoteMergingState = "Merging"
	autoPromoteReworkState  = "Rework"
)

func (o *Orchestrator) autoPromoteHumanReviewIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	if !cfg.Enabled {
		if o.logger != nil {
			o.logger.Debug("auto promote skipped", "reason", AutoPromoteReasonDisabled)
		}
		return nil
	}

	transitioned := map[string]struct{}{}
	for _, issue := range issuesInStates(issues, []string{autoPromoteSourceState}) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}

		summary := AutoPromoteSummaryFromIssue(issue)
		decision := EvaluateAutoPromote(issue, summary, cfg, now)
		targetState := autoPromoteTargetState(decision.Action)
		if targetState == "" {
			o.logAutoPromoteDecision(issue, decision, "")
			continue
		}
		if !o.applyAutoPromoteDecision(ctx, state, issue, summary, decision, targetState, now) {
			continue
		}
		transitioned[issueID] = struct{}{}
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func AutoPromoteSummaryFromIssue(issue connector.Issue) AutoPromoteSummary {
	summary := AutoPromoteSummary{
		LastActivityAt: autoPromoteLastActivityAt(issue),
	}
	if issue.PullRequest == nil {
		return summary
	}

	pullRequest := issue.PullRequest
	if normalizePullRequestState(pullRequest.State) != "open" {
		return summary
	}
	summary.PullRequestPresent = true
	summary.PullRequestURL = strings.TrimSpace(pullRequest.URL)
	summary.CIStatus = pullRequest.CIStatus
	summary.ReviewState = pullRequest.CodexReviewState
	summary.P1Findings = autoPromoteFindingsFromPullRequest(pullRequest)
	return summary
}

func autoPromoteLastActivityAt(issue connector.Issue) *time.Time {
	var latest *time.Time
	latest = latestTime(latest, issue.StageUpdatedAt)
	latest = latestTime(latest, issue.UpdatedAt)
	if issue.PullRequest != nil {
		latest = latestTime(latest, issue.PullRequest.CodexReviewSubmittedAt)
	}
	return latest
}

func latestTime(current *time.Time, candidate *time.Time) *time.Time {
	if candidate == nil {
		return current
	}
	if current == nil || candidate.After(*current) {
		value := *candidate
		return &value
	}
	return current
}

func autoPromoteFindingsFromPullRequest(pullRequest *connector.PullRequest) []AutoPromoteFinding {
	if pullRequest == nil {
		return nil
	}
	findings := make([]AutoPromoteFinding, 0, len(pullRequest.CodexReviewFindings))
	for _, finding := range pullRequest.CodexReviewFindings {
		findings = append(findings, AutoPromoteFinding{
			Body: finding.Body,
			URL:  finding.URL,
			Path: finding.Path,
			Line: finding.Line,
		})
	}
	if len(findings) == 0 && strings.EqualFold(strings.TrimSpace(pullRequest.CodexReviewState), "P1") {
		findings = append(findings, AutoPromoteFinding{
			Body: "Codex review reported P1 findings.",
			URL:  strings.TrimSpace(pullRequest.URL),
		})
	}
	return findings
}

func autoPromoteTargetState(action AutoPromoteAction) string {
	switch action {
	case AutoPromoteActionPromote:
		return autoPromoteMergingState
	case AutoPromoteActionRework:
		return autoPromoteReworkState
	default:
		return ""
	}
}

func (o *Orchestrator) applyAutoPromoteDecision(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	targetState string,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	if err := o.connector.UpdateIssueState(ctx, issueID, targetState); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"auto promote transition failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"action", decision.Action,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
		return false
	}

	body := autoPromoteComment(summary, decision, targetState)
	if strings.TrimSpace(body) != "" {
		if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
			o.logger.Warn(
				"auto promote comment failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"action", decision.Action,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
	}

	o.logAutoPromoteDecision(issue, decision, targetState)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "auto_promote_transition",
		Message: "auto-promoted " + issueLabel(issue) + " from " + autoPromoteSourceState + " to " + targetState,
	})
	return true
}

func (o *Orchestrator) logAutoPromoteDecision(issue connector.Issue, decision AutoPromoteDecision, targetState string) {
	if o.logger == nil {
		return
	}

	attrs := []any{
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", issue.Identifier,
		"action", decision.Action,
		"reason", decision.Reason,
	}
	if decision.CIStatus != "" {
		attrs = append(attrs, "ci_status", decision.CIStatus)
	}
	if decision.QuietRemaining > 0 {
		attrs = append(attrs, "quiet_remaining", decision.QuietRemaining)
	}
	if targetState != "" {
		attrs = append(attrs, "target_state", targetState)
		o.logger.Info("auto promote decision", attrs...)
		return
	}
	o.logger.Debug("auto promote decision", attrs...)
}

func autoPromoteComment(
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	targetState string,
) string {
	var b strings.Builder
	switch targetState {
	case autoPromoteMergingState:
		b.WriteString("Auto-promoted this issue from Human Review to Merging.")
	case autoPromoteReworkState:
		b.WriteString("Auto-promote routed this issue from Human Review to Rework.")
	default:
		return ""
	}

	b.WriteString("\n\n")
	b.WriteString("- reason: ")
	b.WriteString(string(decision.Reason))
	if summary.PullRequestURL != "" {
		b.WriteString("\n- pull request: ")
		b.WriteString(summary.PullRequestURL)
	}
	if decision.CIStatus != "" {
		b.WriteString("\n- ci_status: ")
		b.WriteString(decision.CIStatus)
	}

	if len(decision.Findings) > 0 {
		b.WriteString("\n\nFindings:")
		for _, finding := range decision.Findings {
			b.WriteString("\n- ")
			b.WriteString(autoPromoteFindingText(finding))
		}
	}

	return b.String()
}

func autoPromoteFindingText(finding AutoPromoteFinding) string {
	body := strings.Join(strings.Fields(finding.Body), " ")
	if body == "" {
		body = "P1 finding"
	}
	if finding.Path != "" && finding.Line > 0 {
		body = fmt.Sprintf("%s (%s:%d)", body, finding.Path, finding.Line)
	} else if finding.Path != "" {
		body = body + " (" + finding.Path + ")"
	}
	if finding.URL != "" {
		body = body + " " + finding.URL
	}
	return body
}
