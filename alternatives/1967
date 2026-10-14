package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

const slackTestWebhook = "https://hooks.slack.com/services/T123/B456/SLACK_SECRET_SENTINEL"

func configureTestSlack(t *testing.T, f nativeFixture, now *time.Time, configured bool) {
	t.Helper()
	f.service.stopHealthDetector()
	f.service.outbox.stop()
	f.service.config.now = func() time.Time { return *now }
	f.service.config.Hosted = &HostedConfig{PublicURL: "https://cloud.detent.test"}
	f.service.config.SecretKeys = secretTestKeys(t, "1", "1")
	if !configured {
		return
	}
	envelope, err := f.service.config.SecretKeys.Seal([]byte(slackTestWebhook), secretAAD(string(f.project.OrganizationID), "", slackWebhookSecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_secrets(organization_id,kind,ciphertext,nonce,wrapped_data_key,master_key_version,updated_at) VALUES(?,?,?,?,?,?,?)`, f.project.OrganizationID, slackWebhookSecret, envelope.Ciphertext, envelope.Nonce, envelope.WrappedKey, envelope.Version, formatHubTime(*now)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO slack_integrations(organization_id,channel_name) VALUES(?,'#operations')`, f.project.OrganizationID); err != nil {
		t.Fatal(err)
	}
}

func TestSlackFindingDelivery(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		watch, unset     bool
		failure          int
		timeout, recover bool
	}{
		{name: "open and resolve once"}, {name: "watch never posts", watch: true}, {name: "unset never calls HTTP", unset: true},
		{name: "4xx retries once", failure: 403}, {name: "5xx retries once", failure: 503}, {name: "redirect is not followed", failure: 302}, {name: "timeout retries once", timeout: true}, {name: "retry recovers", failure: 503, recover: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "slack")
			now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
			configureTestSlack(t, f, &now, !test.unset)
			finding := newHealthFinding("runner_heartbeat_gap", "instance", "runner", "runner_test", "Runner heartbeat is behind.", "Check the runner connection.", []string{string(f.project.ID)}, healthEvidence{})
			if test.watch {
				finding.Severity = "watch"
			}
			calls := []string{}
			f.service.config.SlackHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != slackTestWebhook || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
					t.Fatal("wrong Slack request")
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				var payload struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(raw, &payload); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "SLACK_SECRET_SENTINEL") {
					t.Fatal("webhook leaked into message")
				}
				calls = append(calls, payload.Text)
				if test.timeout {
					return nil, errors.New(slackTestWebhook)
				}
				if test.failure != 0 && (!test.recover || len(calls) == 1) {
					response := spritesTestResponse(slackTestWebhook, test.failure)
					response.Header.Set("Location", "https://outside.detent.test/secret")
					return response, nil
				}
				return spritesTestResponse("ok", 200), nil
			})}
			process := func() bool {
				t.Helper()
				processed, err := f.service.processSlackDelivery(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				return processed
			}
			applyTestHealth(t, f, now, []healthFinding{finding})
			if len(calls) != 0 {
				t.Fatal("detector called HTTP inline")
			}
			process()
			enabled := !test.watch && !test.unset
			want := 0
			if enabled {
				want = 1
			}
			if len(calls) != want {
				t.Fatalf("open calls=%d want %d", len(calls), want)
			}
			applyTestHealth(t, f, now, []healthFinding{finding})
			if process() {
				t.Fatal("repeated evaluation delivered again")
			}
			if test.failure != 0 || test.timeout {
				now = now.Add(59 * time.Second)
				if process() {
					t.Fatal("retry before 60 seconds")
				}
				now = now.Add(time.Second)
				if !process() || len(calls) != 2 {
					t.Fatalf("retry calls=%d", len(calls))
				}
				now = now.Add(time.Minute)
				if process() {
					t.Fatal("retried more than once")
				}
				page, err := f.service.readHealthFindings(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}, "open", "", "", 100)
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(page)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "SLACK_SECRET_SENTINEL") {
					t.Fatal("finding read leaked webhook")
				}
				failure := page.Items[0].SlackUnavailable
				if test.recover {
					if failure != nil {
						t.Fatalf("recovered finding=%+v", failure)
					}
				} else if failure == nil || failure.Code != "slack_unavailable" || failure.StatusCode != test.failure {
					t.Fatalf("failure=%+v", failure)
				}
				status, err := readSlackIntegration(t.Context(), f.service.database.db, f.project.OrganizationID)
				if err != nil {
					t.Fatal(err)
				}
				if status.LastDeliveryFailed == test.recover || status.LastFailureAt == nil {
					t.Fatalf("status=%+v", status)
				}
				return
			}
			now = now.Add(2 * time.Minute)
			applyTestHealth(t, f, now, nil)
			process()
			if enabled {
				want++
			}
			if len(calls) != want {
				t.Fatalf("resolve calls=%d want %d", len(calls), want)
			}
			applyTestHealth(t, f, now, nil)
			if process() {
				t.Fatal("repeated resolve delivered again")
			}
			now = now.Add(time.Minute)
			applyTestHealth(t, f, now, []healthFinding{finding})
			if process() {
				t.Fatal("reopen window posted again")
			}
			applyTestHealth(t, f, now, nil)
			if process() {
				t.Fatal("second resolve in reopen window posted again")
			}
			now = now.Add(healthReopenWindow + time.Second)
			applyTestHealth(t, f, now, []healthFinding{finding})
			process()
			if enabled {
				want++
			}
			if len(calls) != want {
				t.Fatalf("later occurrence calls=%d want %d", len(calls), want)
			}
			if enabled && (!strings.Contains(calls[0], "Opened") || !strings.Contains(calls[1], "Resolved") || !strings.Contains(calls[1], "Duration: 2m0s")) {
				t.Fatalf("messages=%v", calls)
			}
		})
	}
}

func TestSlackMessagePrivacyAndLinks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	for _, test := range []struct{ kind, id, path string }{
		{"work_item", "wi_test", "/work/i/wi_test?tab=diagnostics"}, {"runner", "runner_test", "/fleet"}, {"project", "prj_test", "/diagnostics"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			finding := healthFinding{Signal: "runner_heartbeat_gap", Subject: healthSubject{test.kind, test.id}, OpenedAt: now.Add(-time.Minute), Summary: "Failed with xoxb-123-SECRET sk-proj-SECRET ghp_SECRET github_pat_SECRET glpat-SECRET AIzaSECRET AKIA1234567890ABCDEF eyJabc.def.ghi " + slackTestWebhook, NextAction: "token=SECRET Bearer SECRET\n<@here> checks `raw log`."}
			message, err := renderSlackFinding("https://cloud.detent.test", "/organizations/org_test", finding, "opened", now)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"SECRET", "<@here>", "`raw log`", "xoxb-", "sk-proj-", "ghp_", "github_pat_", "Bearer", "eyJabc"} {
				if strings.Contains(message, secret) {
					t.Fatalf("message leaked %s: %s", secret, message)
				}
			}
			for _, part := range []string{"*runner_heartbeat_gap*", "https://cloud.detent.test/organizations/org_test" + test.path, "Next action:", "Age: 1m0s"} {
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
	f.service.stopHealthDetector()
	f.service.outbox.stop()
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
	rows, err := readSecretRows(t.Context(), f.service.database.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("disabled secret was retained")
	}
}

func TestSlackSecretRotation(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "slack-rotation")
	now := time.Now().UTC()
	configureTestSlack(t, f, &now, true)
	original, err := readSecretRows(t.Context(), f.service.database.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(original) != 1 || original[0].project != "" {
		t.Fatal("organization secret not in key validation")
	}
	if err := f.service.database.checkProjectSecretKeys(t.Context(), secretTestKeys(t, "2", "2")); err == nil {
		t.Fatal("missing organization secret key was accepted")
	}
	cfg := f.service.config
	cfg.Hosted = nil
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	cfg.SecretKeys = secretTestKeys(t, "2", "1", "2")
	count, err := RotateProjectSecrets(t.Context(), cfg, "operator:test")
	if err != nil || count != 1 {
		t.Fatalf("rotation=%d %v", count, err)
	}
	cfg.SecretKeys = secretTestKeys(t, "2", "2")
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	value, err := service.readSlackWebhook(t.Context(), f.project.OrganizationID, "test")
	if err != nil || string(value) != slackTestWebhook {
		t.Fatalf("rotated secret unavailable: %v", err)
	}
	clear(value)
	rotated, err := readSecretRows(t.Context(), service.database.db)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rotated[0].envelope.Ciphertext, original[0].envelope.Ciphertext) {
		t.Fatal("rotation reencrypted value")
	}
	var actor, event string
	var version int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT actor,event,key_version FROM organization_secret_audit WHERE event='rotate'").Scan(&actor, &event, &version); err != nil {
		t.Fatal(err)
	}
	if actor != "operator:test" || event != "rotate" || version != 2 {
		t.Fatal("incorrect rotation audit")
	}
}
