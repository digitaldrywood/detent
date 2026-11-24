package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/runner/phaserecovery"
	"github.com/digitaldrywood/detent/internal/runner/runworkspace"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (r *Runner) sourceFinalizationContext(ctx context.Context, req RunRequest, timeout time.Duration) (context.Context, context.CancelFunc) {
	if req.Execution == nil {
		ctx = context.WithoutCancel(ctx)
	}
	return r.finalizationContext(ctx, timeout, NewCancellationCause(context.DeadlineExceeded, "runner.finalization"))
}

type sourceTurnBudgetKey struct{}

type sourceTurnBudget struct {
	remaining int
}

func sourceBudgetRemaining(ctx context.Context) int {
	if budget, ok := ctx.Value(sourceTurnBudgetKey{}).(*sourceTurnBudget); ok {
		return budget.remaining
	}
	return -1
}

func sourceBudgetExhaustedMessage(message string) string {
	return message + "\n\nThe shared implementation and review turn budget is exhausted; preserve the current source and review findings for Rework.\n\n```detent-status\nschema: 1\nstatus: in_progress\nblockers: []\nhuman_action: null\n```"
}

func completedSourceDisposition(message string) bool {
	disposition := NativeDispositionFromMessage(message)
	return disposition != nil && disposition.Status == "complete" && !disposition.Blockers && !disposition.HumanAction && disposition.ReasonCode == ""
}

func nativeSourceRework(change *NativeChange, message string) string {
	if !completedSourceDisposition(message) {
		return ""
	}
	if change == nil || change.Reviewed || change.VersionID == "" && change.NoChange == nil || change.Error != "" || change.VersionError != "" || change.ImplementationReviewError != "" || change.Landing != nil {
		return ""
	}
	version := change.VersionID
	if change.NoChange != nil {
		version = change.NoChange.VersionID()
	}
	return phaserecovery.ReworkFindings(version, change.HeadSHA, change.SecurityAudit, change.ImplementationReview, change.Validator)
}

func sourceFinalizationPrompt(status workspace.MergePrepareStatus) string {
	if status == workspace.MergePrepareStatusConflict {
		return "The runner paused the rebase at unresolved conflicts. Inspect git status, resolve the conflicts, and stage the resolutions for host finalization. The runner owns native rebase continuation and commit signing."
	}
	return "The workspace has uncommitted changes. Inspect git status, stage any remaining issue changes for host finalization, and discard unintended changes you produced. The runner owns native rebase continuation and commit signing."
}

type sourceRepair struct {
	err        error
	output     string
	incomplete *workspace.SourceFinalizationIncomplete
}

func (e *sourceRepair) Error() string { return e.err.Error() }
func (e *sourceRepair) Unwrap() error { return e.err }

type PhaseReceiptExecution = runworkspace.PhaseReceiptExecution
type itemPhaseWorkspace = runworkspace.PhasePublisher

func requiresPublishedChangeRecovery(recovery tracker.NativeRecovery, continuation *tracker.NativePhaseContinuation) bool {
	if continuation == nil {
		return false
	}
	if recovery.SourceVersionID != "" && (recovery.Change == nil || recovery.Change.VersionID != recovery.SourceVersionID) {
		return true
	}
	change := recovery.Change
	if change == nil {
		return false
	}
	for _, receipt := range []*tracker.NativePhaseReceipt{continuation.LastPushed, continuation.PublishedChangeReceipt} {
		if receipt != nil && receipt.HeadSHA == change.HeadSHA && !tracker.IsPlanPhase(receipt.Phase) {
			return false
		}
	}
	return true
}

func retainsPhaseCheckout(recovery tracker.NativeRecovery, scope workspace.AttemptWorkspace, activeAttemptID string) bool {
	lease := recovery.Lease
	if scope.Key() == "" || lease.ID == "" || lease.FencingToken == 0 {
		return false
	}
	for _, attempt := range recovery.Attempts {
		if attempt.AttemptID == scope.Attempt {
			return attempt.LeaseID == lease.ID && attempt.FencingToken == lease.FencingToken && attempt.ClaimReleasedAt == nil
		}
	}
	return scope.Attempt == activeAttemptID
}

func verifyItemPhaseCheckout(ctx context.Context, backend workspace.Backend, info workspace.Info, issue workspace.Issue, state *workspace.RecoveryState) error {
	if info.Created {
		head := info.RecoveredPhase.HeadSHA
		if head == "" && issue.PhaseSource != nil {
			head = issue.PhaseSource.HeadSHA
		}
		if head == "" || state == nil || state.HeadSHA != head {
			return fmt.Errorf("%w: checkout after setup differs from the verified phase head or is unavailable", ErrNativeRecoveryRequired)
		}
	}
	verifier, ok := backend.(interface {
		VerifyItemPhaseCheckout(context.Context, workspace.Info, workspace.Issue) error
	})
	if !ok {
		return fmt.Errorf("%w: workspace cannot verify the recorded phase after setup", ErrNativeRecoveryRequired)
	}
	if err := verifier.VerifyItemPhaseCheckout(ctx, info, issue); err != nil {
		return fmt.Errorf("%w: verify the recorded phase after setup: %w", ErrNativeRecoveryRequired, err)
	}
	return nil
}

func publishItemPhase(ctx context.Context, req RunRequest, backend workspace.Backend, info workspace.Info, issue workspace.Issue, phase, input string) error {
	if !req.phaseReceipts {
		return nil
	}
	if issue.ExpectedPhaseRemoteHead == "" && req.publishedPhaseSource != nil {
		issue.ExpectedPhaseRemoteHead = req.publishedPhaseSource.HeadSHA
	}
	source, err := runworkspace.PublishPhase(ctx, req.Execution.(PhaseReceiptExecution), backend, info, issue, req.phaseContinuation, phase, input)
	if req.publishedPhaseSource != nil && source.HeadSHA != "" {
		*req.publishedPhaseSource = source
	}
	return failedStep("publication", err)
}

func publishReviewedPhase(ctx context.Context, req RunRequest, phase string, version *tracker.ChangeVersion) error {
	source := req.retainedReview
	if source == nil || !completedSourceDisposition(source.message) {
		return nil
	}
	if !req.phaseReceipts && req.publishedPhaseSource != nil && req.publishedPhaseSource.HeadSHA != "" {
		req.phaseReceipts = true
	}
	identity := ""
	if version != nil {
		identity = version.ID
	}
	return publishItemPhase(ctx, req, source.backend, source.info, source.issue, phase, identity)
}

func (r *Runner) resumeReviewWorkspace(ctx context.Context, req RunRequest, backend workspace.Backend, continuation NativeReviewContinuation) (*retainedReviewWorkspace, func(), error) {
	target, known := req.workspaceTarget(ctx, r.projectID)
	worktree, ok := backend.(interface {
		workspace.Usage
		workspace.HeadProvider
		workspace.ReviewTreeVerifier
		Existing(workspace.Issue) (workspace.Info, error)
	})
	if !known || !ok {
		return nil, nil, fmt.Errorf("%w: held review workspace is unavailable", ErrNativeRecoveryRequired)
	}
	release, err := worktree.Use(ctx, target)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: hold review workspace: %w", ErrNativeRecoveryRequired, err)
	}
	info, err := worktree.Existing(target)
	if err == nil {
		var head string
		head, err = worktree.Head(ctx, info, target)
		if err == nil && head != continuation.Source.HeadSHA {
			err = errors.New("held review workspace differs from the published head")
		}
	}
	if err == nil {
		err = worktree.VerifyReviewTree(ctx, info, target)
	}
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("%w: verify held review workspace: %w", ErrNativeRecoveryRequired, err)
	}
	return &retainedReviewWorkspace{backend: backend, info: info, issue: target, message: continuation.FinalMessage}, release, nil
}
