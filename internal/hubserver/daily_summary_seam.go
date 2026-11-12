package hubserver

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/dailysummary"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type dailySummaryPreference = dailysummary.Preference

type dailySummaryReports struct{}

func (dailySummaryReports) ValidateUsage(facts []reportstore.UsageFact) error {
	_, err := postgresReportUsageRows(facts)
	return err
}

type dailySummaryWorkItems struct{ service *Service }

func (w dailySummaryWorkItems) AttemptSnapshot(ctx context.Context, reader tenantstore.Reader, attempt leasestore.Attempt, lease leasestore.Lease, event eventstore.Event) (any, error) {
	snapshot := tenantAttemptSnapshot{attempt: attempt, lease: lease, event: event}
	if err := readTenantAttemptPayload(ctx, reader, &snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (w dailySummaryWorkItems) Attempt(ctx context.Context, snapshot any, now time.Time) (tracker.NativeAttempt, error) {
	value, ok := snapshot.(tenantAttemptSnapshot)
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
		Now:              s.config.now,
		Organization:     s.accessOrganization,
		HostedPath:       s.hostedPath,
		IssuePath:        issueBrowserPath,
		RenderText:       healthEmailText,
		Reports:          dailySummaryReports{},
		WorkItems:        dailySummaryWorkItems{service: s},
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

func (s *Service) dailySummaryPreference(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	if credential.Hosted == nil || credential.Hosted.SupportActor != "" {
		return s.hostedJSONError(c, http.StatusForbidden, "Use your own account to change notification preferences")
	}
	summary := s.dailySummaryService()
	if err := summary.Accounts(); err != nil {
		return s.nativeAPIError(c, err)
	}
	var update *dailysummary.PreferenceUpdate
	if c.Request().Method == http.MethodPut {
		update = &dailysummary.PreferenceUpdate{}
		if err := decodeAPIJSON(c, update); err != nil {
			return s.invalidAPIRequest(c, err)
		}
	}
	value, err := summary.Preference(ctx, dailysummary.Member{User: credential.Hosted.Subject, Membership: credential.HostedMembership, Credential: credential.ID}, update)
	if err != nil {
		return s.nativeAPIError(c, fromFeatureError(err))
	}
	return c.JSON(http.StatusOK, value)
}

func (s *Service) unsubscribeDailySummary(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "no-referrer")
	token := c.QueryParam("token")
	if c.Request().Method == http.MethodPost {
		var request struct {
			Token string `json:"token"`
		}
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		token = request.Token
	}
	if !validNativeID(token, "unsub") {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err := s.dailySummaryService().Unsubscribe(c.Request().Context(), token); err != nil {
		return s.nativeAPIError(c, err)
	}
	if c.Request().Method == http.MethodPost {
		return c.NoContent(http.StatusNoContent)
	}
	return c.String(http.StatusOK, "You are unsubscribed from this organization's daily summary.")
}
