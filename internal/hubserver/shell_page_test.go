package hubserver

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOnboardingPageDoesNotReadObjects(t *testing.T) {
	t.Parallel()
	const size = 2
	details := &memoryRunDetails{}
	service := newPostgresNativeService(t, Config{RunDetails: details})
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		details.beforeRead = nil
		fixture := newNativeFixture(t, service, organization, "Example")
		for range size {
			runner := prepareRunnerForScope(t, fixture, "projects", runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			runner.enroll(t)
		}
		saveOnboardingView(t, service, fixture.base, fixture.token)
		previousHosted := service.config.Hosted
		service.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(organization)}
		scheduler, err := service.newReadModelMaintenance()
		if err != nil {
			t.Fatal(err)
		}
		for range 16 {
			if err := scheduler.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		service.config.Hosted = previousHosted
		objects := 0
		details.beforeRead = func(context.Context) error { objects++; return nil }
		response := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/onboarding", fixture.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page onboarding.Project
		decodeHubResponse(t, response, &page)
		if len(page.Runners) != size || page.Policy == nil || page.Policy.Policy.ID == "" || len(page.Steps) != 4 {
			t.Errorf("tenant=%s size=%d wrong onboarding page: %+v", organization, size, page)
		}
		for _, runner := range page.Runners {
			if runner.Runner.OrganizationID != organization || !slices.Equal(runner.Runner.ProjectIDs, []tracker.ProjectID{fixture.project.ID}) {
				t.Error("runner crossed scope boundary")
			}
		}
		if objects != 0 {
			t.Errorf("onboarding request read %d objects", objects)
		}
	}
}
