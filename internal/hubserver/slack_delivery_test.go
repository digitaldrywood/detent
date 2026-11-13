package hubserver

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
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
	f.service.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(f.project.OrganizationID), PublicURL: "https://cloud.detent.test"}
	f.service.config.SecretKeys = secretTestKeys(t, "1", "1")
	if !configured {
		return
	}
	envelope, err := f.service.config.SecretKeys.Seal([]byte(slackTestWebhook), accessstore.SecretAAD(f.project.OrganizationID, "", slackWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.database.postgres.WriteSlack(t.Context(), f.project.OrganizationID, corestore.Serving, func(w postgres.SlackWriter) error {
		if _, err := w.CreateSlackIntegration(t.Context(), postgres.CreateSlackIntegrationInput{OrganizationID: f.project.OrganizationID, ChannelName: "#operations", Enabled: true, UpdatedAt: *now}); err != nil {
			return err
		}
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: f.project.OrganizationID, Kind: slackWebhookSecret, Envelope: envelope, UpdatedAt: *now}, accessstore.SecretAudit{OrganizationID: f.project.OrganizationID, Kind: slackWebhookSecret, Actor: "fixture", Event: "set", KeyVersion: envelope.Version, RecordedAt: *now})
	})
	if err != nil {
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
			finding := healthFinding{Signal: "runner_heartbeat_gap", Subject: healthSubject{Kind: test.kind, ID: test.id}, OpenedAt: now.Add(-time.Minute), Summary: "Failed with xoxb-123-SECRET sk-proj-SECRET ghp_SECRET github_pat_SECRET glpat-SECRET AIzaSECRET AKIA1234567890ABCDEF eyJabc.def.ghi " + slackTestWebhook, NextAction: "token=SECRET Bearer SECRET\n<@here> checks `raw log`."}
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
	f := newBrowserHostedOrganizationFixture(t, true, "org_browser_preview", func(cfg *Config) {
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
	var rows []accessstore.Secret
	err := f.service.accessRead(t.Context(), f.service.accessOrganization(), func(r tenantstore.Reader) error {
		secrets, err := r.AccessSecrets(t.Context(), "", "", 10)
		for _, secret := range secrets {
			if secret.Kind == slackWebhookSecret {
				rows = append(rows, secret)
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("disabled secret was retained")
	}
	org := f.service.accessOrganization()
	historicalAt := f.service.config.now().UTC().Truncate(time.Microsecond).Add(-time.Hour)
	details := &memoryRunDetails{}
	evidence, err := details.StoreDetail(t.Context(), string(org), []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	historical := make(map[string]postgres.SlackDelivery)
	for _, entry := range []struct {
		id, outcome, receipt string
		status               int32
	}{
		{"historical-uncertain", "uncertain", "", 0},
		{"historical-committed", "committed", "historical-ack", 200},
	} {
		if err := f.service.database.postgres.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
			_, err := w.CreateHealthRecord(t.Context(), postgres.CreateHealthRecordInput{OrganizationID: org, ID: entry.id, Fingerprint: entry.id, Signal: "example_signal", Class: "instance", SubjectKind: "runner", SubjectID: "example_runner", OpenedAt: historicalAt, LastSeenAt: historicalAt, Severity: "attention", Summary: "Historical finding", NextAction: "Inspect history", EvidenceObjectKey: evidence.Key, EvidenceSha256: evidence.SHA256, EvidenceBytes: evidence.CompressedSize, EvidenceContentBytes: evidence.UncompressedSize})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.service.database.postgres.WriteSlack(t.Context(), org, corestore.Background, func(w postgres.SlackWriter) error {
			if _, err := w.EnqueueSlackDelivery(t.Context(), postgres.EnqueueSlackDeliveryInput{OrganizationID: org, ID: entry.id, FindingID: entry.id, Event: "opened", EventAt: historicalAt, QueuedAt: historicalAt}); err != nil {
				return err
			}
			if _, err := w.ClaimSlackDelivery(t.Context(), postgres.ClaimSlackDeliveryInput{OrganizationID: org, ID: entry.id, ExpectedAttempts: 0, Now: historicalAt, NextAttemptAt: historicalAt.Add(time.Minute)}); err != nil {
				return err
			}
			row, err := w.FinishSlackDelivery(t.Context(), postgres.FinishSlackDeliveryInput{OrganizationID: org, ID: entry.id, ExpectedAttempts: 1, Outcome: entry.outcome, ExternalReceipt: entry.receipt, StatusCode: entry.status, At: historicalAt, NextAttemptAt: historicalAt.Add(time.Minute)})
			if err == nil {
				historical[entry.id] = row
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertHistory := func() {
		t.Helper()
		if err := f.service.database.postgres.ReadSlack(t.Context(), org, corestore.Serving, func(r postgres.SlackReader) error {
			for id, want := range historical {
				got, err := r.SlackDelivery(t.Context(), postgres.SlackDeliveryInput{OrganizationID: org, ID: id})
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("settings changed historical delivery: got=%+v want=%+v", got, want)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	readSettings := func() postgres.SlackIntegration {
		t.Helper()
		var row postgres.SlackIntegration
		if err := f.service.database.postgres.ReadSlack(t.Context(), org, corestore.Serving, func(r postgres.SlackReader) error {
			var err error
			row, err = r.SlackIntegration(t.Context())
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return row
	}
	totalSent := 0
	for _, test := range []struct {
		name                                    string
		status                                  int
		body                                    string
		transportError, changeSettings, success bool
	}{
		{name: "acknowledged", status: 200, body: "ok", success: true},
		{name: "transport uncertainty", transportError: true},
		{name: "server error", status: 503, body: "error"},
		{name: "definite rejection", status: 403, body: "action_prohibited"},
		{name: "redirect refused", status: 303, body: "redirect"},
		{name: "missing acknowledgment", status: 200, body: "unexpected"},
		{name: "oversized acknowledgment", status: 200, body: strings.Repeat("x", 4097)},
		{name: "settings changed during POST", status: 200, body: "ok", changeSettings: true, success: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.api(t, "owner", http.MethodPut, base, map[string]any{"webhook": slackTestWebhook, "channel_name": "operations"}, http.StatusOK)
			before := readSettings()
			sent := 0
			f.service.config.SlackHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) {
				sent++
				totalSent++
				if r.URL.String() != slackTestWebhook {
					t.Fatal("test message used a different webhook")
				}
				payload, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(payload), "SLACK_SECRET_SENTINEL") {
					t.Fatal("test message body exposed webhook")
				}
				if test.changeSettings {
					f.api(t, "owner", http.MethodPut, base, map[string]any{"channel_name": "changed"}, http.StatusOK)
				}
				if test.transportError {
					return nil, errors.New(slackTestWebhook)
				}
				header := make(http.Header)
				if test.status == 303 {
					header.Set("Location", "https://redirect.example.test")
				}
				return &http.Response{StatusCode: test.status, Status: http.StatusText(test.status), Body: io.NopCloser(strings.NewReader(test.body)), Header: header}, nil
			})}
			response := f.api(t, "owner", http.MethodPost, base+"/test", nil, http.StatusOK)
			if sent != 1 {
				t.Fatalf("explicit test message POST calls=%d", sent)
			}
			if strings.Contains(response.Body.String(), "SLACK_SECRET_SENTINEL") {
				t.Fatal("test response exposed webhook")
			}
			got := readSettings()
			if test.changeSettings {
				if got.ChannelName != "changed" || got.LastStatusCode != before.LastStatusCode || got.LastDeliveryFailed != before.LastDeliveryFailed || !reflect.DeepEqual(got.LastSuccessAt, before.LastSuccessAt) || !reflect.DeepEqual(got.LastFailureAt, before.LastFailureAt) {
					t.Fatalf("stale test status changed current settings: got=%+v before=%+v", got, before)
				}
			} else {
				if got.LastStatusCode != int32(test.status) || got.LastDeliveryFailed == test.success {
					t.Fatalf("test status=%+v", got)
				}
				if test.success && got.LastSuccessAt == nil || !test.success && got.LastFailureAt == nil {
					t.Fatalf("test outcome time missing=%+v", got)
				}
			}
			get := f.api(t, "owner", http.MethodGet, base, nil, http.StatusOK)
			if get.Header().Get("Cache-Control") != "no-store" || !strings.Contains(get.Body.String(), maskedSlackWebhook) {
				t.Fatal("settings read did not protect the webhook")
			}
			assertHistory()
		})
	}
	before := readSettings()
	if err := f.service.database.postgres.WriteSlack(t.Context(), org, corestore.Serving, func(w postgres.SlackWriter) error {
		_, err := w.UpdateSlackIntegration(t.Context(), postgres.UpdateSlackIntegrationInput{OrganizationID: org, ChannelName: before.ChannelName, Enabled: false, UpdatedAt: f.service.config.now(), Revision: before.Revision})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.api(t, "owner", http.MethodPut, base, map[string]any{"channel_name": "paused"}, http.StatusOK)
	if got := readSettings(); got.Enabled || got.ChannelName != "paused" {
		t.Fatalf("channel-only update enabled disabled integration=%+v", got)
	}
	f.api(t, "owner", http.MethodPost, base+"/test", nil, http.StatusUnprocessableEntity)
	f.api(t, "owner", http.MethodPut, base, map[string]any{"webhook": ""}, http.StatusOK)
	f.api(t, "owner", http.MethodPost, base+"/test", nil, http.StatusUnprocessableEntity)
	if err := f.service.accessRead(t.Context(), org, func(r tenantstore.Reader) error {
		_, err := r.AccessSecret(t.Context(), "", slackWebhookSecret)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("removed secret read=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertHistory()
	nextWebhook := strings.Replace(slackTestWebhook, "/T123/", "/T789/", 1)
	f.api(t, "owner", http.MethodPut, base, map[string]any{"webhook": nextWebhook}, http.StatusOK)
	assertHistory()
	if totalSent != 8 {
		t.Fatalf("settings changes sent additional messages=%d", totalSent)
	}
	if strings.Contains(logs.String(), "SLACK_SECRET_SENTINEL") {
		t.Fatal("logs exposed webhook")
	}

}

func TestSlackSecretRotation(t *testing.T) {
	t.Parallel()
	f := newPostgresBrowserHostedFixture(t, true)
	service := f.service
	if service.outbox != nil {
		service.outbox.stop()
	}
	service.workerCancel()
	<-service.workerDone
	<-service.secretRewrapDone
	service.config.SecretKeys = secretTestKeys(t, "1", "1")
	org := service.accessOrganization()
	admin := hostedPostgresAdmin(t, service)
	f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/integrations/slack", map[string]any{"webhook": slackTestWebhook, "channel_name": "operations"}, http.StatusOK)
	readSecrets := func(s *Service) []accessstore.Secret {
		t.Helper()
		var rows []accessstore.Secret
		if err := s.accessRead(t.Context(), org, func(r tenantstore.Reader) error {
			secrets, err := r.AccessSecrets(t.Context(), "", "", 10)
			for _, secret := range secrets {
				if secret.Kind == slackWebhookSecret {
					rows = append(rows, secret)
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	original := readSecrets(service)
	if len(original) != 1 || original[0].ProjectID != "" {
		t.Fatal("organization secret not in key validation")
	}
	service.config.SecretKeys = secretTestKeys(t, "2", "2")
	if err := service.checkAccessSecretKeys(t.Context()); err == nil {
		t.Fatal("missing organization secret key was accepted")
	}
	cfg := service.config
	cfg.SecretKeys = secretTestKeys(t, "2", "1", "2")
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	service = openTestService(t, cfg)
	<-service.secretRewrapDone
	if err := service.rewrapAccessSecrets(t.Context()); err != nil {
		t.Fatal(err)
	}
	value, err := service.readSlackWebhook(t.Context(), org, "test")
	if err != nil || string(value) != slackTestWebhook {
		t.Fatalf("rotated secret unavailable: %v", err)
	}
	clear(value)
	rotated := readSecrets(service)
	if len(rotated) != 1 || rotated[0].Envelope.Version != 2 || !bytes.Equal(rotated[0].Envelope.Ciphertext, original[0].Envelope.Ciphertext) {
		t.Fatal("rotation reencrypted value")
	}
	var actor, event string
	var version int
	if err := admin.QueryRowContext(t.Context(), "SELECT actor,event,key_version FROM organization_secret_audit WHERE organization_id=$1 AND kind=$2 AND event='rotate'", org, slackWebhookSecret).Scan(&actor, &event, &version); err != nil {
		t.Fatal(err)
	}
	if actor != "system:rewrap" || event != "rotate" || version != 2 {
		t.Fatal("incorrect rotation audit")
	}
}
