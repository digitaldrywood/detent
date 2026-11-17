package dailysummary

import (
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
)

type HTTPDeps struct {
	Service        func() *Service
	Auth           func() *hubauth.Authenticator
	Error          func(echo.Context, error) error
	HostedError    func(echo.Context, error) error
	JSONError      func(echo.Context, int, string) error
	InvalidRequest func(echo.Context, error) error
	ValidToken     func(string) bool
	Session        echo.MiddlewareFunc
}
type HTTPService struct{ deps HTTPDeps }

func NewHTTP(deps HTTPDeps) *HTTPService { return &HTTPService{deps: deps} }
func RegisterPreferenceRoutes(e *echo.Group, deps HTTPDeps) {
	s := NewHTTP(deps)
	e.GET("/api/v2/organizations/:organization/daily-summary-preference", s.dailySummaryPreference, deps.Session)
	e.PUT("/api/v2/organizations/:organization/daily-summary-preference", s.dailySummaryPreference, deps.Session)
}
func RegisterUnsubscribeRoute(e *echo.Group, deps HTTPDeps) {
	s := NewHTTP(deps)
	e.GET("/daily-summary/unsubscribe", s.unsubscribeDailySummary)
}
func RegisterSharedUnsubscribeRoute(e *echo.Group, deps HTTPDeps) {
	s := NewHTTP(deps)
	e.POST("/internal/v1/daily-summary/unsubscribe", s.unsubscribeDailySummary)
}

func (s *HTTPService) dailySummaryPreference(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.deps.Auth().HostedCredential(ctx, c)
	if err != nil {
		return s.deps.HostedError(c, err)
	}
	if credential.Hosted == nil || credential.Hosted.SupportActor != "" {
		return s.deps.JSONError(c, http.StatusForbidden, "Use your own account to change notification preferences")
	}
	summary := s.deps.Service()
	if err := summary.Accounts(); err != nil {
		return s.deps.Error(c, err)
	}
	var update *PreferenceUpdate
	if c.Request().Method == http.MethodPut {
		update = &PreferenceUpdate{}
		if err := hubapi.DecodeJSON(c, update); err != nil {
			return s.deps.InvalidRequest(c, err)
		}
	}
	value, err := summary.Preference(ctx, Member{User: credential.Hosted.Subject, Membership: credential.HostedMembership, Credential: credential.ID}, update)
	if err != nil {
		return s.deps.Error(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func (s *HTTPService) unsubscribeDailySummary(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Referrer-Policy", "no-referrer")
	token := c.QueryParam("token")
	if c.Request().Method == http.MethodPost {
		var request struct {
			Token string `json:"token"`
		}
		if err := hubapi.DecodeJSON(c, &request); err != nil {
			return s.deps.InvalidRequest(c, err)
		}
		token = request.Token
	}
	if !s.deps.ValidToken(token) {
		return s.deps.Error(c, hubapi.NotFound())
	}
	if err := s.deps.Service().Unsubscribe(c.Request().Context(), token); err != nil {
		return s.deps.Error(c, err)
	}
	if c.Request().Method == http.MethodPost {
		return c.NoContent(http.StatusNoContent)
	}
	return c.String(http.StatusOK, "You are unsubscribed from this organization's daily summary.")
}
