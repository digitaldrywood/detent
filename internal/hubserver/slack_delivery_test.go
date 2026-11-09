package hubserver

import (
	"bytes"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

const slackTestWebhook = "https://hooks.slack.com/services/T123/B456/SLACK_SECRET_SENTINEL"

func configureTestSlack(t *testing.T, f nativeFixture, now *time.Time, configured bool) {
	t.Helper()
	if f.service.outbox != nil {
		f.service.outbox.stop()
	}
	f.service.config.now = func() time.Time { return *now }
	if f.service.config.Hosted == nil {
		f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID), WorkOSOrganizationID: "provider_" + string(f.project.OrganizationID), Provider: newHostedSecurityProvider()}
	}
	f.service.config.Hosted.PublicURL = "https://cloud.detent.test"
	f.service.config.SecretKeys = secretTestKeys(t, "1", "1")
	if !configured {
		return
	}
	envelope, err := f.service.config.SecretKeys.Seal([]byte(slackTestWebhook), secretAAD(string(f.project.OrganizationID), "", slackWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.postgres.WriteSlack(t.Context(), f.project.OrganizationID, corestore.Serving, func(w postgres.SlackWriter) error {
		if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: f.project.OrganizationID, Kind: slackWebhookSecret, Envelope: envelope, UpdatedAt: *now}, accessstore.SecretAudit{OrganizationID: f.project.OrganizationID, Kind: slackWebhookSecret, Actor: "fixture", Event: "set", KeyVersion: envelope.Version, RecordedAt: *now}); err != nil {
			return err
		}
		_, err := w.CreateSlackIntegration(t.Context(), postgres.CreateSlackIntegrationInput{OrganizationID: f.project.OrganizationID, ChannelName: "#operations", Enabled: true, UpdatedAt: *now})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSlackMessagePrivacyAndLinks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	for _, test := range []struct{ kind, id, path string }{
		{"work_item", "wi_test", "/example-org/example-project/issues/1?tab=diagnostics"}, {"runner", "runner_test", "/fleet"}, {"project", "prj_test", "/diagnostics"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			finding := healthFinding{Signal: "runner_heartbeat_gap", Subject: healthSubject{test.kind, test.id}, OpenedAt: now.Add(-time.Minute), Summary: "Failed with xoxb-123-SECRET sk-proj-SECRET ghp_SECRET github_pat_SECRET glpat-SECRET AIzaSECRET AKIA1234567890ABCDEF eyJabc.def.ghi " + slackTestWebhook, NextAction: "token=SECRET Bearer SECRET\n<@here> checks `raw log`."}
			message, err := renderSlackFinding("https://cloud.detent.test", "/example-org", finding, "opened", now, func() string {
				if test.kind == "work_item" {
					return "/example-org/example-project/issues/1"
				}
				return ""
			}())
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"SECRET", "<@here>", "`raw log`", "xoxb-", "sk-proj-", "ghp_", "github_pat_", "Bearer", "eyJabc"} {
				if strings.Contains(message, secret) {
					t.Fatalf("message leaked %s: %s", secret, message)
				}
			}
			for _, part := range []string{"*runner_heartbeat_gap*", "https://cloud.detent.test" + func() string {
				if test.kind == "work_item" {
					return test.path
				}
				return "/example-org" + test.path
			}(), "Next action:", "Age: 1m0s"} {
				if !strings.Contains(message, part) {
					t.Fatalf("missing %s: %s", part, message)
				}
			}
		})
	}
}

func TestSlackSettingsPrivacyAndAccess(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	f := newPostgresBrowserHostedOrganizationFixture(t, true, "org_browser_preview", func(cfg *Config) {
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
		cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	calls := 0
	if f.service.outbox != nil {
		f.service.outbox.stop()
	}
	f.service.config.SlackHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New(slackTestWebhook) })}
	base := browserHostedOrganizationBase + "/integrations/slack"
	for _, test := range []struct {
		role, method, path string
		body               any
		status             int
	}{
		{"viewer", "GET", base, nil, 403}, {"viewer", "PUT", base, map[string]any{"webhook": slackTestWebhook}, 403}, {"viewer", "POST", base + "/test", nil, 403},
		{"owner", "PUT", base, map[string]any{"webhook": "http://localhost/private"}, 422}, {"owner", "PUT", base, map[string]any{"webhook": "https://hooks.slack.com:443/services/T/B/S"}, 422},
		{"owner", "PUT", base, map[string]any{"webhook": "https://hooks.slack.com.evil.test/services/T/B/S"}, 422},
		{"owner", "PUT", base, map[string]any{"webhook": "https://user:secret@hooks.slack.com/services/T/B/S"}, 422},
		{"owner", "PUT", base, map[string]any{"webhook": "https://hooks.slack.com/services/T/B/S?token=secret"}, 422},
		{"owner", "PUT", base, map[string]any{"webhook": slackTestWebhook, "channel_name": "#operations"}, 200}, {"owner", "GET", base, nil, 200}, {"owner", "POST", base + "/test", map[string]any{}, 200},
		{"owner", "PUT", base, map[string]any{"channel_name": "#team"}, 200}, {"owner", "PUT", base, map[string]any{"webhook": ""}, 200}, {"owner", "POST", base + "/test", map[string]any{}, 422},
	} {
		response := f.api(t, test.role, test.method, test.path, test.body, test.status)
		if strings.Contains(response.Body.String(), "SLACK_SECRET_SENTINEL") {
			t.Fatal("response leaked webhook")
		}
		if test.method == "GET" && test.role == "owner" && !strings.Contains(response.Body.String(), maskedSlackWebhook) {
			t.Fatal("settings read did not mask webhook")
		}
	}
	if calls != 1 {
		t.Fatalf("HTTP calls=%d", calls)
	}
	if strings.Contains(logs.String(), "SLACK_SECRET_SENTINEL") {
		t.Fatal("logs leaked webhook")
	}
	if err := f.service.accessRead(t.Context(), f.service.accessOrganization(), func(r tenantstore.Reader) error {
		_, err := r.AccessSecret(t.Context(), "", slackWebhookSecret)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("disabled secret was retained: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSlackSecretRotation(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "slack-rotation")
	now := time.Now().UTC()
	configureTestSlack(t, f, &now, true)
	var original accessstore.Secret
	if err := f.service.accessRead(t.Context(), f.project.OrganizationID, func(r tenantstore.Reader) error {
		var err error
		original, err = r.AccessSecret(t.Context(), "", slackWebhookSecret)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if original.ProjectID != "" {
		t.Fatal("organization secret has project scope")
	}
	if err := f.service.checkAccessSecretKeys(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.service.config.SecretKeys = secretTestKeys(t, "2", "2")
	if err := f.service.checkAccessSecretKeys(t.Context()); err == nil {
		t.Fatal("missing organization secret key was accepted")
	}
	cfg := f.service.config
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.SecretKeys = secretTestKeys(t, "2", "1", "2")
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if err := service.rewrapAccessSecrets(t.Context()); err != nil {
		t.Fatal(err)
	}
	value, err := service.readSlackWebhook(t.Context(), f.project.OrganizationID, "test")
	if err != nil || string(value) != slackTestWebhook {
		t.Fatalf("rotated secret unavailable: %v", err)
	}
	clear(value)
	var rotated accessstore.Secret
	if err := service.accessRead(t.Context(), f.project.OrganizationID, func(r tenantstore.Reader) error {
		var err error
		rotated, err = r.AccessSecret(t.Context(), "", slackWebhookSecret)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rotated.Envelope.Ciphertext, original.Envelope.Ciphertext) {
		t.Fatal("rotation reencrypted value")
	}
	if rotated.Envelope.Version != 2 {
		t.Fatal("rotation did not update the key version")
	}
	stored, _ := projectRecordDatabases.Load(f.service.config.TenantRepository)
	var actor, event string
	var version int
	if err := stored.(*sql.DB).QueryRowContext(t.Context(), "SELECT actor,event,key_version FROM organization_secret_audit WHERE organization_id=$1 AND kind=$2 AND event='rotate'", f.project.OrganizationID, slackWebhookSecret).Scan(&actor, &event, &version); err != nil {
		t.Fatal(err)
	}
	if actor != "system:rewrap" || event != "rotate" || version != 2 {
		t.Fatal("incorrect rotation audit")
	}
}
