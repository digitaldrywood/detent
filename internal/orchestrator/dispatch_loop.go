package orchestrator

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

const dispatchLoopDetectedReason = "dispatch_loop_detected"

const dispatchLoopMinimumDispatches = 2

func dispatchLoopBlockMessage(decision implementCompletionProgressDecision) string {
	return fmt.Sprintf(
		"loop detected after %d dispatches without lane, diff, commit, or pull request advancement",
		decision.ConsecutiveNoProgress,
	)
}

type dispatchLoopFingerprint struct {
	Lane            string
	PRNumber        int64
	PRHeadSHA       string
	FailedChecks    []string
	FilesChanged    int
	AddedLines      int
	RemovedLines    int
	UnpushedCommits int
	WorkspaceHead   string
	DiffFingerprint string
	DiffStatus      string
}

func (o *Orchestrator) evaluateDispatchLoopProgress(
	ctx context.Context,
	running Running,
	decision implementCompletionProgressDecision,
) implementCompletionProgressDecision {
	if decision.NoProgressLimit <= 0 || dispatchLoopPreservesSpecificDecision(decision) {
		return decision
	}

	dispatchLane := normalizeState(firstNonBlank(running.DispatchSourceState, running.Issue.State))
	currentLane := normalizeState(firstNonBlank(decision.TrackerState, decision.Issue.State))
	decision.TrackerState = strings.TrimSpace(firstNonBlank(decision.TrackerState, decision.Issue.State))
	if dispatchLane == "" || currentLane == "" || dispatchLane != currentLane {
		return resetDispatchLoopDecision(decision)
	}

	attempts, err := o.recentImplementCompletionAttempts(ctx, decision.Issue, running)
	if err != nil {
		if decision.Warning == "" {
			decision.Warning = err.Error()
		}
		return decision
	}

	current := dispatchLoopFingerprintFromDecision(decision, currentLane)
	previous, found := latestDispatchLoopFingerprint(attempts, currentLane)
	if dispatchLoopCurrentAdvanced(decision, current, previous, found) {
		return resetDispatchLoopDecision(decision)
	}

	decision.ConsecutiveNoProgress = 1 + consecutiveDispatchLoopAttempts(attempts, current)
	if decision.BlockReason == noProgressLimitReason {
		decision.Block = false
		decision.BlockReason = ""
	}
	if decision.ConsecutiveNoProgress < dispatchLoopMinimumDispatches || decision.ConsecutiveNoProgress < decision.NoProgressLimit {
		return decision
	}

	decision.Outcome = store.WorkAttemptTerminalNoProgress
	decision.Reason = dispatchLoopDetectedReason
	decision.BlockReason = dispatchLoopDetectedReason
	decision.Block = true
	decision.DependencyDeferral = false
	return decision
}

func dispatchLoopPreservesSpecificDecision(decision implementCompletionProgressDecision) bool {
	switch strings.TrimSpace(decision.Reason) {
	case strandedUnpushedWorkReason, workpadBlockedUnactionedReason:
		return true
	default:
		return decision.Block && decision.BlockReason != "" && decision.BlockReason != noProgressLimitReason
	}
}

func resetDispatchLoopDecision(decision implementCompletionProgressDecision) implementCompletionProgressDecision {
	decision.ConsecutiveNoProgress = 0
	if decision.BlockReason == noProgressLimitReason {
		decision.Block = false
		decision.BlockReason = ""
	}
	return decision
}

func dispatchLoopCurrentAdvanced(
	decision implementCompletionProgressDecision,
	current dispatchLoopFingerprint,
	previous dispatchLoopFingerprint,
	found bool,
) bool {
	switch strings.TrimSpace(decision.Reason) {
	case "pull_request_created_or_updated":
		return implementProgressSignatureUsable(decision.CurrentSignature)
	case implementMergedCompletionReason:
		return true
	case implementOperationalCompletion:
		return strings.TrimSpace(decision.CompletionKind) == workpad.CompletionOperational
	case "pull_request_hydrator_unavailable", "pull_request_hydration_failed", "pull_request_hydration_unavailable",
		"attempt_history_lookup_failed", "workspace_diffstat_unavailable", "workspace_diffstat_unavailable_without_pull_request",
		"workspace_diff_fingerprint_unavailable_without_pull_request", workspaceHeadUnavailableReason,
		pullRequestHeadUnavailableReason, unpushedRemoteTruthUnavailable:
		return true
	}
	if found {
		return !dispatchLoopFingerprintEqual(previous, current)
	}
	return !implementProgressDiffStatsClean(decision.WorkspaceDiffStats) && diffStatsPresent(decision.WorkspaceDiffStats)
}

func latestDispatchLoopFingerprint(attempts []store.WorkAttempt, currentLane string) (dispatchLoopFingerprint, bool) {
	for _, attempt := range attempts {
		record, ok := implementProgressRecordFromAnyAttempt(attempt)
		if !ok {
			continue
		}
		return dispatchLoopFingerprintFromRecord(attempt, record, currentLane), true
	}
	return dispatchLoopFingerprint{}, false
}

func consecutiveDispatchLoopAttempts(attempts []store.WorkAttempt, current dispatchLoopFingerprint) int {
	count := 0
	for _, attempt := range attempts {
		record, ok := implementProgressRecordFromAnyAttempt(attempt)
		if !ok {
			return count
		}
		fingerprint := dispatchLoopFingerprintFromRecord(attempt, record, current.Lane)
		if !dispatchLoopFingerprintEqual(fingerprint, current) || dispatchLoopRecordAdvanced(record) {
			return count
		}
		count++
	}
	return count
}

func dispatchLoopRecordAdvanced(record implementProgressRecord) bool {
	if record.ConsecutiveNoProgress > 0 {
		return false
	}
	for _, kind := range record.ProgressKinds {
		switch strings.TrimSpace(kind) {
		case "tracker_state_transition", "workspace_diff":
			return true
		case "pull_request":
			if implementProgressSignatureUsable(record.CurrentSignature.signature()) {
				return true
			}
		}
	}
	switch strings.TrimSpace(record.Reason) {
	case "pull_request_created_or_updated":
		return implementProgressSignatureUsable(record.CurrentSignature.signature())
	case "signature_changed", implementMergedCompletionReason:
		return true
	case implementOperationalCompletion:
		return strings.TrimSpace(record.CompletionKind) == workpad.CompletionOperational
	default:
		return false
	}
}

func dispatchLoopFingerprintFromDecision(decision implementCompletionProgressDecision, lane string) dispatchLoopFingerprint {
	diff := implementProgressDiffStatsFromDiffStats(decision.WorkspaceDiffStats)
	return dispatchLoopFingerprintFromValues(lane, decision.CurrentSignature, diff)
}

func dispatchLoopFingerprintFromRecord(attempt store.WorkAttempt, record implementProgressRecord, fallbackLane string) dispatchLoopFingerprint {
	lane := normalizeState(firstNonBlank(record.TrackerState, attempt.Lane, fallbackLane))
	return dispatchLoopFingerprintFromValues(lane, record.CurrentSignature.signature(), record.WorkspaceDiffStats)
}

func dispatchLoopFingerprintFromValues(lane string, signature autoPromoteReworkSignature, diff implementProgressDiffStats) dispatchLoopFingerprint {
	diffStatus := strings.TrimSpace(diff.Status)
	if diffStatus == "" && diff.FilesChanged == 0 && diff.AddedLines == 0 && diff.RemovedLines == 0 &&
		diff.UnpushedCommits == 0 && strings.TrimSpace(diff.HeadSHA) == "" && strings.TrimSpace(diff.Fingerprint) == "" {
		diffStatus = "clean"
	}
	return dispatchLoopFingerprint{
		Lane:            normalizeState(lane),
		PRNumber:        signature.PRNumber,
		PRHeadSHA:       strings.TrimSpace(signature.HeadSHA),
		FailedChecks:    autoPromoteCanonicalChecks(signature.FailedChecks),
		FilesChanged:    diff.FilesChanged,
		AddedLines:      diff.AddedLines,
		RemovedLines:    diff.RemovedLines,
		UnpushedCommits: diff.UnpushedCommits,
		WorkspaceHead:   strings.TrimSpace(diff.HeadSHA),
		DiffFingerprint: strings.TrimSpace(diff.Fingerprint),
		DiffStatus:      diffStatus,
	}
}

func dispatchLoopFingerprintEqual(left dispatchLoopFingerprint, right dispatchLoopFingerprint) bool {
	return left.PRNumber == right.PRNumber &&
		left.PRHeadSHA == right.PRHeadSHA &&
		slices.Equal(left.FailedChecks, right.FailedChecks) &&
		left.FilesChanged == right.FilesChanged &&
		left.AddedLines == right.AddedLines &&
		left.RemovedLines == right.RemovedLines &&
		left.UnpushedCommits == right.UnpushedCommits &&
		left.WorkspaceHead == right.WorkspaceHead &&
		left.DiffFingerprint == right.DiffFingerprint &&
		left.DiffStatus == right.DiffStatus
}
