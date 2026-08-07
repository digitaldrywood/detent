package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

const (
	mergeWorkerMissingRequiredCheckLimit            = 3
	mergeWorkerPersistentMissingRequiredCheckReason = "merge_worker_required_checks_persistently_missing"
)

func (o *Orchestrator) evaluateMergeRequiredCheckStreaks(
	ctx context.Context,
	issue connector.Issue,
	missingChecks []string,
	evaluatedAt time.Time,
) []store.MergeRequiredCheckStreak {
	if o == nil || o.mergeRequiredChecks == nil || pullRequestHydrationBlocksProgress(issue.PullRequest) {
		return nil
	}
	issueID := strings.TrimSpace(issue.ID)
	prNumber := pullRequestNumber(issue)
	if issueID == "" || prNumber <= 0 {
		return nil
	}
	if evaluatedAt.IsZero() {
		evaluatedAt = o.clockNow()
	}
	requiredChecks := gate.Effective(o.cfg.AutoPromote.Gate).RequiredStatusChecks
	streaks, err := o.mergeRequiredChecks.EvaluateMergeRequiredChecks(ctx, store.MergeRequiredCheckEvaluation{
		ProjectID:                 o.workflowMetricsProjectID(),
		IssueID:                   issueID,
		Repository:                pullRequestRepository(issue),
		PRNumber:                  prNumber,
		RequiredChecksFingerprint: mergeRequiredCheckConfigFingerprint(requiredChecks),
		MissingChecks:             missingChecks,
		EvaluatedAt:               evaluatedAt,
	})
	if err != nil {
		if o.logger != nil {
			o.logger.Warn(
				"merge required check streak evaluation failed",
				"issue_id", issueID,
				"identifier", issue.Identifier,
				"pull_request_number", prNumber,
				"error", err,
			)
		}
		return nil
	}
	return streaks
}

func mergeRequiredCheckConfigFingerprint(checkNames []string) string {
	normalized := gate.NormalizeRequiredStatusChecks(checkNames)
	sort.Strings(normalized)
	var encoded strings.Builder
	for _, checkName := range normalized {
		encoded.WriteString(strconv.Itoa(len(checkName)))
		encoded.WriteByte(':')
		encoded.WriteString(checkName)
	}
	digest := sha256.Sum256([]byte(encoded.String()))
	return hex.EncodeToString(digest[:])
}

func persistentMissingRequiredCheckStreaks(streaks []store.MergeRequiredCheckStreak) []store.MergeRequiredCheckStreak {
	persistent := make([]store.MergeRequiredCheckStreak, 0, len(streaks))
	for _, streak := range streaks {
		if streak.ConsecutiveMissing >= mergeWorkerMissingRequiredCheckLimit {
			persistent = append(persistent, streak)
		}
	}
	return persistent
}

func (o *Orchestrator) blockPersistentlyMissingRequiredChecks(
	ctx context.Context,
	state *State,
	event runpkg.Completion,
	running Running,
	issue connector.Issue,
	streaks []store.MergeRequiredCheckStreak,
) {
	issueID := strings.TrimSpace(event.IssueID)
	running.Issue = issue
	reason := persistentMissingRequiredCheckReason(streaks)
	metadata := o.newBlockedRecoveryMetadata(
		ctx,
		issue,
		RunModeMerge,
		reason,
		blockedRecoveryPredicateManaged,
		autoPromoteMergingState,
		running.DiffStats,
	)
	metadata.BlockedRecovery.Owner = blockedRecoveryOwnerHuman
	if err := o.updateIssueStateByIDStrictWithMetadata(
		ctx,
		state,
		issueID,
		issue,
		blockedStatusState,
		event.CompletedAt,
		reason,
		metadata,
	); err != nil {
		o.failProgrammaticMergeWorkerResult(ctx, state, event, running, "merge_worker_persistent_missing_required_check_block_failed", err)
		return
	}
	o.clearMergeRequiredCheckStreaks(ctx, issue)

	if comment := persistentMissingRequiredCheckComment(issue, streaks); comment != "" {
		if err := o.connector.CreateComment(ctx, issueID, comment); err != nil && o.logger != nil {
			o.logger.Warn("persistent missing required check comment failed", "issue_id", issueID, "reason", reason, "error", err)
		}
	}
	o.recordProjectAttemptOutcome(state, event.IssueID, event.CompletedAt, store.WorkAttemptTerminalSuccess, nil, "", "")
	o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "blocked", "required checks persistently missing")
	o.logMergeWorkerFailure(issue, reason, nil)
	o.recordMergeFailed(state, issue, event.CompletedAt, reason, nil)
	if err := o.abandonClaim(ctx, issueID); err != nil && o.logger != nil {
		o.logger.Warn("abandon persistent missing required check claim failed", "issue_id", issueID, "error", err)
	}
	delete(state.Claimed, issueID)
	delete(state.Retry, issueID)
	delete(state.BudgetRefusals, issueID)
	delete(state.PriorAttempts, issueID)
	delete(state.Completed, issueID)
	delete(state.AutoPromoteDecisions, issueID)
	delete(state.InstantFailures, issueID)
	delete(state.RepeatedFailures, issueID)

	blockedIssue := cloneIssue(issue)
	blockedIssue.State = blockedStatusState
	blockedIssue.BlockerReason = reason
	blockedAt := event.CompletedAt.UTC()
	blockedIssue.StageUpdatedAt = &blockedAt
	if state.Blocked == nil {
		state.Blocked = map[string]Blocked{}
	}
	state.Blocked[issueID] = Blocked{
		Issue:          blockedIssue,
		Reason:         reason,
		RecoveryReason: string(BlockedRecoveryReasonHumanBlocker),
		RecoveryTarget: autoPromoteMergingState,
		BlockedAt:      event.CompletedAt,
		Source:         BlockedSourceProjectStatus,
		Recovery:       metadata.BlockedRecovery,
	}
	recordStateEvent(state, telemetry.ActivityEvent{
		At:      event.CompletedAt,
		Event:   mergeWorkerPersistentMissingRequiredCheckReason,
		Message: "parked " + issueLabel(issue) + " after required checks remained missing: " + persistentMissingRequiredCheckNames(streaks),
	})
}

func persistentMissingRequiredCheckReason(streaks []store.MergeRequiredCheckStreak) string {
	return mergeWorkerPersistentMissingRequiredCheckReason + ": " + persistentMissingRequiredCheckNames(streaks)
}

func persistentMissingRequiredCheckNames(streaks []store.MergeRequiredCheckStreak) string {
	names := make([]string, 0, len(streaks))
	for _, streak := range streaks {
		name := strings.TrimSpace(streak.CheckName)
		if name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

func persistentMissingRequiredCheckComment(issue connector.Issue, streaks []store.MergeRequiredCheckStreak) string {
	if len(streaks) == 0 {
		return ""
	}
	var body strings.Builder
	body.WriteString("Detent parked this issue in Blocked because required checks remained missing across merge evaluations.")
	body.WriteString("\n\n- reason: ")
	body.WriteString(persistentMissingRequiredCheckReason(streaks))
	body.WriteString("\n- missing_required_checks:")
	for _, streak := range streaks {
		body.WriteString("\n  - ")
		body.WriteString(strings.TrimSpace(streak.CheckName))
		body.WriteString(" (consecutive evaluations: ")
		body.WriteString(strconv.Itoa(streak.ConsecutiveMissing))
		body.WriteString(")")
	}
	if issue.PullRequest != nil {
		if url := strings.TrimSpace(issue.PullRequest.URL); url != "" {
			body.WriteString("\n- pull request: ")
			body.WriteString(url)
		}
		if headSHA := strings.TrimSpace(issue.PullRequest.HeadSHA); headSHA != "" {
			body.WriteString("\n- head_sha: ")
			body.WriteString(headSHA)
		}
	}
	body.WriteString("\n\nRestore or update the required-check configuration, then move the issue back to Merging.")
	return body.String()
}

func (o *Orchestrator) clearMergeRequiredCheckStreaks(ctx context.Context, issue connector.Issue) {
	if o == nil || o.mergeRequiredChecks == nil {
		return
	}
	issueID := strings.TrimSpace(issue.ID)
	if issueID == "" {
		return
	}
	if err := o.mergeRequiredChecks.ClearMergeRequiredCheckStreaks(ctx, o.workflowMetricsProjectID(), issueID); err != nil && o.logger != nil {
		o.logger.Warn(
			"clear merge required check streaks failed",
			"issue_id", issueID,
			"identifier", issue.Identifier,
			"error", err,
		)
	}
}
