package hubserver

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integrationpage"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const integrationPageMaintenanceBytes = integrationpage.MaintenanceBytes

func (s *Service) integrationPageService() *integrationpage.Service {
	return integrationpage.New(integrationpage.Deps{
		Repository: s.config.TenantRepository, Details: s.runDetailStore(), Now: s.config.now, Logger: s.config.Logger,
		Workflow: s.projectRecordWorkflow, RecordError: projectRecordError,
		Resolve: s.resolveNativeRequestScope, Scope: func(c echo.Context) hubapi.Scope { return s.featureScope(nativeRequestScope(c)) },
		ReadProject: func(ctx context.Context, scope hubapi.Scope) (projectstore.Record, error) {
			return s.readProjectRecord(ctx, nativeScope{organization: scope.Organization, project: scope.Project})
		},
		Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) },
		Audit: func(c echo.Context, _ hubapi.Scope) error {
			scope := nativeRequestScope(c)
			if scope.credential.Hosted != nil && (!hostedReadRequest(c) || scope.credential.Hosted.SupportActor != "") {
				return s.hostedAudit(c.Request().Context(), scope.credential.Hosted, "action", c.Request().Method+" "+c.Path(), string(scope.project), 0)
			}
			return nil
		},
	})
}
func (s *Service) readIntegrationPage(ctx context.Context, scope nativeScope) (ProjectIntegration, error) {
	value, err := s.integrationPageService().Read(ctx, s.featureScope(scope))
	return value, fromFeatureError(err)
}
func (s *Service) IntegrationPageMaintenanceFeature() hubmaintenance.Feature {
	return s.integrationPageService().MaintenanceFeature()
}
func (s *Service) MaintainIntegrationPage(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	return s.integrationPageService().Maintain(ctx, organization)
}
func (s *Service) getProjectIntegrationPage(c echo.Context) error {
	return s.integrationPageService().Get(c)
}
