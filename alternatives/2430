package hubserver

import (
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validDependencyReason(text string) bool {
	for index, char := range text {
		if (char == '.' || char == '!' || char == '?') && strings.HasPrefix(text[index+1:], " ") && strings.TrimSpace(text[index+1:]) != "" {
			return false
		}
	}
	return text == strings.TrimSpace(text) && utf8.ValidString(text) && !strings.ContainsFunc(text, unicode.IsControl) && utf8.RuneCountInString(text) <= 140 && !strings.ContainsAny(text, "\r\n\t<>[]*_`") && !strings.Contains(text, "://") && !strings.Contains(strings.ToLower(text), "mailto:") && !strings.Contains(strings.ToLower(text), "www.")
}

func (s *Service) updateNativeDependencyReason(c echo.Context) error {
	var request tracker.UpdateDependencyReason
	if err := hubapi.DecodeAPIJSON(c, &request); err != nil {
		return s.invalidAPIRequest(c, err)
	}
	scope := hubscope.NativeRequestScope(c)
	result, err := s.updateNativeDependencyReasonTenant(c.Request().Context(), scope, c.Param("item"), request)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSONBlob(http.StatusOK, result)
}
