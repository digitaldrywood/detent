package hubserver

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const slackTestWebhook = "https://hooks.slack.com/services/T123/B456/SLACK_SECRET_SENTINEL"

func configureTestSlack(t *testing.T, f nativeFixture, now *time.Time, configured bool) {
	t.Helper()
	if f.service.outbox != nil {
		f.service.outbox.stop()
	}
	f.service.config.Clock = func() time.Time { return *now }
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
	historicalAt := f.service.config.Clock().UTC().Truncate(time.Microsecond).Add(-time.Hour)
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
					ctx := testenv.WaitContext(t, t.Context())
					if err := f.service.database.postgres.WriteSlack(ctx, org, corestore.Serving, func(w postgres.SlackWriter) error {
						current, err := w.SlackIntegration(ctx)
						if err != nil {
							return err
						}
						_, err = w.UpdateSlackIntegration(ctx, postgres.UpdateSlackIntegrationInput{OrganizationID: org, ChannelName: "changed", Enabled: current.Enabled, UpdatedAt: f.service.config.Clock(), Revision: current.Revision})
						return err
					}); err != nil {
						t.Fatalf("concurrent settings change: %v", err)
					}
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
		_, err := w.UpdateSlackIntegration(t.Context(), postgres.UpdateSlackIntegrationInput{OrganizationID: org, ChannelName: before.ChannelName, Enabled: false, UpdatedAt: f.service.config.Clock(), Revision: before.Revision})
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

func TestSlackTestMessageLeavesHostedWritesUnblocked(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedOrganizationFixture(t, true, "org_browser_preview", func(cfg *Config) {
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
	})
	if f.service.outbox != nil {
		f.service.outbox.stop()
	}
	org := f.service.accessOrganization()
	base := browserHostedOrganizationBase + "/integrations/slack"
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
	f.api(t, "owner", http.MethodPut, base, map[string]any{"webhook": slackTestWebhook, "channel_name": "operations"}, http.StatusOK)
	before := readSettings()
	inFlight := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	var requests sync.WaitGroup
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		requests.Wait()
	})
	f.service.config.SlackHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) {
		close(inFlight)
		<-release
		return &http.Response{StatusCode: http.StatusOK, Status: http.StatusText(http.StatusOK), Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})}
	serve := func(account, method, path string, body string) <-chan *httptest.ResponseRecorder {
		done := make(chan *httptest.ResponseRecorder, 1)
		var encoded []byte
		if body != "" {
			encoded = []byte(body)
		}
		requests.Go(func() { done <- f.serve(f.cookies[account], method, path, encoded) })
		return done
	}
	ctx := testenv.WaitContext(t, t.Context())
	await := func(name string, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		select {
		case response := <-done:
			return response
		case <-ctx.Done():
			t.Fatalf("%s did not finish while a Slack test message was in flight", name)
			return nil
		}
	}
	posted := serve("owner", http.MethodPost, base+"/test", "")
	select {
	case <-inFlight:
	case <-ctx.Done():
		t.Fatal("Slack test message never reached the webhook")
	}
	browserHostedStatus(t, await("another member's preference write", serve("viewer", http.MethodPut, browserHostedOrganizationBase+"/issue-notification-preference", `{"enabled":false}`)), http.StatusOK)
	browserHostedStatus(t, await("a Slack settings write", serve("owner", http.MethodPut, base, `{"channel_name":"changed"}`)), http.StatusOK)
	if got := readSettings(); got.ChannelName != "changed" || got.LastStatusCode != before.LastStatusCode {
		t.Fatalf("settings written during the test message=%+v", got)
	}
	releaseOnce.Do(func() { close(release) })
	browserHostedStatus(t, await("the Slack test message", posted), http.StatusOK)
	got := readSettings()
	if got.ChannelName != "changed" || got.LastStatusCode != before.LastStatusCode || got.LastDeliveryFailed != before.LastDeliveryFailed || !reflect.DeepEqual(got.LastSuccessAt, before.LastSuccessAt) || !reflect.DeepEqual(got.LastFailureAt, before.LastFailureAt) {
		t.Fatalf("stale test status changed current settings: got=%+v before=%+v", got, before)
	}
	f.service.config.SlackHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Status: http.StatusText(http.StatusOK), Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})}
	browserHostedStatus(t, f.serve(f.cookies["owner"], http.MethodPost, base+"/test", nil), http.StatusOK)
	if got := readSettings(); got.LastStatusCode != http.StatusOK || got.LastDeliveryFailed || got.LastSuccessAt == nil {
		t.Fatalf("current test status=%+v", got)
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
	service.workerCancel()
	select {
	case <-service.secretRewrapDone:
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("startup secret rewrap did not stop")
	}
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

func TestHealthSlackMaintenanceDeliversOnlyTheTenantQueueOnce(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		respond     func() (*http.Response, error)
		wantPending int
		retried     bool
		wantFailed  bool
		blocked     bool
	}{
		{name: "acknowledged", respond: func() (*http.Response, error) { return spritesTestResponse("ok", http.StatusOK), nil }},
		{name: "blocked transport leaves the writer free", respond: func() (*http.Response, error) { return spritesTestResponse("ok", http.StatusOK), nil }, blocked: true},
		{name: "uncertain transport", respond: func() (*http.Response, error) { return nil, errors.New("connection reset") }, wantFailed: true},
		{name: "definite rejection", respond: func() (*http.Response, error) { return spritesTestResponse("no_service", http.StatusNotFound), nil }, wantPending: 2, retried: true, wantFailed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := newPostgresNativeService(t, Config{RunDetails: &memoryRunDetails{}})
			now := service.config.Clock().UTC().Truncate(time.Microsecond)
			clock := now
			service.config.Clock = func() time.Time { return clock }
			primary := tracker.OrganizationID("org_first")
			organizations := []tracker.OrganizationID{primary, primary + "_second"}
			for _, org := range organizations {
				f := newNativeFixture(t, service, org, "Example")
				envelope, err := service.config.SecretKeys.Seal([]byte(slackTestWebhook), accessstore.SecretAAD(org, "", slackWebhookSecret))
				if err != nil {
					t.Fatal(err)
				}
				if err := service.database.postgres.WriteSlack(t.Context(), org, corestore.Serving, func(w postgres.SlackWriter) error {
					if _, err := w.CreateSlackIntegration(t.Context(), postgres.CreateSlackIntegrationInput{OrganizationID: org, ChannelName: "example", Enabled: true, UpdatedAt: now}); err != nil {
						return err
					}
					return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: slackWebhookSecret, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: slackWebhookSecret, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
				}); err != nil {
					t.Fatal(err)
				}
				findings := []healthFinding{
					newHealthFinding("example_first", "instance", "organization", string(org), "Example first finding", "Inspect the example", []string{string(f.project.ID)}, healthEvidence{}),
					newHealthFinding("example_second", "instance", "organization", string(org), "Example second finding", "Inspect the example", []string{string(f.project.ID)}, healthEvidence{}),
				}
				if err := service.publishHealthEvaluation(t.Context(), org, now, findings); err != nil {
					t.Fatal(err)
				}
			}
			service.config.Hosted = &hostedplans.HostedConfig{OrganizationID: string(primary), PublicURL: "https://cloud.detent.test"}
			var bodies []string
			service.config.SlackHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != slackTestWebhook {
					t.Fatalf("Slack POST used another webhook: %s", r.URL.Redacted())
				}
				payload, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				bodies = append(bodies, string(payload))
				return test.respond()
			})}
			scheduler, err := service.newReadModelMaintenance()
			if err != nil {
				t.Fatal(err)
			}
			rotate := func() {
				t.Helper()
				for range 32 {
					_ = scheduler.Tick(t.Context())
				}
			}
			pending := func(org tracker.OrganizationID) []postgres.SlackDelivery {
				t.Helper()
				var rows []postgres.SlackDelivery
				if err := service.database.postgres.ReadSlack(t.Context(), org, corestore.Serving, func(r postgres.SlackReader) error {
					var err error
					rows, err = r.PendingSlackDeliveries(t.Context(), postgres.PendingSlackDeliveriesInput{OrganizationID: org, NextAttemptAt: clock.Add(24 * time.Hour), Limit: 10})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return rows
			}
			if test.blocked {
				arrived, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var released, signalled sync.Once
				t.Cleanup(func() { released.Do(func() { close(release) }) })
				respond := test.respond
				test.respond = func() (*http.Response, error) {
					signalled.Do(func() { close(arrived) })
					<-release
					return respond()
				}
				go func() { defer close(done); rotate() }()
				<-arrived
				if err := service.database.postgres.WriteSlack(t.Context(), primary, corestore.Serving, func(postgres.SlackWriter) error { return nil }); err != nil {
					t.Fatalf("database writer unavailable while Slack was blocked: %v", err)
				}
				released.Do(func() { close(release) })
				<-done
			} else {
				rotate()
			}
			if len(bodies) != 2 {
				t.Fatalf("Slack POSTs after first rotation=%d %q", len(bodies), bodies)
			}
			for _, body := range bodies {
				if !strings.Contains(body, "example_") || !strings.Contains(body, "https://cloud.detent.test/diagnostics") || !strings.Contains(body, "Opened") {
					t.Fatalf("Slack message=%s", body)
				}
			}
			if got := pending(primary); len(got) != test.wantPending {
				t.Fatalf("primary pending deliveries=%+v", got)
			}
			if got := pending(organizations[1]); len(got) != 2 {
				t.Fatalf("other tenant deliveries were touched: %+v", got)
			}
			var records []postgres.HealthRecord
			if err := service.database.postgres.ReadHealth(t.Context(), primary, corestore.Serving, func(r postgres.HealthReader) error {
				var err error
				records, err = r.HealthEvaluationRecords(t.Context(), postgres.HealthEvaluationRecordsInput{OrganizationID: primary, Limit: 10})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				if (record.SlackUnavailableAt != nil) != test.wantFailed {
					t.Fatalf("finding Slack status=%v want failed=%t", record.SlackUnavailableAt, test.wantFailed)
				}
			}
			rotate()
			if len(bodies) != 2 {
				t.Fatalf("second rotation repeated Slack POSTs=%d", len(bodies))
			}
			clock = clock.Add(time.Hour)
			rotate()
			want := 2
			if test.retried {
				want = 4
			}
			if len(bodies) != want {
				t.Fatalf("Slack POSTs after retry window=%d want %d", len(bodies), want)
			}
		})
	}
}
