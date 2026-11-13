package hubserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) entitlementAdministrator(c echo.Context) bool {
	token, err := apiBearerToken(c)
	supplied := sha256.Sum256([]byte(token))
	configured := sha256.Sum256(s.config.Hosted.EntitlementAdminToken)
	return err == nil && len(s.config.Hosted.EntitlementAdminToken) >= 32 && subtle.ConstantTimeCompare(supplied[:], configured[:]) == 1
}

func (s *Service) hostedPlanReport(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
		return s.hostedJSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}
	after := int64(0)
	if value := c.QueryParam("history_after"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			return s.invalidAPIRequest(c, errors.New("invalid grant history cursor"))
		}
		after = parsed
	}
	limit := 100
	if value := c.QueryParam("history_limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 100 {
			return s.invalidAPIRequest(c, errors.New("grant history limit must be between 1 and 100"))
		}
		limit = parsed
	}
	report, err := s.database.hostedEntitlementReport(c.Request().Context(), after, limit)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, report)
}

func (s *Service) updateHostedPlan(c echo.Context) error {
	if !s.entitlementAdministrator(c) || c.Param("organization") != s.config.Hosted.OrganizationID {
		return s.hostedJSONError(c, http.StatusForbidden, "Entitlement administrator access is required")
	}

	var command hostedPlanCommand
	if err := decodeAPIJSON(c, &command); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	if err := s.database.applyHostedPlanCommand(c.Request().Context(), s.config.Hosted.EntitlementAdministrator, command); err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.NoContent(http.StatusNoContent)
}

func hostedRunnerTransactionMetrics(path string) []string {
	switch path {
	case nativeBase + "/leases/:lease/validate", nativeBase + "/claims/preview":
		return []string{"events_total", "usage_windows"}
	case nativeBase + "/machines/:machine/heartbeat":
		return []string{"connected_runners", "events_total", "usage_windows", "collaboration_bytes"}
	case enrollmentBase + "/redeem":
		return []string{"registered_runners", "connected_runners", "usage_windows"}
	case runnerBase + "/:runner/renew", runnerBase + "/:runner/rotate":
		return []string{"usage_windows"}
	}
	return nil
}

func hostedNativeMutationMetrics(input any, completion bool) []string {
	metrics := []string{"events_total", "usage_windows"}
	if _, runEvent := input.(tracker.NativeRunEvent); !runEvent {
		metrics = append(metrics, "unarchived_issues", "projects", "repositories", "registered_runners")
		if !completion {
			metrics = append(metrics, "connected_runners")
		}
	}
	if !completion {
		metrics = append(metrics, "collaboration_bytes", "history_records")
	}
	return metrics
}

func hostedCompletionMutation(c echo.Context, input any) bool {
	if strings.HasSuffix(c.Path(), "/work-items/:item/archive") {
		return true
	}
	if event, ok := input.(tracker.NativeRunEvent); ok {
		return event.Type == "run.finished" || event.Type == "run.checkpointed"
	}
	return false
}

func hostedMutationFeature(c echo.Context) string {
	if strings.Contains(c.Path(), "/integration") || strings.Contains(c.Path(), "/imports") {
		return "github_integration"
	}
	return "collaboration"
}
