package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/activity"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) activityHTTP() *activity.HTTPService {
	return activity.NewHTTP(activity.HTTPDeps{Service: s.activityService, Unavailable: errHubOperatorUnavailable, Now: s.config.Clock, Available: func() bool { return s.database != nil && s.config.Hosted != nil }, ReadError: hubapi.SafeWorkReadError, Result: hubOperatorResult, Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) }, Refuse: func(c echo.Context, status int, code, reason string, response any) error {
		return s.refusedAPIError(c, status, code, reason, response)
	}, Caller: func(ctx context.Context) (activity.Caller, error) {
		credential, err := currentHubOperator(ctx)
		return s.activityCaller(credential), err
	}})
}
func (s *Service) activityCaller(credential hubapi.APICredential) activity.Caller {
	scope := hubscope.NativeScope{Organization: tracker.OrganizationID(s.appOrganizationID()), Credential: credential}
	return activity.Caller{Credential: hubscope.AuthCredential(credential), Scope: s.authorization().Scope(scope), IsNotFound: hubapi.IsNativeNotFound}
}
func (s *Service) executeActivityRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	return s.activityHTTP().Execute(ctx, call)
}
func (s *Service) hostedActivity(c echo.Context) error { return s.activityHTTP().Serve(c) }
func (s *Service) activityService() *activity.Service {
	return activity.New(activity.Deps{Repository: s.config.TenantRepository, Details: s.runDetailStore, Now: s.config.Clock, Organization: s.appOrganizationID, MissingParticipants: hubapi.ErrProjectParticipants, Unavailable: errHubOperatorUnavailable})
}
func (s *Service) readActivityReport(ctx context.Context, credential hubapi.APICredential, r operatortool.ActivityRequest, w operatortool.AnalyticsWindow) (operatortool.ActivityReport, error) {
	out, err := s.activityService().Read(ctx, s.activityCaller(credential), r, w)
	return out, hubapi.FromFeatureError(err)
}

func readDiagnosticObjects[T, R any](records []T, read func(T) (R, error)) ([]R, []error) {
	return reportdata.ReadObjects(records, read)
}
