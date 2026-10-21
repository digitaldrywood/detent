package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/labstack/echo/v4"
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
		tx, err := s.database.db.BeginTx(ctx, nil)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "UPDATE hosted_members SET notify_issues=? WHERE user_id=?", value.Enabled, credential.Hosted.Subject); err != nil {
			return s.nativeAPIError(c, err)
		}
		if !value.Enabled {
			if _, err := tx.ExecContext(ctx, "DELETE FROM issue_notification_batches WHERE user_id=?", credential.Hosted.Subject); err != nil {
				return s.nativeAPIError(c, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return s.nativeAPIError(c, err)
		}
	} else {
		ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
		defer cancel()
		if err := s.database.reader.QueryRowContext(ctx, "SELECT notify_issues FROM hosted_members WHERE user_id=?", credential.Hosted.Subject).Scan(&value.Enabled); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return c.JSON(http.StatusOK, value)
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
	var item string
	err := s.database.db.QueryRowContext(c.Request().Context(), `UPDATE issue_subscriptions SET unsubscribed=1 WHERE token=? RETURNING work_item_id`, token).Scan(&item)
	if errors.Is(err, sql.ErrNoRows) {
		return s.nativeAPIError(c, nativeNotFound())
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if c.Request().Method == http.MethodPost {
		return c.NoContent(http.StatusNoContent)
	}
	return c.String(http.StatusOK, "You are unsubscribed from email updates for this issue.")
}
