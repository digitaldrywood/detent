package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const slackWebhookSecret = "slack_webhook"
const maskedSlackWebhook = "https://hooks.slack.com/services/••••••••"

var slackWebhookPath = regexp.MustCompile(`^/services/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+/[A-Za-z0-9_-]+$`)
var slackChannelName = regexp.MustCompile(`^#?[\p{L}\p{N}_.-]{1,80}$`)

type slackIntegrationStatus struct {
	Webhook            string  `json:"webhook"`
	ChannelName        string  `json:"channel_name"`
	LastSuccessAt      *string `json:"last_success_at"`
	LastFailureAt      *string `json:"last_failure_at"`
	LastDeliveryFailed bool    `json:"last_delivery_failed"`
	LastStatusCode     int     `json:"last_status_code"`
}

func validSlackWebhook(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return u.Scheme == "https" && (u.Host == "hooks.slack.com" || u.Host == "hooks.slack-gov.com") && u.User == nil && u.RawQuery == "" && u.Fragment == "" && u.RawPath == "" && slackWebhookPath.MatchString(u.Path)
}

func readSlackIntegration(ctx context.Context, db nativeQueryer, organization tracker.OrganizationID) (slackIntegrationStatus, error) {
	var result slackIntegrationStatus
	var success, failure sql.NullString
	err := db.QueryRowContext(ctx, `SELECT channel_name,last_success_at,last_failure_at,last_delivery_failed,last_status_code FROM slack_integrations WHERE organization_id=?`, organization).Scan(&result.ChannelName, &success, &failure, &result.LastDeliveryFailed, &result.LastStatusCode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if success.Valid {
		result.LastSuccessAt = &success.String
	}
	if failure.Valid {
		result.LastFailureAt = &failure.String
	}
	var present bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM organization_secrets WHERE organization_id=? AND kind=?)`, organization, slackWebhookSecret).Scan(&present)
	if present {
		result.Webhook = maskedSlackWebhook
	}
	return result, err
}

func (s *Service) slackSettingsScope(c echo.Context) (nativeScope, error) {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return scope, err
	}
	if !canManageProjectSecrets(scope.credential) {
		return scope, &nativeError{Code: "forbidden", Message: "Slack settings require owner or admin access", status: http.StatusForbidden, publicMessage: true}
	}
	return scope, nil
}

func (s *Service) getSlackIntegration(c echo.Context) error {
	scope, err := s.slackSettingsScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	result, err := readSlackIntegration(c.Request().Context(), s.database.db, scope.organization)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, result)
}

func (s *Service) setSlackIntegration(c echo.Context) error {
	scope, err := s.slackSettingsScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	var request struct {
		Webhook     *string `json:"webhook"`
		ChannelName string  `json:"channel_name"`
	}
	if decodeAPIJSON(c, &request) != nil {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter a Slack incoming webhook URL and channel name")
	}
	request.ChannelName = strings.TrimSpace(request.ChannelName)
	if request.ChannelName != "" && !slackChannelName.MatchString(request.ChannelName) {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter a channel name of at most 80 letters, numbers, dots, dashes or underscores")
	}
	var token []byte
	var envelope hubsecrets.Envelope
	if request.Webhook != nil {
		value := strings.TrimSpace(*request.Webhook)
		request.Webhook = &value
		if value != "" {
			if !validSlackWebhook(value) {
				return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Enter a Slack incoming webhook URL")
			}
			token = []byte(value)
			defer clear(token)
			envelope, err = s.config.SecretKeys.Seal(token, secretAAD(string(scope.organization), "", slackWebhookSecret))
			if err != nil {
				return s.hostedJSONError(c, http.StatusServiceUnavailable, "Slack secret storage is unavailable")
			}
		}
	}
	ctx := c.Request().Context()
	err = s.secretMutation(ctx, scope, func(tx *sql.Tx) error {
		now := formatHubTime(s.config.now())
		if _, err := tx.ExecContext(ctx, `INSERT INTO slack_integrations(organization_id,channel_name) VALUES(?,?) ON CONFLICT(organization_id) DO UPDATE SET channel_name=excluded.channel_name`, scope.organization, request.ChannelName); err != nil {
			return err
		}
		if request.Webhook == nil {
			return nil
		}
		old, err := readSlackIntegration(ctx, tx, scope.organization)
		if err != nil {
			return err
		}
		event, version := "set", envelope.Version
		if old.Webhook != "" {
			event = "replace"
		}
		if len(token) == 0 {
			event = "remove"
			if old.Webhook != "" {
				if err := tx.QueryRowContext(ctx, `SELECT master_key_version FROM organization_secrets WHERE organization_id=? AND kind=?`, scope.organization, slackWebhookSecret).Scan(&version); err != nil {
					return err
				}
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM organization_secrets WHERE organization_id=? AND kind=?`, scope.organization, slackWebhookSecret); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE health_slack_deliveries SET completed_at=? WHERE completed_at IS NULL AND finding_id IN (SELECT id FROM health_findings WHERE organization_id=?)`, now, scope.organization); err != nil {
				return err
			}
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO organization_secrets(organization_id,kind,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(organization_id,kind) DO UPDATE SET ciphertext=excluded.ciphertext,nonce=excluded.nonce,wrapped_data_key=excluded.wrapped_data_key,master_key_version=excluded.master_key_version,updated_at=excluded.updated_at`, scope.organization, slackWebhookSecret, envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, now)
			if err != nil {
				return err
			}
		}
		return secretAudit(ctx, tx, string(scope.organization), "", secretActor(scope), slackWebhookSecret, event, version, now)
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err := readSlackIntegration(ctx, s.database.db, scope.organization)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) testSlackIntegration(c echo.Context) error {
	scope, err := s.slackSettingsScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	ctx := c.Request().Context()
	if err := s.secretMutation(ctx, scope, func(tx *sql.Tx) error { return nil }); err != nil {
		return s.nativeAPIError(c, err)
	}
	webhook, err := s.readSlackWebhook(ctx, scope.organization, secretActor(scope))
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Slack secret storage is unavailable")
	}
	defer clear(webhook)
	if len(webhook) == 0 {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Save a Slack webhook before sending a test message")
	}
	status, delivered := s.postSlack(ctx, webhook, "*Detent* · Test message\nHealth findings that need attention will be posted here when they open and resolve.")
	if err := s.recordSlackResult(ctx, s.database.db, scope.organization, "", status, delivered, s.config.now()); err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err := readSlackIntegration(ctx, s.database.db, scope.organization)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}
