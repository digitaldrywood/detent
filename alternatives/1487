package accessinvites

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type HTTPOptions struct {
	Error   func(echo.Context, error) error
	Invalid func(echo.Context, error) error
	Refuse  func(echo.Context, int, string, string, any) error
}
type SharedInvitation struct {
	InvitationID string `json:"invitation_id"`
	Token        string `json:"token"`
}

func (s *Service) AcceptHTTP(c echo.Context, claims cloudassert.Claims, ok bool, options HTTPOptions) error {
	if !ok || claims.Kind != cloudassert.KindService {
		return options.Error(c, hubapi.NotFound())
	}
	var request SharedInvitation
	if err := hubapi.DecodeJSON(c, &request); err != nil || claims.Subject == "" || claims.Email == "" {
		return options.Invalid(c, errors.New("invalid invitation"))
	}
	identity := auth.Identity{Subject: claims.Subject, Email: claims.Email, EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: claims.Subject, SessionID: claims.ProviderSession}}
	var err error
	if request.InvitationID != "" && request.Token == "" {
		err = s.Accept(c.Request().Context(), identity, request.InvitationID, true)
	} else if request.InvitationID == "" {
		err = s.Accept(c.Request().Context(), identity, request.Token, false)
	} else {
		err = auth.ErrHostedIdentity
	}
	if err != nil && !errors.Is(s.deps.Classify(err), operatortool.ErrAccessDenied) {
		return options.Error(c, s.deps.Classify(err))
	}
	if err != nil {
		return options.Refuse(c, http.StatusForbidden, "invitation_unavailable", "This invitation is expired, already used, or intended for another account or organization", hubauth.Response{Code: "invitation_unavailable", Message: "This invitation is expired, already used, or intended for another account or organization"})
	}
	return c.NoContent(http.StatusNoContent)
}
