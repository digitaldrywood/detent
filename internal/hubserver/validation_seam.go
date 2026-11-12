package hubserver

import (
	"context"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/validation"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
	"time"
)

func (s *Service) validationService() *validation.Service {
	return validation.New(validation.Deps{
		InvalidRequest: s.invalidAPIRequest,
		Repository:     s.config.TenantRepository, Now: s.config.now, SecretKeys: s.config.SecretKeys,
		Organization: func() tracker.OrganizationID {
			if s.config.Hosted == nil {
				return ""
			}
			return tracker.OrganizationID(s.config.Hosted.OrganizationID)
		},
		Scope: func(c echo.Context) hubauth.Input { return s.authInput(nativeRequestScope(c)) },
		Authorize: func(ctx context.Context, reader accessstore.Reader, scope hubauth.Input, write bool) error {
			return s.authorizeAccessScope(ctx, reader, nativeAuthInput(scope), write)
		},
		Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, fromFeatureError(err)) },
	})
}

func (s *Service) lookupGoTestReceipts(c echo.Context) error {
	return s.validationService().LookupReceipts(c)
}
func (s *Service) attestGoTestReceipts(c echo.Context) error {
	return s.validationService().AttestReceipts(c)
}
func (s *Service) recordGoTestReceipt(c echo.Context) error {
	return s.validationService().RecordReceipt(c)
}
func (s *Service) requireGoTestReceiptTenantAuthority(ctx context.Context, reader tenantstore.Reader, scope nativeScope, now time.Time) (string, error) {
	value, err := s.validationService().RequireReceiptAuthority(ctx, reader, s.authInput(scope), now)
	return value, fromFeatureError(err)
}
func (s *Service) expireGoTestReceipts(ctx context.Context, now time.Time, clock func() time.Time) error {
	return fromFeatureError(s.validationService().ExpireReceipts(ctx, now, clock))
}
