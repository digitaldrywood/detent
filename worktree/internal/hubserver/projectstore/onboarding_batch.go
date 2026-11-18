package projectstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type OnboardingRecord struct {
	ProjectID  tracker.ProjectID
	Repository string
	Progress   onboarding.Progress
	Policy     *policystore.Revision
}

type OnboardingBatchReader interface {
	runnerstore.FleetReader
	ProjectOnboardingRecords(context.Context, []tracker.ProjectID) ([]OnboardingRecord, error)
	ProjectRunnerChecksForProjects(context.Context, []tracker.ProjectID, int) (map[tracker.ProjectID]map[string]runnerauth.LocalChecks, error)
}
