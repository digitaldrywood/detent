package hubserver

import (
	"context"
	"net/http"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/healthprojection"
	"github.com/digitaldrywood/detent/internal/hubserver/healthsignals"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthSubject = healthsignals.Subject
type healthEvidence = healthsignals.Evidence
type healthFinding = healthsignals.Finding
type healthRunner = healthsignals.Runner
type healthProject = healthsignals.Project
type healthWait = healthsignals.Wait
type healthSnapshot = healthsignals.Snapshot
type healthRateAttempt = healthsignals.RateAttempt
type healthItemCost = healthsignals.ItemCost
type healthRateProject = healthsignals.RateProject
type healthQueueEvidence = healthsignals.QueueEvidence
type healthBaselineUnavailable = healthsignals.BaselineUnavailable
type healthProductivityEvidence = healthsignals.ProductivityEvidence

const (
	healthReopenWindow    = healthsignals.ReopenWindow
	healthProjectLimit    = healthsignals.ProjectLimit
	healthBaselineWindow  = healthsignals.BaselineWindow
	healthLongWindow      = healthsignals.LongWindow
	healthBaselineSamples = healthsignals.BaselineSamples
)

func healthRecoveryReason(text string) string {
	return healthsignals.New(healthsignals.Deps{NormalizeFailureSignature: normalizeFailureSignature}).RecoveryReason(text)
}
func healthResidenceTimeline(t analyticsResidenceTimeline) healthsignals.ResidenceTimeline {
	out := healthsignals.ResidenceTimeline{Issue: healthsignals.ResidenceIssue{ID: t.Issue.Id, Partial: t.Issue.Partial}, State: t.State, Entered: t.Entered, Done: t.Done}
	if t.Spans != nil {
		out.Spans = make([]healthsignals.ResidenceSpan, len(t.Spans))
		for i, s := range t.Spans {
			out.Spans[i] = healthsignals.ResidenceSpan{Lane: s.Lane, From: s.From, To: s.To}
		}
	}
	return out
}

func (s *Service) healthProjection() *healthprojection.Service {
	var repository *postgres.Repository
	if s.database != nil {
		repository = s.database.postgres
	}
	return &healthprojection.Service{Postgres: repository, Tenants: s.config.TenantRepository, Details: s.runDetailStore(), Clock: s.config.Clock, Logger: s.config.Logger, SlackWebhookSecret: slackWebhookSecret}
}

func (s *Service) MaintainHealthPage(ctx context.Context, org tracker.OrganizationID) (bool, error) {
	return s.healthProjection().MaintainPage(ctx, org)
}

func (s *Service) readHealthFindings(ctx context.Context, scope hubscope.NativeScope, state, since, cursor string, limit int) (healthprojection.Page, error) {
	return s.healthProjection().Read(ctx, scope, state, since, cursor, limit)
}

func (s *Service) getHealthFindings(c echo.Context) error {
	limit := 100
	if raw := c.QueryParam("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return s.nativeAPIError(c, hubapi.NativePublicInvalid("limit must be between 1 and 100"))
		}
		limit = value
	}
	page, err := s.readHealthFindings(c.Request().Context(), hubscope.NativeRequestScope(c), c.QueryParam("state"), c.QueryParam("since"), c.QueryParam("cursor"), limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, page)
}
