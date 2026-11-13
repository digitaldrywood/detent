package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/activity"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) activityHTTP() *activity.HTTPService {
	return activity.NewHTTP(activity.HTTPDeps{Service: s.activityService, Unavailable: errHubOperatorUnavailable, Now: s.config.now, Available: func() bool { return s.database != nil && s.config.Hosted != nil }, ReadError: safeWorkReadError, Result: hubOperatorResult, Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) }, Refuse: func(c echo.Context, status int, code, reason string, response any) error {
		return s.refusedAPIError(c, status, code, reason, response)
	}, Caller: func(ctx context.Context) (activity.Caller, error) {
		credential, err := currentHubOperator(ctx)
		return s.activityCaller(credential), err
	}})
}
func (s *Service) activityCaller(credential apiCredential) activity.Caller {
	scope := nativeScope{organization: tracker.OrganizationID(s.appOrganizationID()), credential: credential}
	return activity.Caller{Credential: authCredential(credential), Scope: s.featureScope(scope), IsNotFound: isNativeNotFound}
}
func (s *Service) executeActivityRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	return s.activityHTTP().Execute(ctx, call)
}
func (s *Service) hostedActivity(c echo.Context) error { return s.activityHTTP().Serve(c) }
func (s *Service) activityService() *activity.Service {
	return activity.New(activity.Deps{Repository: s.config.TenantRepository, Details: s.runDetailStore, Now: s.config.now, Organization: s.appOrganizationID, MissingParticipants: ErrProjectParticipants, Unavailable: errHubOperatorUnavailable})
}
func (s *Service) readActivityReport(ctx context.Context, credential apiCredential, r operatortool.ActivityRequest, w operatortool.AnalyticsWindow) (operatortool.ActivityReport, error) {
	out, err := s.activityService().Read(ctx, s.activityCaller(credential), r, w)
	return out, fromFeatureError(err)
}
func closedAttemptSeconds(phases []tracker.NativePhase) float64 {
	return reportdata.ClosedAttemptSeconds(phases)
}

type analyticsResidenceSample = reportdata.ResidenceSample

func analyticsDuration(values []float64) operatortool.AnalyticsDuration {
	return reportdata.Duration(values)
}
func readDiagnosticObjects[T, R any](records []T, read func(T) (R, error)) ([]R, []error) {
	return reportdata.ReadObjects(records, read)
}

type reportsStage = reportdata.Stage
type reportsSpend = reportdata.Spend

func sampledDuration(values []float64) operatortool.AnalyticsDuration {
	return reportdata.SampledDuration(values)
}
func activityRunRole(data tracker.NativeRunData) string { return activity.RunRole(data) }
func reportsAttemptTotals(attempts []nativeAnalyticsAttempt, usage map[string][]usageRow) ([]reportsStage, []reportsSpend) {
	values := make([]reportdata.AttemptTiming, len(attempts))
	for n, a := range attempts {
		values[n] = reportdata.AttemptTiming{Identity: a.Identity, AttemptID: a.AttemptID, WorkItemID: a.WorkItemID, Status: a.Status, Phases: a.Phases}
	}
	rows := make(map[string][]reportdata.Usage, len(usage))
	for id, us := range usage {
		for _, u := range us {
			rows[id] = append(rows[id], reportdata.Usage{Input: u.Input, CachedInput: u.CachedInput, Output: u.Output, Cost: u.Cost})
		}
	}
	return reportdata.AttemptTotals(values, rows)
}
