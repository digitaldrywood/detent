package cloudentry

import (
	"net/http"

	"github.com/labstack/echo/v4"
)

func (s *Service) unsubscribeIssueEmail(c echo.Context) error {
	return s.unsubscribeNotification(c, "/internal/v1/issue-notifications/unsubscribe", "You are unsubscribed from email updates for this issue.")
}

func (s *Service) unsubscribeDailySummary(c echo.Context) error {
	return s.unsubscribeNotification(c, "/internal/v1/daily-summary/unsubscribe", "You are unsubscribed from this organization's daily summary.")
}

func (s *Service) unsubscribeNotification(c echo.Context, path, message string) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "no-referrer")
	token := c.QueryParam("token")
	if len(token) != 38 {
		return c.String(http.StatusNotFound, "Unsubscribe link was not found.")
	}
	organization, err := s.readyOrganization(c.Request().Context(), c.Param("organization"))
	if err != nil {
		return c.String(http.StatusNotFound, "Unsubscribe link was not found.")
	}
	status, err := s.serviceRequest(c.Request().Context(), organization, path, map[string]string{"token": token}, nil)
	if err != nil || status >= http.StatusInternalServerError {
		return c.String(http.StatusServiceUnavailable, "Unsubscribe is temporarily unavailable. Please try again.")
	}
	if status != http.StatusNoContent {
		return c.String(http.StatusNotFound, "Unsubscribe link was not found.")
	}
	return c.String(http.StatusOK, message)
}
