package orchestrator

import (
	"context"
	"encoding/json"
	"maps"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workpad"
)

// Completion evidence classifies the session before review or gate-wait routing.
// The existing issue allowance owns stopping repeated implementation attempts.
func (o *Orchestrator) evaluateImplementCompletionProgress(ctx context.Context, running Running, finalState string, pullRequestUpdated bool) implementCompletionProgressDecision {
	decision := o.evaluateImplementCompletionCandidate(ctx, running, finalState, pullRequestUpdated)
	if strings.TrimSpace(finalState) != FinalStateCompleted || decision.DependencyDeferral {
		return decision
	}
	signal, _ := autoPromoteIssueWorkpadSignal(decision.Issue)
	mergedReceipt := workpad.MergedCompletionEvidence(signal)
	if decision.CompletionKind == workpad.CompletionOperational || decision.Reason == implementMergedCompletionReason && mergedReceipt {
		// Resolving a merged receipt must not bypass the attribution checks
		// applied to ordinary operational delivery, or complete on assertion alone.
		if operationalCompletionAttributed(signal) && !workpad.CurrentAttemptCompletion(signal, running.WorkAttemptID, running.Generation) ||
			mergedReceipt && !pullRequestMerged(decision.Issue.PullRequest) {
			decision.Outcome = store.WorkAttemptTerminalNoProgress
			decision.Reason = implementProgressOutcomeNoProgress
			decision.ProgressKinds = nil
			decision.CompletionKind = ""
			return decision
		}
		if decision.CompletionKind == workpad.CompletionOperational {
			return decision
		}
	}
	if completionWorkpadUnfinished(decision.Issue) {
		decision.WorkpadStatus = workpad.StatusInProgress
	} else if signal, _ := autoPromoteIssueWorkpadSignal(decision.Issue); completionForgeSupersedesWorkpad(decision.Issue.PullRequest, signal) {
		// This is a completion decision, not an authored Workpad receipt.
		decision.WorkpadStatus = workpad.StatusComplete
	}
	if !o.implementCompletionRebaseOnly(ctx, running, decision) {
		return decision
	}
	decision.Outcome = store.WorkAttemptTerminalNoProgress
	decision.Reason = implementProgressOutcomeNoProgress
	decision.ProgressKinds = nil
	decision.CompletionKind = ""
	return decision
}

type operationalCompletionReceipt struct {
	Generation uint64          `json:"generation"`
	Signal     *workpad.Signal `json:"signal"`
}

func operationalCompletionAttributed(signal *workpad.Signal) bool {
	return signal != nil && (signal.Fields[workpad.FieldCompletionAttempt] != "" || signal.Fields[workpad.FieldCompletionGeneration] != "")
}

func operationalCompletionEvidenceCurrent(accepted, current connector.Issue) bool {
	before, _ := autoPromoteIssueWorkpadSignal(accepted)
	after, _ := autoPromoteIssueWorkpadSignal(current)
	return before != nil && after != nil && maps.Equal(before.Fields, after.Fields)
}

func operationalCompletionReceiptMatches(issue connector.Issue, attempt store.WorkAttempt) bool {
	signal, _ := autoPromoteIssueWorkpadSignal(issue)
	var metadata struct {
		Receipt *operationalCompletionReceipt `json:"operational_completion_receipt"`
	}
	if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) != nil {
		return false
	}
	if metadata.Receipt == nil {
		// Unattributed legacy receipts retain their original timing check. Never
		// infer the generation of a newly attributed claim from old metadata.
		return !operationalCompletionAttributed(signal) && signal != nil && signal.RecordedAt != nil &&
			!signal.RecordedAt.IsZero() && !attempt.CompletedAt.Before(*signal.RecordedAt)
	}
	receipt := metadata.Receipt
	return signal != nil && receipt.Signal != nil && maps.Equal(signal.Fields, receipt.Signal.Fields) &&
		(!operationalCompletionAttributed(signal) || workpad.CurrentAttemptCompletion(signal, attempt.ID, receipt.Generation))
}

func (o *Orchestrator) implementCompletionRebaseOnly(ctx context.Context, running Running, decision implementCompletionProgressDecision) bool {
	signal, _ := autoPromoteIssueWorkpadSignal(decision.Issue)
	pr := decision.Issue.PullRequest
	if (workpad.CurrentAttemptCompletion(signal, running.WorkAttemptID, running.Generation) || completionForgeSupersedesWorkpad(pr, signal)) &&
		pullRequestOpen(pr) && !pr.Draft && !connector.PullRequestConflicts(pr.MergeableState) && !mergeWorkerCIFailed(pr) {
		return false
	}
	before, after := running.Issue.PullRequest, decision.Issue.PullRequest
	baseline := running.DispatchLoopStart
	if baseline.PRDiffFingerprint == "" && running.WorkAttemptID > 0 && o.workAttempts != nil {
		attempt, err := o.workAttempts.WorkAttempt(ctx, running.WorkAttemptID)
		if err == nil {
			var metadata struct {
				Start dispatchLoopStartRecord `json:"dispatch_loop_start"`
			}
			if json.Unmarshal([]byte(attempt.WorkerMetadataJSON), &metadata) == nil {
				baseline = metadata.Start
			}
		}
	}
	if baseline.PRDiffFingerprint != "" {
		before = &connector.PullRequest{Number: int(baseline.Fingerprint.PRNumber), HeadSHA: baseline.Fingerprint.PRHeadSHA, MergeableState: baseline.PRMergeableState}
	}
	fingerprint := strings.TrimSpace(firstNonBlank(baseline.PRDiffFingerprint, running.DispatchProgress.PullRequestDiffFingerprint))
	if fingerprint == "" || before == nil || after == nil || before.Number != after.Number ||
		strings.TrimSpace(before.HeadSHA) == "" || strings.TrimSpace(after.HeadSHA) == "" || before.HeadSHA == after.HeadSHA ||
		pullRequestMerged(after) || !implementProgressDiffStatsClean(decision.WorkspaceDiffStats) {
		return false
	}
	if stranded, unavailable := implementProgressUnpushedClassification(decision.WorkspaceDiffStats, after); stranded || unavailable != "" {
		return false
	}
	if connector.PullRequestConflictCleared(before.MergeableState, after.MergeableState) {
		return false
	}

	return fingerprint == o.implementCompletionDiffFingerprint(ctx, decision.Issue)
}

func (o *Orchestrator) implementCompletionDiffFingerprint(ctx context.Context, issue connector.Issue) string {
	if issue.PullRequest == nil {
		return ""
	}
	if fingerprint := strings.TrimSpace(issue.PullRequest.DiffFingerprint); fingerprint != "" {
		return fingerprint
	}
	reader, ok := o.connector.(connector.PullRequestDiffFingerprintReader)
	if !ok {
		return ""
	}
	fingerprint, err := reader.PullRequestDiffFingerprint(ctx, issue)
	if err != nil {
		o.warnImplementProgressHydration(issue, "pull request diff fingerprint unavailable", err)
		return ""
	}
	return strings.TrimSpace(fingerprint)
}

func completionWorkpadUnfinished(issue connector.Issue) bool {
	signal, ok := autoPromoteIssueWorkpadSignal(issue)
	return ok && signal != nil && signal.Invalid == nil &&
		signal.Source == workpad.SourceStructured && signal.Status == workpad.StatusInProgress &&
		!completionForgeSupersedesWorkpad(issue.PullRequest, signal)
}

func completionForgeSupersedesWorkpad(pr *connector.PullRequest, signal *workpad.Signal) bool {
	if signal == nil || signal.Invalid != nil || signal.Source != workpad.SourceStructured ||
		signal.Status != workpad.StatusInProgress || signal.HumanAction != "" || len(signal.Blockers) != 0 ||
		pr == nil || pullRequestHydrationBlocksProgress(pr) || strings.TrimSpace(pr.HeadSHA) == "" {
		return false
	}
	if pullRequestMerged(pr) {
		return true
	}
	return pullRequestOpen(pr) && !pr.Draft && signal.RecordedAt != nil && !signal.RecordedAt.IsZero() &&
		pr.HeadCommittedAt != nil && pr.HeadCommittedAt.After(*signal.RecordedAt)
}
