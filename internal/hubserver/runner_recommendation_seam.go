package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runneradvice"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const capacityGiB = runneradvice.CapacityGiB

type (
	runnerCapacityRead   = runneradvice.CapacityRead
	runnerRecommendation = runneradvice.Recommendation
	slotRange            = runneradvice.SlotRange
	recommendationHour   = runneradvice.Hour
	recommendationRun    = runneradvice.Run
)

func recommendRunner(r runnerauth.Routing, hours []recommendationHour, runs []recommendationRun) (runnerRecommendation, error) {
	return runneradvice.Recommend(r, hours, runs)
}

func runnerRecommendationFromFacts(r runnerauth.Routing, hours []recommendationHour, facts reportstore.RunnerCapacityFacts) (runnerRecommendation, error) {
	return runneradvice.FromFacts(r, hours, facts)
}

func recommendationHoursFromSegments(segments []hostmetrics.Summary) ([]recommendationHour, error) {
	return runneradvice.HoursFromSegments(segments)
}

func mergeHostSummary(a, b hostmetrics.Summary) (hostmetrics.Summary, bool) {
	return runneradvice.MergeHostSummary(a, b)
}

func (s *Service) runnerAdviceService() *runneradvice.Service {
	return runneradvice.New(runneradvice.Deps{
		Repository: s.config.TenantRepository, Now: s.config.now, Auth: s.requestAuth, RecordError: hubapi.ProjectRecordError,
		ReadRouting: func(ctx context.Context, input hubauth.Input, resource string) (runnerauth.Runner, error) {
			return s.readRunnerRouting(ctx, nativeAuthInput(input), resource)
		},
		CapacityView: func(ctx context.Context, runner runnerauth.Runner, backend string, now time.Time) (runnerauth.CapacityView, error) {
			return s.runnerCapacityView(ctx, nil, runner, backend, now)
		},
	})
}
func (s *Service) registerRunnerAdviceRoutes(e *echo.Echo) {
	runneradvice.RegisterRoutes(e.Group(""), runneradvice.HTTPDeps{
		Service: s.runnerAdviceService,
		Credential: func(c echo.Context) (hubauth.Credential, bool) {
			credential, ok := c.Get("hub_api_credential").(hubapi.APICredential)
			return hubscope.AuthCredential(credential), ok
		},
		Admin: s.requireInstanceAdmin(),
		Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
	})
}

type runnerHostHour = runneradvice.HostHour

func prepareRunnerHostMetrics(raw json.RawMessage) []hostmetrics.Summary {
	return runneradvice.PrepareHostMetrics(raw)
}

func (s *Service) readRunnerHostHistory(ctx context.Context, organization tracker.OrganizationID, runner string, from, to time.Time) ([]runnerHostHour, error) {
	history, err := s.runnerAdviceService().HostHistory(ctx, organization, runner, from, to)
	return history, hubapi.FromFeatureError(err)
}
