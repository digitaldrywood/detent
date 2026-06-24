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

const (
	autoPromoteSourceState  = "Human Review"
	autoPromoteMergingState = "Merging"
	autoPromoteReworkState  = "Rework"
)

type autoPromoteTickResult struct {
	transitioned map[string]struct{}
}

func (o *Orchestrator) autoPromoteHumanReviewIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) autoPromoteTickResult {
	cfg := normalizeAutoPromoteConfig(o.cfg.AutoPromote)
	if !cfg.Enabled {
		if o.logger != nil {
			o.logger.Debug("auto promote skipped", "reason", AutoPromoteReasonDisabled)
		}
		return autoPromoteTickResult{}
	}

	result := autoPromoteTickResult{transitioned: map[string]struct{}{}}
	for _, issue := range issuesInStates(issues, []string{autoPromoteSourceState}) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" {
			continue
		}

		summary := AutoPromoteSummaryFromIssue(issue)
		decision := EvaluateAutoPromote(issue, summary, cfg, now)
		if decision.Reason == AutoPromoteReasonValidatorMissing {
			validation, shouldComment, ok := o.validatorStageResult(issue)
			if !ok {
				o.startValidatorStage(ctx, issue, now)
				o.logAutoPromoteDecision(issue, decision, "")
				continue
			}
			summary.Validator = validation
			if shouldComment {
				o.commentValidatorResult(ctx, issue, validation)
				o.markValidatorResultCommented(issue)
			}
			decision = EvaluateAutoPromote(issue, summary, cfg, now)
		}
		targetState := autoPromoteTargetState(decision.Action)
		if targetState == "" {
			o.logAutoPromoteDecision(issue, decision, "")
			continue
		}
		if !o.applyAutoPromoteDecision(ctx, state, issue, summary, decision, targetState, now) {
			continue
		}
		result.transitioned[issueID] = struct{}{}
		o.clearAutoPromotedIssueDispatchMemory(state, issueID)
	}
	if len(result.transitioned) == 0 {
		return autoPromoteTickResult{}
	}
	return result
}

func (o *Orchestrator) reconcileStaleTodoPullRequestIssues(
	ctx context.Context,
	state *State,
	issues []connector.Issue,
	now time.Time,
) map[string]struct{} {
	transitioned := map[string]struct{}{}
	for _, issue := range issuesInStates(issues, []string{"Todo"}) {
		issueID := strings.TrimSpace(issue.ID)
		if issueID == "" || issue.PullRequest == nil || normalizePullRequestState(issue.PullRequest.State) != "open" {
			continue
		}
		if staleTodoPullRequestAlreadyActive(state, issueID) {
			continue
		}

		summary := AutoPromoteSummaryFromIssue(issue)
		if !summary.PullRequestPresent {
			continue
		}
		decision := staleTodoPullRequestDecision(issue, summary, o.cfg.AutoPromote, now)
		targetState := staleTodoPullRequestTargetState(decision)
		if targetState == "" {
			o.logAutoPromoteDecision(issue, decision, "")
			continue
		}
		if !o.applyStaleTodoPullRequestDecision(ctx, state, issue, summary, decision, targetState, now) {
			continue
		}
		transitioned[issueID] = struct{}{}
		o.clearAutoPromotedIssueDispatchMemory(state, issueID)
	}
	if len(transitioned) == 0 {
		return nil
	}
	return transitioned
}

func staleTodoPullRequestAlreadyActive(state *State, issueID string) bool {
	if state == nil {
		return false
	}
	if _, ok := state.Running[issueID]; ok {
		return true
	}
	if _, ok := state.Claimed[issueID]; ok {
		return true
	}
	return false
}

func staleTodoPullRequestDecision(
	issue connector.Issue,
	summary AutoPromoteSummary,
	cfg AutoPromoteConfig,
	now time.Time,
) AutoPromoteDecision {
	if autoPromoteMergeConflicts(summary.MergeableState) {
		return autoPromoteDecision(AutoPromoteActionRework, AutoPromoteReasonMergeConflicts)
	}
	cfg = normalizeAutoPromoteConfig(cfg)
	if !cfg.Enabled {
		return autoPromoteDecision(AutoPromoteActionAwaitReview, AutoPromoteReasonDisabled)
	}
	return EvaluateAutoPromote(issue, summary, cfg, now)
}

func staleTodoPullRequestTargetState(decision AutoPromoteDecision) string {
	if targetState := autoPromoteTargetState(decision.Action); targetState != "" {
		return targetState
	}
	switch decision.Reason {
	case AutoPromoteReasonMissingPullRequest:
		return ""
	default:
		return autoPromoteSourceState
	}
}

func (o *Orchestrator) applyStaleTodoPullRequestDecision(
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
				"stale_todo_pr_reconciliation_failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
		return false
	}

	body := autoPromoteComment(summary, decision, displayStateName(issue.State), targetState)
	if strings.TrimSpace(body) != "" {
		if err := o.connector.CreateComment(ctx, issueID, body); err != nil && o.logger != nil {
			o.logger.Warn(
				"stale_todo_pr_reconciliation_comment_failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"reason", decision.Reason,
				"target_state", targetState,
				"error", err,
			)
		}
	}

	o.logStaleTodoPullRequestDecision(issue, decision, targetState)
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      now,
		Event:   "stale_todo_pr_reconciled",
		Message: "reconciled stale linked PR for " + issueLabel(issue) + " from " + displayStateName(issue.State) + " to " + targetState + ": " + string(decision.Reason),
	})
	return true
}

func (o *Orchestrator) logStaleTodoPullRequestDecision(issue connector.Issue, decision AutoPromoteDecision, targetState string) {
	if o.logger == nil {
		return
	}
	attrs := []any{
		"issue_id", strings.TrimSpace(issue.ID),
		"identifier", issue.Identifier,
		"reason", decision.Reason,
		"target_state", targetState,
	}
	if issue.PullRequest != nil {
		if issue.PullRequest.Number > 0 {
			attrs = append(attrs, "pull_request_number", issue.PullRequest.Number)
		}
		if mergeableState := strings.TrimSpace(issue.PullRequest.MergeableState); mergeableState != "" {
			attrs = append(attrs, "mergeable_state", strings.ToLower(mergeableState))
		}
	}
	o.logger.Info("stale_todo_pr_reconciled", attrs...)
}

func (o *Orchestrator) clearAutoPromotedIssueDispatchMemory(state *State, issueID string) {
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.Blocked, issueID)
}

func (o *Orchestrator) startValidatorStage(ctx context.Context, issue connector.Issue, now time.Time) {
	if o.validator == nil {
		if o.logger != nil {
			o.logger.Warn(
				"validator stage skipped",
				"issue_id", strings.TrimSpace(issue.ID),
				"identifier", issue.Identifier,
				"reason", "validator runner unavailable",
			)
		}
		return
	}

	key := validatorStageKey(issue)
	if key == "" {
		return
	}

	o.validatorMu.Lock()
	if o.validatorRuns == nil {
		o.validatorRuns = map[string]struct{}{}
	}
	if o.validatorResults == nil {
		o.validatorResults = map[string]validatorStageResult{}
	}
	if _, ok := o.validatorRuns[key]; ok {
		o.validatorMu.Unlock()
		return
	}
	if _, ok := o.validatorResults[key]; ok {
		o.validatorMu.Unlock()
		return
	}
	o.validatorRuns[key] = struct{}{}
	o.validatorMu.Unlock()

	selectorContext := o.cfg.SelectorContext
	go func() {
		result, err := o.validator.Validate(ctx, ValidatorRequest{
			Issue:           issue,
			StartedAt:       now.UTC(),
			SelectorContext: selectorContext,
		})

		o.validatorMu.Lock()
		defer o.validatorMu.Unlock()
		delete(o.validatorRuns, key)
		if err != nil {
			if o.logger != nil {
				o.logger.Warn(
					"validator stage failed",
					"issue_id", strings.TrimSpace(issue.ID),
					"identifier", issue.Identifier,
					"error", err,
				)
			}
			return
		}
		o.validatorResults[key] = validatorStageResult{Result: result}
	}()
}

func (o *Orchestrator) validatorStageResult(issue connector.Issue) (gate.ValidatorResult, bool, bool) {
	key := validatorStageKey(issue)
	if key == "" {
		return gate.ValidatorResult{}, false, false
	}
	o.validatorMu.Lock()
	defer o.validatorMu.Unlock()
	result, ok := o.validatorResults[key]
	if !ok {
		return gate.ValidatorResult{}, false, false
	}
	return result.Result, !result.Commented, true
}

func (o *Orchestrator) markValidatorResultCommented(issue connector.Issue) {
	key := validatorStageKey(issue)
	if key == "" {
		return
	}
	o.validatorMu.Lock()
	defer o.validatorMu.Unlock()
	result, ok := o.validatorResults[key]
	if !ok {
		return
	}
	result.Commented = true
	o.validatorResults[key] = result
}

func (o *Orchestrator) commentValidatorResult(ctx context.Context, issue connector.Issue, result gate.ValidatorResult) {
	commenter, ok := o.connector.(connector.PullRequestCommenter)
	if !ok {
		return
	}
	repository := pullRequestRepository(issue)
	number := pullRequestNumber(issue)
	if repository == "" || number <= 0 {
		return
	}
	if err := commenter.CreatePullRequestComment(ctx, repository, number, validatorResultComment(result)); err != nil && o.logger != nil {
		o.logger.Warn(
			"validator result comment failed",
			"issue_id", strings.TrimSpace(issue.ID),
			"identifier", issue.Identifier,
			"pull_request", number,
			"error", err,
		)
	}
}

func validatorResultComment(result gate.ValidatorResult) string {
	var b strings.Builder
	b.WriteString("Validator verdict: ")
	b.WriteString(strings.TrimSpace(result.Verdict))
	if result.Score > 0 {
		b.WriteString("\n- score: ")
		b.WriteString(fmt.Sprintf("%.2f", result.Score))
	}
	if strings.TrimSpace(result.Summary) != "" {
		b.WriteString("\n- summary: ")
		b.WriteString(strings.TrimSpace(result.Summary))
	}
	if len(result.Findings) > 0 {
		b.WriteString("\n\nFindings:")
		for _, finding := range result.Findings {
			b.WriteString("\n- ")
			b.WriteString(autoPromoteFindingText(AutoPromoteFinding{
				Body: finding.Body,
				URL:  finding.URL,
				Path: finding.Path,
				Line: finding.Line,
			}))
		}
	}
	return b.String()
}

func pullRequestRepository(issue connector.Issue) string {
	if strings.TrimSpace(issue.PRRepository) != "" {
		return strings.TrimSpace(issue.PRRepository)
	}
	identifier := strings.TrimSpace(issue.Identifier)
	repository, _, ok := strings.Cut(identifier, "#")
	if ok {
		return strings.TrimSpace(repository)
	}
	return ""
}

func pullRequestNumber(issue connector.Issue) int {
	if issue.PullRequest != nil && issue.PullRequest.Number > 0 {
		return issue.PullRequest.Number
	}
	if issue.PRNumber != nil {
		return *issue.PRNumber
	}
	return 0
}

func validatorStageKey(issue connector.Issue) string {
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return ""
	}
	headSHA := ""
	if issue.PullRequest != nil {
		headSHA = strings.TrimSpace(issue.PullRequest.HeadSHA)
	}
	if headSHA == "" && issue.PullRequest != nil {
		headSHA = strings.TrimSpace(issue.PullRequest.BranchName)
	}
	if headSHA == "" {
		headSHA = strings.TrimSpace(issue.BranchName)
	}
	return issueID + ":" + headSHA
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
	summary.MergeableState = strings.ToLower(strings.TrimSpace(pullRequest.MergeableState))
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
		latest = latestTime(latest, issue.PullRequest.ActivityAt)
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

	body := autoPromoteComment(summary, decision, displayStateName(issue.State), targetState)
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
	if issue.PullRequest != nil {
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			attrs = append(attrs, "pull_request", url)
		}
		if mergeableState := strings.TrimSpace(issue.PullRequest.MergeableState); mergeableState != "" {
			attrs = append(attrs, "mergeable_state", mergeableState)
		}
	}
	if decision.QuietRemaining > 0 {
		attrs = append(attrs, "quiet_remaining", decision.QuietRemaining)
	}
	if targetState != "" {
		attrs = append(attrs, "target_state", targetState)
		o.logger.Info("auto promote decision", attrs...)
		return
	}
	o.logger.Info("auto promote decision", attrs...)
}

func autoPromoteComment(
	summary AutoPromoteSummary,
	decision AutoPromoteDecision,
	sourceState string,
	targetState string,
) string {
	var b strings.Builder
	sourceState = displayStateName(sourceState)
	if sourceState == "" {
		sourceState = autoPromoteSourceState
	}
	switch targetState {
	case autoPromoteMergingState:
		b.WriteString("Auto-promoted this issue from ")
		b.WriteString(sourceState)
		b.WriteString(" to Merging.")
	case autoPromoteReworkState:
		b.WriteString("Auto-promote routed this issue from ")
		b.WriteString(sourceState)
		b.WriteString(" to Rework")
		switch decision.Reason {
		case AutoPromoteReasonCINotGreen:
			b.WriteString(": current-head CI is failing")
		case AutoPromoteReasonMergeConflicts:
			b.WriteString(": linked PR has merge conflicts")
		}
		b.WriteString(".")
	case autoPromoteSourceState:
		b.WriteString("Reconciled this issue from ")
		b.WriteString(sourceState)
		b.WriteString(" to Human Review because it already has a linked PR.")
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
	if summary.MergeableState != "" {
		b.WriteString("\n- mergeable_state: ")
		b.WriteString(summary.MergeableState)
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
