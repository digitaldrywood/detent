package hubserver

import (
	"context"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readRunnerRecoveryEligibility(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, item tracker.NativeWorkItemID) (bool, bool, error) {
	if scope.Credential.Scope != hubapi.APIScopeWorker || scope.Credential.Runner.RunnerID == "" || !slices.Contains(scope.Credential.Runner.Operations, runnerauth.Claim) {
		return false, false, nil
	}
	owner, ok := reader.(interface {
		RunnerRecoveryEligibility(context.Context, tracker.ProjectID, tracker.NativeWorkItemID) (bool, error)
	})
	if !ok {
		return false, false, hubapi.NativeRunEventStorageUnavailable()
	}
	eligible, err := owner.RunnerRecoveryEligibility(ctx, scope.Project, item)
	if err != nil {
		return false, false, err
	}
	return eligible, true, nil
}
