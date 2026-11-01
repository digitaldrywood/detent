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
)

const (
	mergeWorkerMissingRequiredCheckLimit            = 3
	mergeWorkerPersistentMissingRequiredCheckReason = "merge_worker_required_checks_persistently_missing"
	workflowActionMergeRequiredCheckParkRecovery    = "merge_required_check_park_recovery"
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
		HeadSHA:                   strings.TrimSpace(issue.PullRequest.HeadSHA),
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
	running.Issue = issue
	reason := persistentMissingRequiredCheckReason(streaks)
	o.completeDurableWorkAttempt(ctx, state, running, event.CompletedAt, store.WorkAttemptTerminalSuccess, "", "", "completed", reason)
	o.retryMachineCompletion(ctx, state, event, running, nextAttempt(running.Attempt), reason)
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
