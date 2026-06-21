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
const planReviewCommentHeading = "detent plan review"

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
		decision := gate.EvaluatePlan(cfg, issue.Labels, summary)
		targetState := planReviewTargetState(decision.Action)
		if targetState == "" {
			o.logPlanReviewDecision(issue, decision, "")
			continue
		}
		if !o.applyPlanReviewDecision(ctx, state, issue, summary, decision, targetState, now) {
			continue
		}
		o.trackPlanReviewTransition(state, issueID, targetState)
		transitioned[issueID] = struct{}{}
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func planReviewSummaryFromIssue(issue connector.Issue) gate.Summary {
	summary := planReviewSummaryFromComments(issue.Comments)
	if issue.PullRequest == nil || normalizePullRequestState(issue.PullRequest.State) != "open" {
		return summary
	}
	return mergePlanReviewSummaries(summary, gate.Summary{
		ReviewState: issue.PullRequest.CodexReviewState,
		P1Findings:  planReviewFindingsFromPullRequest(issue.PullRequest),
	})
}

func mergePlanReviewSummaries(left gate.Summary, right gate.Summary) gate.Summary {
	out := left
	if planReviewStateSeverity(right.ReviewState) > planReviewStateSeverity(out.ReviewState) {
		out.ReviewState = right.ReviewState
	}
	out.P1Findings = append(out.P1Findings, right.P1Findings...)
	return out
}

func planReviewSummaryFromComments(comments []connector.IssueComment) gate.Summary {
	for index := len(comments) - 1; index >= 0; index-- {
		comment := comments[index]
		body := strings.TrimSpace(comment.Body)
		if !planReviewArtifact(body) {
			continue
		}
		state := planReviewCommentState(body)
		summary := gate.Summary{ReviewState: state}
		if automatedReviewHasPlanP1(state, body) {
			summary.ReviewState = "P1"
			summary.P1Findings = []gate.Finding{{
				Body: planReviewCommentFindingBody(body),
				URL:  strings.TrimSpace(comment.URL),
			}}
		}
		return summary
	}
	return gate.Summary{}
}

func planReviewArtifact(body string) bool {
	for line := range strings.SplitSeq(body, "\n") {
		heading, ok := planReviewMarkdownHeadingTitle(line)
		if !ok {
			continue
		}
		return normalizeState(heading) == normalizeState(planReviewCommentHeading)
	}
	return false
}

func planReviewCommentState(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch normalizeState(key) {
		case "state", "reviewstate", "review_state", "result":
			if state := normalizePlanReviewCommentState(value); state != "" {
				return state
			}
		}
	}
	if containsReviewSeverity(body, "P1") {
		return "P1"
	}
	return ""
}

func normalizePlanReviewCommentState(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "approved", "approve", "pass", "passed", "ready":
		return "APPROVED"
	case "commented":
		return "COMMENTED"
	case "p1", "priority 1", "priority-1":
		return "P1"
	case "p2", "priority 2", "priority-2", "concerns":
		return "P2"
	case "changes_requested", "requested_changes", "request changes", "changes requested":
		return "CHANGES_REQUESTED"
	default:
		return strings.ToUpper(strings.TrimSpace(value))
	}
}

func automatedReviewHasPlanP1(state string, body string) bool {
	return strings.EqualFold(strings.TrimSpace(state), "P1") || containsReviewSeverity(body, "P1")
}

func planReviewCommentFindingBody(body string) string {
	if findings := planReviewMarkdownSectionText(body, "Findings"); findings != "" {
		return findings
	}
	return body
}

func planReviewStateSeverity(state string) int {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "P1":
		return 4
	case "CHANGES_REQUESTED", "REQUESTED_CHANGES":
		return 3
	case "P2":
		return 2
	case "APPROVED", "COMMENTED":
		return 1
	default:
		return 0
	}
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

func (o *Orchestrator) hydratePlanIssueComments(ctx context.Context, fetched *tickFetchedIssues) bool {
	cfg := gate.EffectivePlan(o.cfg.Plan)
	if !cfg.Enabled {
		return true
	}
	reader, ok := o.connector.(connector.IssueCommentReader)
	if !ok {
		return true
	}
	return o.hydratePlanIssueCommentsFor(ctx, reader, fetched.status, cfg) &&
		o.hydratePlanIssueCommentsFor(ctx, reader, fetched.candidates, cfg)
}

func (o *Orchestrator) hydratePlanIssueCommentsFor(
	ctx context.Context,
	reader connector.IssueCommentReader,
	issues []connector.Issue,
	cfg gate.PlanConfig,
) bool {
	for index := range issues {
		if !planCommentHydrationCandidate(cfg, issues[index]) || len(issues[index].Comments) > 0 {
			continue
		}
		comments, err := reader.FetchIssueComments(ctx, issues[index])
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("fetch plan issue comments failed", "issue_id", issues[index].ID, "identifier", issues[index].Identifier, "error", err)
			}
			return false
		}
		issues[index].Comments = comments
	}
	return true
}

func planCommentHydrationCandidate(cfg gate.PlanConfig, issue connector.Issue) bool {
	state := normalizeState(issue.State)
	return state == normalizeState(cfg.Stop) || state == normalizeState(autoPromoteReworkState)
}

func (o *Orchestrator) trackPlanReviewTransition(state *State, issueID string, targetState string) {
	if state.planRework == nil {
		state.planRework = map[string]struct{}{}
	}
	if normalizeState(targetState) == normalizeState(autoPromoteReworkState) {
		state.planRework[issueID] = struct{}{}
		return
	}
	delete(state.planRework, issueID)
}

func planReviewReworkRequested(issue connector.Issue) bool {
	for _, comment := range issue.Comments {
		body := strings.ToLower(comment.Body)
		if strings.Contains(body, "plan review routed this issue") && strings.Contains(body, " to rework") {
			return true
		}
	}
	return false
}

func planReviewMarkdownSectionText(body string, title string) string {
	want := normalizeState(title)
	inSection := false
	lines := []string{}
	for line := range strings.SplitSeq(body, "\n") {
		heading, ok := planReviewMarkdownHeadingTitle(line)
		if ok {
			if inSection {
				break
			}
			inSection = normalizeState(heading) == want
			continue
		}
		if inSection {
			lines = append(lines, line)
		}
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func planReviewMarkdownHeadingTitle(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "#") {
		return "", false
	}
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == len(line) || line[level] != ' ' {
		return "", false
	}
	return strings.TrimSpace(line[level+1:]), true
}

func containsReviewSeverity(body string, severity string) bool {
	body = strings.ToUpper(body)
	severity = strings.ToUpper(strings.TrimSpace(severity))
	if severity == "" {
		return false
	}
	return strings.Contains(body, "["+severity+"]") ||
		strings.Contains(body, severity+" BADGE") ||
		strings.Contains(body, severity+" FINDING") ||
		strings.Contains(body, " "+severity+" ")
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
