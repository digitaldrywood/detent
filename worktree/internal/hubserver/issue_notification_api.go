package hubserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/accountstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type issueNotificationPreference struct {
	Enabled bool `json:"enabled"`
}

func (s *Service) issueNotificationPreference(c echo.Context) error {
	ctx := c.Request().Context()
	credential, _, err := s.hostedCredential(ctx, c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	if credential.Hosted == nil || credential.Hosted.SupportActor != "" {
		return s.hostedJSONError(c, http.StatusForbidden, "Use your own account to change notification preferences")
	}
	if s.config.AccessRepository == nil {
		return s.nativeAPIError(c, errors.New("account repository unavailable"))
	}
	value := issueNotificationPreference{}
	if c.Request().Method == http.MethodPut {
		var request struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		if request.Enabled == nil {
			return s.nativeAPIError(c, nativePublicInvalid("An enabled preference is required"))
		}
		value.Enabled = *request.Enabled
		err = s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
			member, err := authorizeIssueNotificationMember(ctx, w, credential)
			if err != nil {
				return err
			}
			member.NotifyIssues = value.Enabled
			member.UpdatedAt = s.config.now()
			return w.UpdateAccessMember(ctx, member)
		})
	} else {
		readCtx, cancel := context.WithTimeout(ctx, hubReadTimeout)
		defer cancel()
		err = s.accessRead(readCtx, s.accessOrganization(), func(r tenantstore.Reader) error {
			member, err := authorizeIssueNotificationMember(readCtx, r, credential)
			value.Enabled = member.NotifyIssues
			return err
		})
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, value)
}

func authorizeIssueNotificationMember(ctx context.Context, r tenantstore.Reader, credential apiCredential) (accessstore.Member, error) {
	member, err := r.AccessMember(ctx, credential.Hosted.Subject)
	if errors.Is(err, corestore.ErrNotFound) {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	if err != nil {
		return accessstore.Member{}, err
	}
	if !member.Active || member.MembershipID != credential.HostedMembership || member.PrincipalID != credential.ID {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	principal, err := r.AccessCredential(ctx, member.PrincipalID)
	if errors.Is(err, corestore.ErrNotFound) {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	if err != nil {
		return accessstore.Member{}, err
	}
	if principal.RevokedAt != nil {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	return member, nil
}

func (s *Service) unsubscribeIssueEmail(c echo.Context) error {
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
	repository, ok := s.config.AccessRepository.(accountstore.NotificationRepository)
	if !ok {
		return s.nativeAPIError(c, errors.New("notification repository unavailable"))
	}
	if err := repository.UnsubscribeNotification(c.Request().Context(), s.accessOrganization(), token); err != nil {
		return s.nativeAPIError(c, err)
	}
	if c.Request().Method == http.MethodPost {
		return c.NoContent(http.StatusNoContent)
	}
	return c.String(http.StatusOK, "You are unsubscribed from email updates for this issue.")
}
