package orchestrator

import (
	"context"
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
	if completionWorkpadUnfinished(decision.Issue) {
		decision.WorkpadStatus = workpad.StatusInProgress
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

func (o *Orchestrator) implementCompletionRebaseOnly(ctx context.Context, running Running, decision implementCompletionProgressDecision) bool {
	before, after := running.Issue.PullRequest, decision.Issue.PullRequest
	fingerprint := strings.TrimSpace(running.DispatchProgress.PullRequestDiffFingerprint)
	if fingerprint == "" || before == nil || after == nil || before.Number != after.Number ||
		strings.TrimSpace(before.HeadSHA) == "" || strings.TrimSpace(after.HeadSHA) == "" || before.HeadSHA == after.HeadSHA ||
		pullRequestMerged(after) || !implementProgressDiffStatsClean(decision.WorkspaceDiffStats) {
		return false
	}
	if stranded, unavailable := implementProgressUnpushedClassification(decision.WorkspaceDiffStats, after); stranded || unavailable != "" {
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
		signal.Source == workpad.SourceStructured && signal.Status == workpad.StatusInProgress
}
