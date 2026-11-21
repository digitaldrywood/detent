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
	if req.Execution == nil || req.observationPeer == nil {
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

type sourceGateRepair struct {
	err    error
	output string
}

func (e *sourceGateRepair) Error() string { return e.err.Error() }
func (e *sourceGateRepair) Unwrap() error { return e.err }

type PhaseReceiptExecution = runworkspace.PhaseReceiptExecution
type itemPhaseWorkspace = runworkspace.PhasePublisher

func publishItemPhase(ctx context.Context, req RunRequest, backend workspace.Backend, info workspace.Info, issue workspace.Issue, phase, input string) error {
	if !req.phaseReceipts {
		return nil
	}
	return failedStep("publication", runworkspace.PublishPhase(ctx, req.Execution.(PhaseReceiptExecution), backend, info, issue, req.phaseContinuation, phase, input))
}

func publishReviewedPhase(ctx context.Context, req RunRequest, phase string, version *tracker.ChangeVersion) error {
	source := req.retainedReview
	if source == nil || !completedSourceDisposition(source.message) {
		return nil
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
