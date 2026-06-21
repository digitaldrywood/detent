package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const planImplementationState = "In Progress"

func (o *Orchestrator) reviewPlanIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	cfg := gate.EffectivePlan(o.cfg.Plan)
	if !cfg.Enabled {
		return nil
	}

	transitioned := map[string]struct{}{}
	for _, issue := range issuesInStates(issues, []string{cfg.Stop}) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}

		summary := planReviewSummaryFromIssue(issue)
		approvalLabel := gate.Effective(o.cfg.AutoPromote.Gate).ApprovalLabel
		decision := gate.EvaluatePlan(cfg, approvalLabel, issue.Labels, summary)
		targetState := planReviewTargetState(decision.Action)
		if targetState == "" {
			o.logPlanReviewDecision(issue, decision, "")
			continue
		}
		if !o.applyPlanReviewDecision(ctx, state, issue, summary, decision, targetState, now) {
			continue
		}
		transitioned[issueID] = struct{}{}
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func planReviewSummaryFromIssue(issue connector.Issue) gate.Summary {
	summary := gate.Summary{}
	if issue.PullRequest == nil || normalizePullRequestState(issue.PullRequest.State) != "open" {
		return summary
	}
	summary.ReviewState = issue.PullRequest.CodexReviewState
	summary.P1Findings = planReviewFindingsFromPullRequest(issue.PullRequest)
	return summary
}

func planReviewFindingsFromPullRequest(pullRequest *connector.PullRequest) []gate.Finding {
	if pullRequest == nil {
		return nil
	}
	findings := make([]gate.Finding, 0, len(pullRequest.CodexReviewFindings))
	for _, finding := range pullRequest.CodexReviewFindings {
		findings = append(findings, gate.Finding{
			Body: finding.Body,
			URL:  finding.URL,
			Path: finding.Path,
			Line: finding.Line,
		})
	}
	if len(findings) == 0 && strings.EqualFold(strings.TrimSpace(pullRequest.CodexReviewState), "P1") {
		findings = append(findings, gate.Finding{
			Body: "Automated plan review reported P1 findings.",
			URL:  strings.TrimSpace(pullRequest.URL),
		})
	}
	return findings
}

func planReviewTargetState(action gate.Action) string {
	switch action {
	case gate.ActionPass:
		return planImplementationState
	case gate.ActionRework:
		return autoPromoteReworkState
	default:
		return ""
	}
}

func (o *Orchestrator) applyPlanReviewDecision(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	summary gate.Summary,
	decision gate.Decision,
	targetState string,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	if err := o.connector.UpdateIssueState(ctx, issueID, targetState); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"plan review transition failed",
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

	body := planReviewComment(gate.EffectivePlan(o.cfg.Plan).Stop, summary, decision, targetState)
	if strings.TrimSpace(body) != "" {
		if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
			o.logger.Warn(
				"plan review comment failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"action", decision.Action,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
	}

	o.logPlanReviewDecision(issue, decision, targetState)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "plan_review_transition",
		Message: "plan review moved " + issueLabel(issue) + " from " + gate.EffectivePlan(o.cfg.Plan).Stop + " to " + targetState,
	})
	return true
}

func (o *Orchestrator) logPlanReviewDecision(issue connector.Issue, decision gate.Decision, targetState string) {
	if o.logger == nil {
		return
	}

	attrs := []any{
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", issue.Identifier,
		"action", decision.Action,
		"reason", decision.Reason,
	}
	if targetState != "" {
		attrs = append(attrs, "target_state", targetState)
		o.logger.Info("plan review decision", attrs...)
		return
	}
	o.logger.Debug("plan review decision", attrs...)
}

func planReviewComment(stop string, summary gate.Summary, decision gate.Decision, targetState string) string {
	if targetState != autoPromoteReworkState {
		return ""
	}

	var b strings.Builder
	b.WriteString("Plan review routed this issue from ")
	b.WriteString(strings.TrimSpace(stop))
	b.WriteString(" to Rework.")
	b.WriteString("\n\n- reason: ")
	b.WriteString(string(decision.Reason))
	if summary.ReviewState != "" {
		b.WriteString("\n- review_state: ")
		b.WriteString(summary.ReviewState)
	}
	if len(decision.Findings) > 0 {
		b.WriteString("\n\nFindings:")
		for _, finding := range decision.Findings {
			b.WriteString("\n- ")
			b.WriteString(planReviewFindingText(finding))
		}
	}
	return b.String()
}

func planReviewFindingText(finding gate.Finding) string {
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

func planArtifactComment(issue connector.Issue, output string) string {
	var b strings.Builder
	b.WriteString("## Detent Plan")
	if strings.TrimSpace(issue.Identifier) != "" {
		b.WriteString("\n\n- issue: ")
		b.WriteString(strings.TrimSpace(issue.Identifier))
	}
	if strings.TrimSpace(issue.Title) != "" {
		b.WriteString("\n- title: ")
		b.WriteString(strings.TrimSpace(issue.Title))
	}
	b.WriteString("\n\n### Artifact\n\n")
	output = strings.TrimSpace(output)
	if output == "" {
		output = "The plan run completed without assistant text."
	}
	b.WriteString(output)
	b.WriteString("\n")
	return b.String()
}
