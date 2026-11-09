package hubserver

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOutboxWorkerDeliversSlackAndGitHub(t *testing.T) {
	repository := projectRecordPostgres(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		details := &memoryRunDetails{}
		f, owner := newChangePolicyPostgresTenantFixture(t, repository, org, details)
		f.service.workerCancel()
		<-f.service.workerDone
		<-f.service.secretRewrapDone
		if f.service.outbox != nil {
			f.service.outbox.stop()
		}
		githubCalls := make(chan OutboxItem, 4)
		slackCalls := make(chan struct{}, 4)
		var slackCount atomic.Int64
		f.service.config.SecretKeys = secretTestKeys(t, "1", "1")
		f.service.config.OutboxBackend = githubOutboundSignal{calls: githubCalls}
		f.service.config.SlackHTTPClient = &http.Client{Transport: spritesTestTransport(func(r *http.Request) (*http.Response, error) {
			slackCount.Add(1)
			select {
			case slackCalls <- struct{}{}:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
		})}
		base := "/api/v2/organizations/" + string(org) + "/integrations/slack"
		requireNativeStatus(t, f.request(t, hostedSecurityUser{}, http.MethodGet, base, nil), http.StatusUnauthorized)
		requireNativeStatus(t, f.request(t, owner, http.MethodPut, base, map[string]string{"webhook": slackTestWebhook, "channel_name": "#operations"}), http.StatusOK)
		now := time.Now().UTC().Truncate(time.Microsecond)
		for _, id := range []string{"first", "second"} {
			evidence, err := details.StoreDetail(t.Context(), string(org), []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteHealth(t.Context(), org, corestore.Background, func(w postgres.HealthWriter) error {
				_, err := w.CreateHealthRecord(t.Context(), postgres.CreateHealthRecordInput{OrganizationID: org, ID: id, Fingerprint: id, Signal: "runner_heartbeat_gap", Class: "instance", SubjectKind: "runner", SubjectID: "runner_" + id, OpenedAt: now, LastSeenAt: now, Severity: "attention", Summary: "Runner is stale", NextAction: "Check runner", EvidenceObjectKey: evidence.Key, EvidenceSha256: evidence.SHA256, EvidenceBytes: evidence.CompressedSize, EvidenceContentBytes: evidence.UncompressedSize})
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
			raw, err := json.Marshal(WorkpadDesired{Phase: "summary", Summary: true, Marker: "<!-- example-summary -->", Body: "Example summary", IssueNumber: 1})
			if err != nil {
				t.Fatal(err)
			}
			payload, err := eventstore.NewContent(details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				project := tracker.ProjectID("outbox_" + id)
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: "Example " + id, Profile: "native"}); err != nil {
					return err
				}
				return w.(projectstore.GitHubOutboxWriter).EnqueueGitHubOutbound(t.Context(), projectstore.GitHubOutbound{ProjectID: project, WorkItemID: tracker.NativeWorkItemID("wi_" + id), Key: id, Repository: "example-org/example-project", IssueNumber: 1, Kind: string(MutationWorkpad), CreatedAt: now, Payload: payload})
			}); err != nil {
				t.Fatal(err)
			}
		}
		worker := newOutboxWorker(f.service)
		f.service.outbox = worker
		t.Cleanup(worker.stop)
		worker.start(t.Context())
		seen := map[string]bool{}
		for range 2 {
			select {
			case item := <-githubCalls:
				if item.OrganizationID != org || seen[item.IdempotencyKey] {
					t.Fatalf("foreign or duplicate outbound item: %+v", item)
				}
				seen[item.IdempotencyKey] = true
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
			select {
			case <-slackCalls:
			case <-t.Context().Done():
				t.Fatal(t.Context().Err())
			}
		}
		worker.stop()
		cfg := f.service.config
		if err := f.service.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		if reopened.outbox != nil {
			reopened.outbox.stop()
		}
		for _, id := range []string{"first", "second"} {
			if err := repository.ReadSlack(t.Context(), org, corestore.Serving, func(r postgres.SlackReader) error {
				row, err := r.SlackDelivery(t.Context(), postgres.SlackDeliveryInput{OrganizationID: org, ID: id})
				if err == nil && (row.Outcome != "committed" || row.Attempts != 1) {
					t.Fatalf("Slack receipt=%+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), id)
				if err == nil && (row.Status != "completed" || row.Outcome != "committed" || row.Attempts != 0) {
					t.Fatalf("GitHub receipt=%+v", row)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if slackCount.Load() != 2 || len(githubCalls) != 0 || len(seen) != 2 {
			t.Fatalf("replayed delivery: slack=%d github=%d seen=%v", slackCount.Load(), len(githubCalls), seen)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
