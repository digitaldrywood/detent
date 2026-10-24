package web

import (
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/intake"
	"github.com/digitaldrywood/detent/internal/project"
)

const intakeWebhookMaxBodyBytes = 2 << 20

func (s *Server) intakeWebhook(c echo.Context) error {
	projectID := strings.TrimSpace(c.Param("project_id"))
	trackedProject, ok := s.registry.Get(project.ID(projectID))
	if !ok {
		return c.JSON(http.StatusNotFound, errorResponse("project_not_found", "Project not found"))
	}
	manager := trackedProject.Intake()
	if manager == nil || !manager.Enabled() {
		return c.JSON(http.StatusNotFound, errorResponse("intake_not_configured", "Intake is not configured for this project"))
	}
	sourceName := strings.TrimSpace(c.Param("source"))
	source, ok := manager.Source(sourceName)
	if !ok || source.Kind == intake.KindSchedule {
		return c.JSON(http.StatusNotFound, errorResponse("intake_source_not_found", "Webhook intake source not found"))
	}
	secret := s.resolveGitHubWebhookSecret(source.Secret)
	if secret == "" {
		return c.JSON(http.StatusServiceUnavailable, errorResponse("intake_secret_unavailable", "Webhook intake secret is unavailable"))
	}
	if !validIntakeToken(secret, intakeToken(c.Request())) {
		return c.JSON(http.StatusUnauthorized, errorResponse("invalid_intake_token", "Webhook intake token is invalid"))
	}

	request := c.Request()
	request.Body = http.MaxBytesReader(c.Response(), request.Body, intakeWebhookMaxBodyBytes)
	payload, err := io.ReadAll(request.Body)
	if err != nil {
		return c.JSON(http.StatusBadRequest, errorResponse("invalid_payload", "Webhook intake payload is invalid"))
	}
	result, err := manager.IngestWebhook(request.Context(), sourceName, payload)
	if err != nil {
		return intakeWebhookError(c, err)
	}
	if !result.Matched {
		return c.JSON(http.StatusAccepted, result)
	}
	s.requestIntakeRefresh(request, trackedProject, result)
	if result.Created {
		return c.JSON(http.StatusCreated, result)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Server) requestIntakeRefresh(request *http.Request, trackedProject *project.Project, result intake.Result) {
	if s.refresher == nil || trackedProject == nil || !result.Matched {
		return
	}
	workflow := trackedProject.Workflow().Config
	_, err := s.requestWebhookRefresh(request.Context(), RefreshTarget{
		Repository:  workflow.Tracker.Repository,
		IssueNumber: result.Issue.Number,
		ProjectIDs:  []string{string(trackedProject.ID())},
		Event:       "intake:" + result.Source,
	})
	if err != nil {
		s.logger.WarnContext(request.Context(), "intake refresh request failed", "project_id", trackedProject.ID(), "source", result.Source, "error", err)
	}
}

func intakeWebhookError(c echo.Context, err error) error {
	switch {
	case errors.Is(err, intake.ErrSourceNotFound), errors.Is(err, intake.ErrSourceNotWebhook):
		return c.JSON(http.StatusNotFound, errorResponse("intake_source_not_found", "Webhook intake source not found"))
	case errors.Is(err, intake.ErrInvalidPayload), errors.Is(err, intake.ErrMissingFingerprint):
		return c.JSON(http.StatusUnprocessableEntity, errorResponse("invalid_intake_event", err.Error()))
	default:
		return c.JSON(http.StatusBadGateway, errorResponse("intake_failed", "Webhook intake failed"))
	}
}

func intakeToken(request *http.Request) string {
	if request == nil {
		return ""
	}
	if token := strings.TrimSpace(request.Header.Get("X-Detent-Intake-Token")); token != "" {
		return token
	}
	authorization := strings.TrimSpace(request.Header.Get("Authorization"))
	if token, ok := strings.CutPrefix(authorization, "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	return ""
}

func validIntakeToken(secret string, token string) bool {
	secret = strings.TrimSpace(secret)
	token = strings.TrimSpace(token)
	if secret == "" || token == "" || len(secret) != len(token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(secret), []byte(token)) == 1
}
