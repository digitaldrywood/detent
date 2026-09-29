package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// landNativeChange is a hub-native landing run. There is no agent in it: the
// reviewed head is combined with the base branch by the runner's own git and
// pushed with the runner's own credentials, and the hub records the commit
// the base branch advanced to. A refusal (a head that moved, a conflict, a
// base branch the forge protects) is reported on the run for the
// orchestrator to hand back to review with its reason; only an
// infrastructure failure fails the run.
func (r *Runner) landNativeChange(ctx context.Context, req RunRequest, landing LandingExecution, backend workspace.Backend, info workspace.Info, issue workspace.Issue) (RunResult, error) {
	target, err := landing.LandingTarget(ctx)
	if err != nil {
		if errors.Is(err, ErrLandingNotReviewed) {
			return r.refusedLanding(req, target, workspace.LandRefusalNothing, err.Error()), nil
		}
		return RunResult{}, fmt.Errorf("resolve landing target: %w", err)
	}
	lander, ok := backend.(workspace.Lander)
	if !ok {
		return RunResult{}, errors.New("workspace backend cannot land changes")
	}
	message := target.Title
	if message == "" {
		message = "Land " + target.HeadSHA
	}
	message += fmt.Sprintf("\n\nChange Request %s, round %d, head %s.", target.ChangeID, target.Number, target.HeadSHA)
	result, err := lander.LandChange(ctx, info, issue, workspace.LandOptions{HeadSHA: target.HeadSHA, Method: target.Method, Message: message, PushAttemptBranch: true})
	var refusal *workspace.LandRefusal
	if errors.As(err, &refusal) {
		r.logWorkerEvent(req.Issue, "worker_native_landing_refused",
			telemetry.WorkAttemptIDKey, req.WorkAttemptID, "change", target.ChangeID, "version", target.VersionID, "kind", refusal.Kind, "reason", refusal.Reason)
		return r.refusedLanding(req, target, refusal.Kind, refusal.Reason), nil
	}
	if err != nil {
		return RunResult{}, err
	}
	landed := NativeLanding{ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA, Landed: true, MergeSHA: result.MergeSHA, BaseRef: result.BaseRef, Method: result.Method}
	if err := landing.RecordLanding(ctx, landed); err != nil {
		return RunResult{}, fmt.Errorf("record landing %s on %s: %w", result.MergeSHA, result.BaseRef, err)
	}
	r.logWorkerEvent(req.Issue, "worker_native_landed",
		telemetry.WorkAttemptIDKey, req.WorkAttemptID, "change", target.ChangeID, "version", target.VersionID, "merge_sha", result.MergeSHA, "base_ref", result.BaseRef, "method", result.Method)
	return RunResult{FinalState: FinalStateCompleted, Output: RunOutputNativeLanded, NativeLanding: &landed}, nil
}

func (r *Runner) refusedLanding(_ RunRequest, target NativeLandingTarget, kind, reason string) RunResult {
	return RunResult{FinalState: FinalStateCompleted, Output: RunOutputNativeLandingRefused, NativeLanding: &NativeLanding{
		ChangeID: target.ChangeID, VersionID: target.VersionID, HeadSHA: target.HeadSHA, RefusalKind: kind, Refusal: reason,
	}}
}
