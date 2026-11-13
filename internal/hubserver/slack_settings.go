package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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

func readSlackIntegration(ctx context.Context, owner postgres.SlackReader) (slackIntegrationStatus, error) {
	var result slackIntegrationStatus
	row, err := owner.SlackIntegration(ctx)
	if err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return result, err
	}
	if err == nil {
		result.ChannelName, result.LastDeliveryFailed, result.LastStatusCode = row.ChannelName, row.LastDeliveryFailed, int(row.LastStatusCode)
		if row.LastSuccessAt != nil {
			value := hubapi.FormatHubTime(*row.LastSuccessAt)
			result.LastSuccessAt = &value
		}
		if row.LastFailureAt != nil {
			value := hubapi.FormatHubTime(*row.LastFailureAt)
			result.LastFailureAt = &value
		}
	}
	_, err = owner.AccessSecret(ctx, "", slackWebhookSecret)
	if errors.Is(err, corestore.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Webhook = maskedSlackWebhook
	return result, nil
}

func (s *Service) slackSettingsScope(c echo.Context) (hubscope.NativeScope, error) {
	scope, err := s.organizationModelSelectionScope(c)
	if err != nil {
		return scope, err
	}
	if !hubscope.CanManageProjectSecrets(scope.Credential) {
		return scope, &hubapi.NativeError{Code: "forbidden", Message: "Slack settings require owner or admin access", Status: http.StatusForbidden, PublicMessage: true}
	}
	return scope, nil
}

func (s *Service) getSlackIntegration(c echo.Context) error {
	ctx, cancel := context.WithTimeout(c.Request().Context(), hubapi.ReadTimeout)
	defer cancel()
	scope, err := s.slackSettingsScope(c)
	if err != nil {
		return s.hostedAPIError(c, err)
	}
	var result slackIntegrationStatus
	err = s.accessRead(ctx, scope.Organization, func(r tenantstore.Reader) error {
		if err := s.authorizeAccessScope(ctx, r, scope, false); err != nil {
			return err
		}
		owner, ok := r.(postgres.SlackReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		result, err = readSlackIntegration(ctx, owner)
		return err
	})
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
	if hubapi.DecodeAPIJSON(c, &request) != nil {
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
			if s.config.SecretKeys == nil {
				return s.hostedJSONError(c, http.StatusServiceUnavailable, "Slack secret storage is unavailable")
			}
			envelope, err = s.config.SecretKeys.Seal(token, accessstore.SecretAAD(scope.Organization, "", slackWebhookSecret))
			if err != nil {
				return s.hostedJSONError(c, http.StatusServiceUnavailable, "Slack secret storage is unavailable")
			}
		}
	}
	ctx := c.Request().Context()
	var result slackIntegrationStatus
	err = s.accessSecretWrite(ctx, scope, func(w tenantstore.Writer) error {
		if err := w.LockAccessOrganization(ctx); err != nil {
			return err
		}
		owner, ok := w.(postgres.SlackWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		old, secretErr := w.AccessSecret(ctx, "", slackWebhookSecret)
		if secretErr != nil && !errors.Is(secretErr, corestore.ErrNotFound) {
			return secretErr
		}
		now := s.config.now()
		settings, err := owner.SlackIntegration(ctx)
		enabled := err == nil && settings.Enabled
		if request.Webhook != nil {
			enabled = len(token) > 0
		}
		if errors.Is(err, corestore.ErrNotFound) {
			_, err = owner.CreateSlackIntegration(ctx, postgres.CreateSlackIntegrationInput{OrganizationID: scope.Organization, ChannelName: request.ChannelName, Enabled: enabled, UpdatedAt: now})
		} else if err == nil {
			_, err = owner.UpdateSlackIntegration(ctx, postgres.UpdateSlackIntegrationInput{OrganizationID: scope.Organization, ChannelName: request.ChannelName, Enabled: enabled, UpdatedAt: now, Revision: settings.Revision})
		}
		if err != nil {
			return err
		}
		if request.Webhook != nil {
			event := "set"
			if secretErr == nil {
				event = "replace"
			}
			audit := accessstore.SecretAudit{OrganizationID: scope.Organization, Actor: secretActor(scope), Kind: slackWebhookSecret, Event: event, KeyVersion: envelope.Version, RecordedAt: now}
			if len(token) == 0 {
				if secretErr == nil {
					audit.Event, audit.KeyVersion = "remove", old.Envelope.Version
					err = w.DeleteAccessSecret(ctx, "", slackWebhookSecret, audit)
				}
			} else {
				err = w.StoreAccessSecret(ctx, accessstore.Secret{OrganizationID: scope.Organization, Kind: slackWebhookSecret, Envelope: envelope, UpdatedAt: now}, audit)
			}
			if err != nil {
				return err
			}
		}
		result, err = readSlackIntegration(ctx, owner)
		return err
	})
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
	err = s.accessSecretWrite(ctx, scope, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SlackReader)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		row, err := owner.SlackIntegration(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.Enabled {
			revision = row.Revision
		}
		return nil
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if revision == 0 {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Save a Slack webhook before sending a test message")
	}
	webhook, err := s.readSlackWebhook(ctx, scope.Organization, secretActor(scope))
	if err != nil {
		return s.hostedJSONError(c, http.StatusServiceUnavailable, "Slack secret storage is unavailable")
	}
	defer clear(webhook)
	if len(webhook) == 0 {
		return s.hostedJSONError(c, http.StatusUnprocessableEntity, "Save a Slack webhook before sending a test message")
	}
	statusCode, delivered := s.postSlack(ctx, webhook, "*Detent* · Test message")
	finishCtx, cancelFinish := context.WithTimeout(context.WithoutCancel(ctx), s.config.BusyTimeout)
	defer cancelFinish()
	var status slackIntegrationStatus
	err = s.accessWrite(finishCtx, scope.Organization, func(w tenantstore.Writer) error {
		owner, ok := w.(postgres.SlackWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		if err := owner.RecordSlackStatus(finishCtx, postgres.RecordSlackStatusInput{OrganizationID: scope.Organization, ExpectedRevision: revision, StatusCode: int32(statusCode), Failed: !delivered, At: s.config.now()}); err != nil && !errors.Is(err, corestore.ErrRevisionConflict) {
			return err
		}
		var err error
		status, err = readSlackIntegration(finishCtx, owner)
		return err
	})
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	return c.JSON(http.StatusOK, status)
}
