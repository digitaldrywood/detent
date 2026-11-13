package hubserver

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/releaseaudits"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) releaseAuditsService(caller nativeScope) *releaseaudits.Service {
	return releaseaudits.New(releaseaudits.Deps{
		Repository: s.config.TenantRepository, Now: s.config.now, Details: s.runDetailStore,
		Scope:  func(echo.Context) hubapi.Scope { return s.featureScope(caller) },
		Decode: hubapi.DecodeAPIJSON, InvalidRequest: s.invalidAPIRequest,
		Error:       func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		RecordError: func(err error) error { return hubapi.ProjectRecordError(hubapi.FromFeatureError(err)) },
		Recheck:     func(ctx context.Context, _ hubapi.Scope) error { return s.recheckProjectRecordMutation(ctx, caller) },
		RequireFeature: func(ctx context.Context, r tenantstore.Reader, feature string, now time.Time) error {
			return s.database.requireHostedFeatureRecord(ctx, r, feature, now)
		},
		Audit: func(hubapi.Scope) (accessstore.Audit, bool) {
			return projectRecordActor(caller).Audit, caller.credential.Hosted != nil
		},
	})
}

func (s *Service) registerReleaseAuditRoutes(e *echo.Echo) {
	authority := s.requireNativeScope(hubapi.APIScopeOperator)
	e.GET(hubapi.NativeBase+"/release-audits", func(c echo.Context) error { return s.releaseAuditsService(nativeRequestScope(c)).List(c) }, authority)
	e.GET(hubapi.NativeBase+"/release-audits/:checkpoint", func(c echo.Context) error { return s.releaseAuditsService(nativeRequestScope(c)).Get(c) }, authority)
	e.POST(hubapi.NativeBase+"/release-audits", func(c echo.Context) error { return s.releaseAuditsService(nativeRequestScope(c)).Publish(c) }, authority)
}
