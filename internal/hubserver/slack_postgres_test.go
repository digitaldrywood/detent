package hubserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSlackPostgresTenantDeliveryAndUncertainReplay(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		details := &memoryRunDetails{}
		f, owner := newChangePolicyPostgresTenantFixture(t, repository, org, details)
		f.service.workerCancel()
		<-f.service.workerDone
		<-f.service.secretRewrapDone
		if f.service.outbox != nil {
			f.service.outbox.stop()
		}
		now = time.Now().UTC().Truncate(time.Microsecond)
		f.service.config.now = func() time.Time { return now }
		f.service.config.SecretKeys = secretTestKeys(t, "1", "1")
		base := "/api/v2/organizations/" + string(org) + "/integrations/slack"
		requireNativeStatus(t, f.request(t, hostedSecurityUser{}, http.MethodGet, base, nil), http.StatusUnauthorized)
		viewer := f.user(t, "viewer", "viewer", "viewer@example.test", "", "")
		requireNativeStatus(t, f.request(t, viewer, http.MethodPut, base, map[string]string{"webhook": slackTestWebhook}), http.StatusForbidden)
		requireNativeStatus(t, f.request(t, owner, http.MethodPut, base, map[string]string{"webhook": slackTestWebhook, "channel_name": "#operations"}), http.StatusOK)
		requireNativeStatus(t, f.request(t, owner, http.MethodGet, strings.Replace(base, string(org), "org_foreign", 1), nil), http.StatusNotFound)
		for _, id := range []string{"finding_first", "finding_second"} {
			evidence, err := details.StoreDetail(t.Context(), string(org), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
				_, err := w.CreateHealthRecord(t.Context(), postgres.CreateHealthRecordInput{OrganizationID: org, ID: id, Fingerprint: id, Signal: "runner_heartbeat_gap", Class: "instance", SubjectKind: "runner", SubjectID: "runner_example", OpenedAt: now, LastSeenAt: now, Severity: "attention", Summary: "Runner is stale", NextAction: "Check runner", EvidenceObjectKey: evidence.Key, EvidenceSha256: evidence.SHA256, EvidenceBytes: evidence.CompressedSize, EvidenceContentBytes: evidence.UncompressedSize})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteSlack(t.Context(), org, corestore.Background, func(w postgres.SlackWriter) error {
				_, err := w.EnqueueSlackDelivery(t.Context(), postgres.EnqueueSlackDeliveryInput{OrganizationID: org, ID: id, FindingID: id, Event: "opened", EventAt: now, QueuedAt: now})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		calls := 0
		f.service.config.SlackHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return &http.Response{StatusCode: http.StatusTooManyRequests, Body: io.NopCloser(strings.NewReader("rejected")), Header: make(http.Header)}, nil
			}
			if calls == 2 {
				return nil, errors.New("connection outcome unknown")
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
		})}
		for range 2 {
			if processed, err := f.service.processSlackDelivery(t.Context()); err != nil || !processed {
				t.Fatalf("delivery processed=%v err=%v", processed, err)
			}
		}
		now = now.Add(2 * time.Minute)
		if processed, err := f.service.processSlackDelivery(t.Context()); err != nil || !processed {
			t.Fatalf("retry processed=%v err=%v", processed, err)
		}
		now = now.Add(24 * time.Hour)
		if processed, err := f.service.processSlackDelivery(t.Context()); err != nil || processed {
			t.Fatalf("uncertain delivery replayed: processed=%v err=%v", processed, err)
		}
		if calls != 3 {
			t.Fatalf("calls=%d", calls)
		}
		for _, test := range []struct {
			id, outcome string
			attempts    int64
		}{{"finding_first", "committed", 2}, {"finding_second", "uncertain", 1}} {
			if err := repository.ReadSlack(t.Context(), org, corestore.Serving, func(r postgres.SlackReader) error {
				row, err := r.SlackDelivery(t.Context(), postgres.SlackDeliveryInput{OrganizationID: org, ID: test.id})
				if err == nil && (row.Outcome != test.outcome || row.Attempts != test.attempts) {
					t.Fatalf("receipt=%+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		cfg := f.service.config
		if err := f.service.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if reopened.outbox != nil {
			reopened.outbox.stop()
		}
		status, readErr := reopened.readSlackIntegration(t.Context(), org)
		if readErr != nil || status.Webhook != maskedSlackWebhook || status.ChannelName != "#operations" || status.LastDeliveryFailed {
			t.Fatalf("reopened status=%+v err=%v", status, readErr)
		}
		if processed, err := reopened.processSlackDelivery(context.Background()); err != nil || processed {
			t.Fatalf("reopen replay processed=%v err=%v", processed, err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSlackSendOutcomeClassification(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		err     error
		outcome string
	}{
		{name: "accepted", status: 200, outcome: "committed"},
		{name: "rate limited", status: 429, outcome: "rejected"},
		{name: "server failure", status: 503, outcome: "uncertain"},
		{name: "request timeout", status: 408, outcome: "uncertain"},
		{name: "redirect", status: 302, outcome: "uncertain"},
		{name: "lost response", err: errors.New("response unavailable"), outcome: "uncertain"},
		{name: "connection refused", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, outcome: "rejected"},
		{name: "dns failure", err: &net.DNSError{Err: "lookup failed", Name: "example.test"}, outcome: "rejected"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{config: Config{SlackHTTPClient: &http.Client{Transport: spritesTestTransport(func(*http.Request) (*http.Response, error) {
				if test.err != nil {
					return nil, test.err
				}
				return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader("response")), Header: make(http.Header)}, nil
			})}}}
			result := service.postSlack(t.Context(), []byte(slackTestWebhook), "Example")
			if result.outcome != test.outcome || result.status != test.status {
				t.Fatalf("result=%+v want status=%d outcome=%s", result, test.status, test.outcome)
			}
		})
	}
}
