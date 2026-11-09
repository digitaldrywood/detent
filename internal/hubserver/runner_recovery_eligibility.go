package hubserver

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readRunnerRecoveryEligibility(ctx context.Context, reader tenantstore.Reader, scope nativeScope, item tracker.NativeWorkItemID) (bool, bool, error) {
	if scope.credential.Scope != apiScopeWorker || scope.credential.Runner.RunnerID == "" || !slices.Contains(scope.credential.Runner.Operations, runnerauth.Claim) {
		return false, false, nil
	}
	owner, ok := reader.(interface {
		RunnerRecoveryEligibility(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (bool, error)
	})
	if !ok {
		return false, false, nativeRunEventStorageUnavailable()
	}
	eligible, err := owner.RunnerRecoveryEligibility(ctx, scope.project, item)
	if err != nil {
		return false, false, err
	}
	return eligible, true, nil
}
