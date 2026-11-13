package hubserver

import (
	"context"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/validation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) validationServiceForScope(caller hubscope.NativeScope) *validation.Service {
	return validation.New(validation.Deps{
		Details: s.runDetailStoreFor, HydrateWorkItem: s.hydrateCanonicalWorkItemRecord,
		FilterChange: func(ctx context.Context, scope hubapi.Scope, change tracker.ChangeRequest) (tracker.ChangeRequest, bool, error) {
			return s.filterChangeRecord(ctx, caller, change)
		},
		RecordError: hubapi.ProjectRecordError, OriginMatches: hubrecords.ScheduledRepairOriginMatches,

		InvalidRequest: s.invalidAPIRequest,
		Repository:     s.config.TenantRepository, Now: s.config.now, SecretKeys: s.config.SecretKeys,
		Organization: func() tracker.OrganizationID {
			if s.config.Hosted == nil {
				return ""
			}
			return tracker.OrganizationID(s.config.Hosted.OrganizationID)
		},
		Scope: func(c echo.Context) hubauth.Input { return s.authInput(hubscope.NativeRequestScope(c)) },
		Authorize: func(ctx context.Context, reader accessstore.Reader, scope hubauth.Input, write bool) error {
			return s.authorizeAccessScope(ctx, reader, nativeAuthInput(scope), write)
		},
		Error: func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
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
func (s *Service) expireGoTestReceipts(ctx context.Context, now time.Time, clock func() time.Time) error {
	return hubapi.FromFeatureError(s.validationService().ExpireReceipts(ctx, now, clock))
}

func (s *Service) readValidationAuditRecord(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeIssue, attempt *tracker.NativeAttempt) (*tracker.ValidationAudit, error) {
	service := s.validationServiceForScope(scope)
	value, err := service.AuditRecord(ctx, s.featureScope(scope), item, attempt)
	return value, hubapi.FromFeatureError(err)
}
func (s *Service) readValidationAuditTenant(ctx context.Context, scope hubscope.NativeScope, scheduledItem, localItem, changeID, attemptID string) (*tracker.ValidationAudit, error) {
	value, err := s.validationServiceForScope(scope).AuditTenant(ctx, s.featureScope(scope), scheduledItem, localItem, changeID, attemptID)
	return value, hubapi.FromFeatureError(err)
}

func (s *Service) validationService() *validation.Service {
	return s.validationServiceForScope(hubscope.NativeScope{})
}
