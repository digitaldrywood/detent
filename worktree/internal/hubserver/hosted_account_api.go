package hubserver

import (
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
)

type hostedNextResponse struct {
	Next string `json:"next"`
}

type hostedSupportView struct {
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	ExpiresAt time.Time `json:"expires_at"`
}

type hostedSupportResponse struct {
	Support hostedSupportView `json:"support"`
}

// hostedEntryOwned refuses the account flows the shared entry owns. Behind the
// shared entry, switching, joining and starting support are entry routes, so
// a tenant Hub answers them as absent rather than running a second copy.
func (s *Service) hostedEntryOwned(c echo.Context) (bool, error) {
	if s.hostedShared() {
		return true, s.nativeAPIError(c, nativeNotFound())
	}
	return false, nil
}

func (s *Service) hostedPlanJSON(c echo.Context) error {
	if _, _, err := s.hostedCredential(c.Request().Context(), c); err != nil {
		return s.hostedAPIError(c, err)
	}
	if _, err := s.hostedAdministrator(c); err != nil {
		return s.hostedJSONError(c, http.StatusForbidden, "Organization plan and usage require owner or admin access")
	}
	entitlement, err := s.database.hostedPlanUsage(c.Request().Context(), s.config.now())
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Plan information is temporarily unavailable")
	}
	return c.JSON(http.StatusOK, entitlement)
}

func (s *Service) switchHostedOrganizationJSON(c echo.Context) error {
	if owned, err := s.hostedEntryOwned(c); owned {
		return err
	}
	var request struct {
		hostedIdempotent
		Organization string `json:"organization"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, _, err := s.hostedSession(c.Request().Context(), c); err != nil {
		return s.hostedAPIError(c, err)
	}
	next, message := s.hostedSwitchDestination(c, strings.TrimSpace(request.Organization))
	if next == "" {
		return s.hostedJSONError(c, http.StatusForbidden, message)
	}
	return c.JSON(http.StatusOK, hostedNextResponse{Next: next})
}

func (s *Service) acceptHostedInvitationJSON(c echo.Context) error {
	if owned, err := s.hostedEntryOwned(c); owned {
		return err
	}
	var request struct {
		hostedIdempotent
		Token string `json:"token"`
	}
	if err := decodeAPIJSON(c, &request); err != nil {
		return invalidAPIRequest(c, err)
	}
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, _, err := s.hostedSession(c.Request().Context(), c); err != nil {
		return s.hostedAPIError(c, err)
	}
	if message, denial := s.acceptHostedInvitationToken(c, request.Token); message != "" && !s.hostedInvitationAcceptedBySession(c, request.Token) {
		denial.Status = http.StatusForbidden
		auth.LogHostedDenial(s.hostedAuthLogger, c.Response(), c.Request(), denial)
		return s.hostedJSONError(c, http.StatusForbidden, message)
	}
	return c.JSON(http.StatusOK, hostedNextResponse{Next: s.config.Hosted.PublicURL + "/auth/oidc/start"})
}

func (s *Service) startHostedSupportJSON(c echo.Context) error {
	if owned, err := s.hostedEntryOwned(c); owned {
		return err
	}
	var request hostedIdempotent
	if c.Request().ContentLength != 0 {
		if err := decodeAPIJSON(c, &request); err != nil {
			return invalidAPIRequest(c, err)
		}
	}
	if err := request.validate(false); err != nil {
		return s.nativeAPIError(c, err)
	}
	if _, _, err := s.hostedSession(c.Request().Context(), c); err != nil {
		return s.hostedAPIError(c, err)
	}
	session, transaction, status, message, reason := s.beginHostedSupport(c)
	if status != http.StatusOK {
		auth.LogHostedDenial(s.hostedAuthLogger, c.Response(), c.Request(), auth.HostedDenial{Flow: "support_start", Reason: reason, Status: status, Email: session.Email})
		return s.hostedJSONError(c, status, message)
	}
	return c.JSON(http.StatusOK, hostedSupportResponse{Support: hostedSupportView{Actor: session.Email, ExpiresAt: transaction.ExpiresAt.UTC()}})
}

// hostedInvitationAcceptedBySession reports whether the invitation named by
// token was already accepted by the signed-in account, so a retry after a lost
// response receives the same destination instead of a refusal.
func (s *Service) hostedInvitationAcceptedBySession(c echo.Context, token string) bool {
	session, _, err := s.hostedSession(c.Request().Context(), c)
	if err != nil || session.Identity.SupportActor != "" || token == "" || len(token) > 512 {
		return false
	}
	ctx := c.Request().Context()
	invitation, err := s.config.Hosted.Provider.Invitation(ctx, token)
	if err != nil || invitation.State != "accepted" || invitation.AcceptedUserID != session.Identity.Subject {
		return false
	}
	var accepted int
	err = s.database.db.QueryRowContext(ctx, "SELECT count(*) FROM hosted_invitations WHERE id = ? AND organization_id = ? AND accepted_user_id = ?", invitation.ID, s.config.Hosted.OrganizationID, session.Identity.Subject).Scan(&accepted)
	return err == nil && accepted == 1
}
