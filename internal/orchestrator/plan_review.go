package orchestrator

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/reviewseverity"
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

		evaluation := planReviewEvaluationFromIssue(issue)
		decision := gate.EvaluatePlan(cfg, issue.Labels, evaluation.Summary)
		targetState := planReviewTargetState(decision.Action)
		if targetState == "" {
			o.logPlanReviewDecision(issue, decision, "")
			continue
		}
		reworkSignature := ""
		if normalizeState(targetState) == normalizeState(autoPromoteReworkState) {
			reworkSignature = evaluation.Signature
			if match, ok := o.workflowTimelineLaneActionSignature(ctx, issue, "plan_review_decision", workflowActionPlanReviewRework, reworkSignature); ok {
				o.logPlanReviewReworkSkip(issue, decision, targetState, reworkSignature, match)
				continue
			}
		}
		if !o.applyPlanReviewDecision(ctx, state, issue, evaluation.Summary, decision, targetState, reworkSignature, now) {
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

type planReviewEvaluation struct {
	Summary   gate.Summary
	Signature string
}

func planReviewEvaluationFromIssue(issue connector.Issue) planReviewEvaluation {
	evaluation := planReviewEvaluationFromComments(issue.Comments)
	if issue.PullRequest == nil || normalizePullRequestState(issue.PullRequest.State) != "open" {
		return evaluation
	}
	prEvaluation := planReviewEvaluation{
		Summary: gate.Summary{
			ReviewState: issue.PullRequest.CodexReviewState,
			P1Findings:  planReviewFindingsFromPullRequest(issue.PullRequest),
		},
		Signature: planReviewPullRequestSignature(issue.PullRequest),
	}
	if planReviewStateSeverity(prEvaluation.Summary.ReviewState) > planReviewStateSeverity(evaluation.Summary.ReviewState) {
		prEvaluation.Summary.P1Findings = append(evaluation.Summary.P1Findings, prEvaluation.Summary.P1Findings...)
		return prEvaluation
	}
	evaluation.Summary.P1Findings = append(evaluation.Summary.P1Findings, prEvaluation.Summary.P1Findings...)
	if evaluation.Signature == "" {
		evaluation.Signature = prEvaluation.Signature
	}
	return evaluation
}

func planReviewEvaluationFromComments(comments []connector.IssueComment) planReviewEvaluation {
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
		return planReviewEvaluation{
			Summary:   summary,
			Signature: planReviewCommentSignature(comment),
		}
	}
	return planReviewEvaluation{}
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

func planReviewPullRequestSignature(pullRequest *connector.PullRequest) string {
	if pullRequest == nil {
		return ""
	}
	reviewKey := strings.TrimSpace(pullRequest.LatestCodexReviewCommitSHA)
	if reviewKey == "" {
		reviewKey = strings.TrimSpace(pullRequest.HeadSHA)
	}
	if reviewKey == "" && pullRequest.CodexReviewSubmittedAt != nil {
		reviewKey = pullRequest.CodexReviewSubmittedAt.UTC().Format(time.RFC3339Nano)
	}
	if reviewKey == "" && pullRequest.LatestCodexReviewSubmittedAt != nil {
		reviewKey = pullRequest.LatestCodexReviewSubmittedAt.UTC().Format(time.RFC3339Nano)
	}
	if reviewKey == "" {
		reviewKey = strings.TrimSpace(pullRequest.CodexReviewState)
	}
	if reviewKey == "" {
		return ""
	}
	return fmt.Sprintf("pr=%d;review=%s", pullRequest.Number, reviewKey)
}

func planReviewCommentSignature(comment connector.IssueComment) string {
	if id := strings.TrimSpace(comment.ID); id != "" {
		return "comment_id=" + id
	}
	if url := strings.TrimSpace(comment.URL); url != "" {
		return "comment_url=" + url
	}
	bodyHash := sha256.Sum256([]byte(strings.TrimSpace(comment.Body)))
	return fmt.Sprintf("comment_sha256=%x", bodyHash)
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

func (o *Orchestrator) hydratePlanIssueComments(ctx context.Context, fetched *tickFetchedIssues) error {
	cfg := gate.EffectivePlan(o.cfg.Plan)
	if !cfg.Enabled {
		return nil
	}
	reader, ok := o.connector.(connector.IssueCommentReader)
	if !ok {
		return nil
	}
	if err := o.hydratePlanIssueCommentsFor(ctx, reader, fetched.status, cfg); err != nil {
		return err
	}
	return o.hydratePlanIssueCommentsFor(ctx, reader, fetched.candidates, cfg)
}

func (o *Orchestrator) hydratePlanIssueCommentsFor(
	ctx context.Context,
	reader connector.IssueCommentReader,
	issues []connector.Issue,
	cfg gate.PlanConfig,
) error {
	for index := range issues {
		if !planCommentHydrationCandidate(cfg, issues[index]) || len(issues[index].Comments) > 0 {
			continue
		}
		comments, err := reader.FetchIssueComments(ctx, issues[index])
		if err != nil {
			if o.logger != nil {
				o.logger.Warn("fetch plan issue comments failed", "issue_id", issues[index].ID, "identifier", issues[index].Identifier, "error", err)
			}
			return err
		}
		issues[index].Comments = comments
	}
	return nil
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
	return reviewseverity.Contains(body, severity)
}

func (o *Orchestrator) applyPlanReviewDecision(
	ctx context.Context,
	state *State,
	issue connector.Issue,
	summary gate.Summary,
	decision gate.Decision,
	targetState string,
	reworkSignature string,
	now time.Time,
) bool {
	issueID := strings.TrimSpace(issue.ID)
	metadata := workflowLaneMetadata{}
	if normalizeState(targetState) == normalizeState(autoPromoteReworkState) {
		metadata = workflowLaneMetadataWithActionSignature(metadata, workflowActionPlanReviewRework, reworkSignature)
	}
	if err := o.updateIssueStateByIDWithMetadata(ctx, state, issueID, issue, targetState, now, "plan_review_decision", metadata, laneMutationRevokeWorker); err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"plan review transition failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"action", decision.Action,
				"reason", decision.Reason,
				"target_state", targetState,
				"signature", reworkSignature,
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
	attrs = appendPullRequestReviewDisagreementAttrs(attrs, issue.PullRequest)
	if targetState != "" {
		attrs = append(attrs, "target_state", targetState)
		o.logger.Info("plan review decision", attrs...)
		return
	}
	o.logger.Debug("plan review decision", attrs...)
}

func appendPullRequestReviewDisagreementAttrs(attrs []any, pullRequest *connector.PullRequest) []any {
	if pullRequest == nil {
		return attrs
	}
	apiState := strings.ToUpper(strings.TrimSpace(pullRequest.CodexReviewAPIState))
	bodySeverity := strings.ToUpper(strings.TrimSpace(pullRequest.CodexReviewBodySeverity))
	if apiState == "" || bodySeverity == "" || apiState == bodySeverity {
		return attrs
	}
	return append(attrs,
		"review_api_state", apiState,
		"review_body_severity", bodySeverity,
	)
}

func (o *Orchestrator) logPlanReviewReworkSkip(
	issue connector.Issue,
	decision gate.Decision,
	targetState string,
	signature string,
	match workflowTimelineMetadataMatch,
) {
	if o.logger == nil {
		return
	}
	o.logger.Info(
		"plan review rework already routed",
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", issue.Identifier,
		"action", decision.Action,
		"reason", decision.Reason,
		"target_state", targetState,
		"signature", signature,
		"matched_event_id", match.Event.ID,
		"matched_event_reason", match.Event.Reason,
		"matched_event_phase", match.Event.PhaseName,
	)
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
