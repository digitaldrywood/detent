package hubserver

import (
	"context"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/integrationpage"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const integrationPageMaintenanceBytes = integrationpage.MaintenanceBytes

func (s *Service) integrationPageDeps() integrationpage.Deps {
	return integrationpage.Deps{
		Repository: s.config.TenantRepository, Details: s.runDetailStore(), Now: s.config.Clock, Logger: s.config.Logger,
		Workflow: s.projectRecordWorkflow, RecordError: hubapi.ProjectRecordError,
	}
}
func projectIntegrationRecord(record projectstore.Record) ProjectIntegration {
	return integrationpage.Record(record)
}

func (s *Service) IntegrationPageMaintenanceFeature() hubmaintenance.Feature {
	return integrationpage.New(s.integrationPageDeps()).MaintenanceFeature()
}
func (s *Service) MaintainIntegrationPage(ctx context.Context, organization tracker.OrganizationID) (bool, error) {
	return integrationpage.New(s.integrationPageDeps()).Maintain(ctx, organization)
}
func (s *Service) getProjectIntegrationPage(c echo.Context) error {
	deps := s.integrationPageDeps()
	deps.Resolve = s.resolveNativeRequestScope
	deps.Scope = func(c echo.Context) hubapi.Scope { return s.authorization().Scope(hubscope.NativeRequestScope(c)) }
	deps.ReadProject = func(ctx context.Context, scope hubapi.Scope) (projectstore.Record, error) {
		return s.readProjectRecord(ctx, hubscope.NativeScope{Organization: scope.Organization, Project: scope.Project})
	}
	deps.Live = func(c echo.Context, value *projectstore.Integration) error {
		return s.applyProjectIntegrationLive(c.Request().Context(), hubscope.NativeRequestScope(c), value)
	}
	deps.Error = func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) }
	deps.Audit = func(c echo.Context, _ hubapi.Scope) error {
		scope := hubscope.NativeRequestScope(c)
		if scope.Credential.Hosted != nil && (!hubscope.HostedReadRequest(c) || scope.Credential.Hosted.SupportActor != "") {
			return s.hostedAudit(c.Request().Context(), scope.Credential.Hosted, "action", c.Request().Method+" "+c.Path(), string(scope.Project), 0)
		}
		return nil
	}
	return integrationpage.New(deps).Get(c)
}
