package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// registerHostedRoutes mounts the hosted surface. Since decisions section 11
// the React application owns every screen, so nothing here renders HTML: the
// sign-in exchange, the JSON organization API and the application shell.
func (s *Service) registerHostedRoutes(e *echo.Echo) {
	e.GET("/static/*", echo.WrapHandler(http.StripPrefix("/static/", http.FileServerFS(detent.StaticFS()))))
	e.GET("/auth/oidc/start", s.startHostedLogin)
	e.GET("/auth/oidc/callback", s.completeHostedLogin)
	e.GET("/invite", s.startHostedInvitation)
	e.POST("/logout", s.logoutHosted)
	e.POST("/webhooks/stripe", s.hostedStripeWebhook)
	e.GET("/api/cloud/metadata", s.hostedMetadata)
	e.GET("/api/cloud/billing", s.hostedBilling)
	e.GET("/api/cloud/billing/subscription", s.hostedBillingExport)
	e.POST("/api/v2/organizations/:organization/entitlements", s.updateHostedPlan)
	e.POST("/api/v2/organizations/:organization/artifact-allowances/:service", s.hostedArtifactAllowances)
	// The project event stream keeps its old path for one release
	// (decisions section 12).
	e.GET(nativeBase+"/events", s.hostedProjectEvents)
	e.GET("/projects/:project/events", s.hostedProjectEvents)
	s.registerHostedOrganizationRoutes(e)
	s.registerAppRoutes(e)
}

// hostedError answers with the native error shape. Every hosted surface is
// JSON since the Templ pages were removed (decisions section 12).
func (s *Service) hostedError(c echo.Context, status int, message string) error {
	return c.JSON(status, apiErrorResponse{Code: hostedErrorCode(status), Message: message})
}

func hostedErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request"
	case http.StatusTooManyRequests:
		return "allowance_exhausted"
	case http.StatusServiceUnavailable:
		return "unavailable"
	default:
		return "request_failed"
	}
}

// hostedProjectEvents streams the project's activity. It carries two kinds of
// frame: the original bare "activity" sequence, which every existing reader
// answers by re-fetching, and the typed events of section 12 -- today
// workspace.<state> (section 18.1) -- each with its own id and the resource as
// its body, so a client observes readiness by subscription and never by
// polling.
//
// The typed half is the conversation stream's design: the durable log is the
// queue, a subscriber is only woken and then reads from its own cursor, and a
// cursor below the retained window is answered with a close rather than a gap.
// The session is re-read on every wake, so a revoked grant closes the stream.
func (s *Service) hostedProjectEvents(c echo.Context) error {
	initial, status, err := s.hostedCredential(c)
	if err != nil {
		return c.NoContent(status)
	}
	organization := tracker.OrganizationID(s.config.Hosted.OrganizationID)
	project := tracker.ProjectID(c.Param("project"))
	initialScope := nativeScope{organization: organization, project: project, credential: initial}
	if err := s.requireHostedProject(c.Request().Context(), s.database.db, initialScope, false); err != nil {
		return c.NoContent(http.StatusForbidden)
	}
	if err := s.hostedAudit(c.Request().Context(), initial.Hosted, "action", "GET "+c.Path(), string(initialScope.project), http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	ctx := c.Request().Context()
	cursor, err := hostedProjectEventCursor(c)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	head, oldest, err := projectEventHead(ctx, s.database.db, organization, project)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
	c.Response().Header().Set("Cache-Control", "no-cache")
	c.Response().Header().Set("X-Accel-Buffering", "no")
	// A cursor ahead of the head, or behind everything still retained, cannot
	// be replayed exactly, so the client is told to re-snapshot instead of
	// being shown a stream with a hole in it.
	if cursor > head || cursor > 0 && oldest > 0 && cursor < oldest-1 {
		_, err := fmt.Fprintf(c.Response(), "event: closed\ndata: {\"reason\":\"cursor_expired\"}\n\n")
		c.Response().Flush()
		return err
	}
	var wake <-chan struct{}
	if s.workspaces != nil {
		subscription, cancel := s.workspaces.broker.subscribe(organization, project)
		defer cancel()
		wake = subscription
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		credential, _, err := s.hostedCredential(c)
		if err != nil {
			return nil
		}
		scope := nativeScope{organization: organization, project: project, credential: credential}
		if err := s.requireHostedProject(ctx, s.database.db, scope, false); err != nil {
			return nil
		}
		cursor, err = s.writeProjectEvents(c, scope, cursor)
		if err != nil {
			return nil
		}
		var sequence int64
		if err := s.database.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(event_sequence),0) FROM issues WHERE organization_id = ? AND project_id = ?", scope.organization, scope.project).Scan(&sequence); err != nil {
			return nil
		}
		if _, err := fmt.Fprintf(c.Response(), "event: activity\ndata: %d\n\n", sequence); err != nil {
			return nil
		}
		c.Response().Flush()
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
			// A transition committed; read from the cursor again straight
			// away rather than waiting out the tick.
		case <-ticker.C:
		}
	}
}

// hostedProjectEventCursor reads the typed-event cursor a subscriber resumes
// from: ?after= first, then the Last-Event-ID an EventSource replays with.
func hostedProjectEventCursor(c echo.Context) (int64, error) {
	raw := strings.TrimSpace(c.QueryParam("after"))
	if raw == "" {
		raw = strings.TrimSpace(c.Request().Header.Get("Last-Event-ID"))
	}
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, nativeInvalid("after must be a non-negative event sequence")
	}
	return value, nil
}

// writeProjectEvents drains the typed log from cursor and returns the new one.
func (s *Service) writeProjectEvents(c echo.Context, scope nativeScope, cursor int64) (int64, error) {
	ctx := c.Request().Context()
	for {
		events, err := listProjectEvents(ctx, s.database.db, scope.organization, scope.project, cursor, projectEventPage)
		if err != nil {
			return cursor, err
		}
		for _, event := range events {
			if _, err := fmt.Fprintf(c.Response(), "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Type, event.Data); err != nil {
				return cursor, err
			}
			cursor = event.Seq
		}
		c.Response().Flush()
		if len(events) < projectEventPage {
			return cursor, nil
		}
	}
}

func (s *Service) hostedMetadata(c echo.Context) error {
	if c.Request().Header.Get(echo.HeaderAuthorization) != "" {
		credential, _, err := s.authenticateAPIRequest(c)
		if err != nil || credential.ID != bootstrapTokenID || credential.Scope != apiScopeAdmin {
			return c.NoContent(http.StatusForbidden)
		}
	} else {
		session, _, err := s.hostedSession(c)
		if err != nil || session.Identity.SupportActor != "" || !hostedEmailListed(s.config.Hosted.StaffEmails, session.Email) {
			return c.NoContent(http.StatusForbidden)
		}
	}
	report, err := s.database.hostedMetadata(c.Request().Context())
	if err != nil {
		return c.JSON(http.StatusServiceUnavailable, apiErrorResponse{Code: "metadata_unavailable", Message: "Service metadata is temporarily unavailable"})
	}
	usage, err := s.hostedUsage(c.Request().Context(), report)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, usage)
}

type hostedUsageReport struct {
	Entitlement HostedEntitlement `json:"entitlement"`
	HostedMetadata
	PlanID            string `json:"plan_id"`
	StorageQuotaBytes int64  `json:"storage_quota_bytes,omitempty"`
	EventQuota        int64  `json:"event_quota,omitempty"`
}

func (s *Service) hostedUsage(ctx context.Context, report HostedMetadata) (hostedUsageReport, error) {
	entitlement, err := s.database.hostedPlanUsage(ctx, s.config.now())
	return hostedUsageReport{HostedMetadata: report, Entitlement: entitlement, PlanID: entitlement.EffectiveBase.ID, StorageQuotaBytes: entitlement.Allowances["collaboration_bytes"], EventQuota: entitlement.Allowances["ingested_events"]}, err
}

func (s *Service) hostedBilling(c echo.Context) error {
	credential, err := s.hostedBillingOwner(c)
	if err != nil {
		return s.nativeAPIError(c, auth.ErrHostedIdentity)
	}
	if err := s.hostedAudit(c.Request().Context(), credential.Hosted, "billing_viewed", "GET /api/cloud/billing", "", http.StatusOK); err != nil {
		return s.nativeAPIError(c, err)
	}
	report, err := s.database.hostedMetadata(c.Request().Context())
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	usage, err := s.hostedUsage(c.Request().Context(), report)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, usage)
}
