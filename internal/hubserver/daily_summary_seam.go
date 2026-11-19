package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/dailysummary"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type dailySummaryReports struct{}

func (dailySummaryReports) ValidateUsage(facts []reportstore.UsageFact) error {
	_, err := postgresReportUsageRows(facts)
	return err
}

type dailySummaryWorkItems struct{ service *Service }

func (w dailySummaryWorkItems) AttemptSnapshot(ctx context.Context, reader tenantstore.Reader, attempt leasestore.Attempt, lease leasestore.Lease, event eventstore.Event) (any, error) {
	snapshot := hubrecords.TenantAttemptSnapshot{Attempt: attempt, Lease: lease, Event: event}
	if err := hubrecords.ReadTenantAttemptPayload(ctx, reader, &snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (w dailySummaryWorkItems) Attempt(ctx context.Context, snapshot any, now time.Time) (tracker.NativeAttempt, error) {
	value, ok := snapshot.(hubrecords.TenantAttemptSnapshot)
	if !ok {
		return tracker.NativeAttempt{}, errors.New("daily summary attempt snapshot is unavailable")
	}
	return w.service.hydrateNativeAttemptTenant(ctx, value, now)
}

func (w dailySummaryWorkItems) Issue(ctx context.Context, row corestore.WorkItem, version eventstore.CollaborationVersion) (tracker.NativeIssue, error) {
	return w.service.hydrateWorkItemRecord(ctx, row, version)
}

func (s *Service) dailySummaryService() *dailysummary.Service {
	deps := dailysummary.Deps{
		Repository:       s.config.TenantRepository,
		AccessRepository: s.config.AccessRepository,
		Now:              s.config.Clock,
		Organization:     s.accessOrganization,
		HostedPath:       s.hostedPath,
		IssuePath:        hubrecords.IssueBrowserPath,
		RenderText: func(value string) string {
			return healthsignals.EmailText(failureFirstLine(value), maxFailureExampleLength)
		},
		Reports:   dailySummaryReports{},
		WorkItems: dailySummaryWorkItems{service: s},
	}
	if s.config.Hosted != nil {
		deps.PublicURL = s.config.Hosted.PublicURL
		deps.EmailSender = s.config.Hosted.EmailSender
	}
	return dailysummary.New(deps)
}

func (s *Service) deliverDailySummaries(ctx context.Context, now time.Time) error {
	return s.dailySummaryService().Deliver(ctx, now)
}

func (s *Service) dailySummaryHTTPDeps() dailysummary.HTTPDeps {
	return dailysummary.HTTPDeps{Service: s.dailySummaryService, Auth: s.requestAuth,
		Error:       func(c echo.Context, err error) error { return s.nativeAPIError(c, hubapi.FromFeatureError(err)) },
		HostedError: s.hostedAPIError, JSONError: s.hostedJSONError, InvalidRequest: s.invalidAPIRequest,
		ValidToken: func(token string) bool { return hubapi.ValidID(token, "unsub") }, Session: s.hostedSessionOnly,
	}
}
func (s *Service) registerDailySummaryPreferenceRoutes(e *echo.Echo) {
	dailysummary.RegisterPreferenceRoutes(e.Group(""), s.dailySummaryHTTPDeps())
}
func (s *Service) registerDailySummaryUnsubscribeRoute(e *echo.Echo) {
	dailysummary.RegisterUnsubscribeRoute(e.Group(""), s.dailySummaryHTTPDeps())
}
func (s *Service) registerDailySummarySharedUnsubscribeRoute(e *echo.Echo) {
	dailysummary.RegisterSharedUnsubscribeRoute(e.Group(""), s.dailySummaryHTTPDeps())
}
