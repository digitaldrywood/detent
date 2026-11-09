package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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

func (s *Service) readSlackIntegration(ctx context.Context, organization tracker.OrganizationID) (slackIntegrationStatus, error) {
	var result slackIntegrationStatus
	err := s.accessRead(ctx, organization, func(r tenantstore.Reader) error {
		owner, ok := r.(postgres.SlackReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		row, err := owner.SlackIntegration(ctx)
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		result.ChannelName, result.LastStatusCode, result.LastDeliveryFailed = row.ChannelName, int(row.LastStatusCode), row.LastDeliveryFailed
		if row.LastSuccessAt != nil {
			value := formatHubTime(*row.LastSuccessAt)
			result.LastSuccessAt = &value
		}
		if row.LastFailureAt != nil {
			value := formatHubTime(*row.LastFailureAt)
			result.LastFailureAt = &value
		}
		_, err = r.AccessSecret(ctx, "", slackWebhookSecret)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err == nil {
			result.Webhook = maskedSlackWebhook
		}
		return err
	})
	return result, err
}

func (s *Service) registerSlackRoutes(e *echo.Echo) {
	e.GET(hostedOrganizationBase+"/integrations/slack", s.getSlackIntegration, s.hostedSessionOnly)
	e.PUT(hostedOrganizationBase+"/integrations/slack", s.setSlackIntegration, s.hostedAdministrationSession)
	e.POST(hostedOrganizationBase+"/integrations/slack/test", s.testSlackIntegration, s.hostedAdministrationSession)
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
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubReadTimeout)
	defer cancel()
	scope, err := s.slackSettingsScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	result, err := s.readSlackIntegration(ctx, scope.organization)
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
	err = s.accessSecretWrite(ctx, scope, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SlackWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		row, err := owner.SlackIntegration(ctx)
		missing := errors.Is(err, corestore.ErrNotFound)
		if err != nil && !missing {
			return err
		}
		old, secretErr := w.AccessSecret(ctx, "", slackWebhookSecret)
		if secretErr != nil && !errors.Is(secretErr, corestore.ErrNotFound) {
			return secretErr
		}
		enabled := secretErr == nil
		if request.Webhook != nil {
			enabled = len(token) != 0
			audit := accessstore.SecretAudit{OrganizationID: scope.organization, Actor: secretActor(scope), Kind: slackWebhookSecret, Event: "set", KeyVersion: envelope.Version, RecordedAt: s.config.now()}
			if secretErr == nil {
				audit.Event = "replace"
			}
			if enabled {
				err = w.StoreAccessSecret(ctx, accessstore.Secret{OrganizationID: scope.organization, Kind: slackWebhookSecret, Envelope: envelope, UpdatedAt: audit.RecordedAt}, audit)
			} else if secretErr == nil {
				audit.Event, audit.KeyVersion = "remove", old.Envelope.Version
				err = w.DeleteAccessSecret(ctx, "", slackWebhookSecret, audit)
			}
			if err != nil {
				return err
			}
		}
		if missing {
			_, err = owner.CreateSlackIntegration(ctx, postgres.CreateSlackIntegrationInput{OrganizationID: scope.organization, ChannelName: request.ChannelName, Enabled: enabled, UpdatedAt: s.config.now()})
		} else {
			_, err = owner.UpdateSlackIntegration(ctx, postgres.UpdateSlackIntegrationInput{OrganizationID: scope.organization, ChannelName: request.ChannelName, Enabled: enabled, UpdatedAt: s.config.now(), Revision: row.Revision})
		}
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	result, err := s.readSlackIntegration(ctx, scope.organization)
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
	var revision int64
	if err := s.accessSecretWrite(ctx, scope, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SlackReader)
		if !ok {
			return workItemStorageUnavailable()
		}
		row, err := owner.SlackIntegration(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		revision = row.Revision
		return err
	}); err != nil {
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
	sent := s.postSlack(ctx, webhook, "*Detent* · Test message\nHealth findings that need attention will be posted here when they open and resolve.")
	if err := s.recordSlackTestResult(ctx, scope, revision, sent.status, sent.outcome == "committed"); err != nil {
		return s.nativeAPIError(c, err)
	}
	readCtx, cancelRead := context.WithTimeout(ctx, hubReadTimeout)
	defer cancelRead()
	result, err := s.readSlackIntegration(readCtx, scope.organization)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, result)
}

func (s *Service) recordSlackTestResult(ctx context.Context, scope nativeScope, revision int64, status int, delivered bool) error {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.accessSecretWrite(finishCtx, scope, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SlackWriter)
		if !ok {
			return workItemStorageUnavailable()
		}
		row, err := owner.SlackIntegration(finishCtx)
		if err != nil {
			return err
		}
		if row.Revision != revision {
			return nil
		}
		return owner.RecordSlackStatus(finishCtx, postgres.RecordSlackStatusInput{OrganizationID: scope.organization, ExpectedRevision: row.Revision, StatusCode: int32(status), Failed: !delivered, At: s.config.now()})
	})
}
