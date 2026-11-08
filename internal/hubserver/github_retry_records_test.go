package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type gitHubDiscoveryFailure struct{ available bool }

var errGitHubDiscoveryFailure = errors.New("example discovery unavailable")

func (b *gitHubDiscoveryFailure) FetchImportPage(context.Context, GitHubImportRequest) (GitHubImportPage, error) {
	if !b.available {
		return GitHubImportPage{}, errGitHubDiscoveryFailure
	}
	return GitHubImportPage{BatchPage: &tracker.GitHubDiscoveryPage{Issues: []tracker.GitHubIssuePreview{}}}, nil
}

func TestGitHubDiscoveryFailureBackoff(t *testing.T) {
	for _, test := range []struct {
		name   string
		jitter float64
		delays []time.Duration
	}{
		{"growing attempts", 1, []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}},
		{"jittered attempts", 0, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, now, _ := gitHubWebhookRecordFixture(t)
			*now = time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
			backend := &gitHubDiscoveryFailure{}
			service.config.ImportBackend = backend
			service.config.jitter = func() float64 { return test.jitter }
			for _, project := range []tracker.ProjectID{"first", "second"} {
				row, err := service.prepareGitHubBatchRecord(t.Context(), nativeScope{organization: "org", project: project}, &tracker.GitHubBatch{ID: "batch_" + string(project), Revision: 1, Status: "discovering", HubOwned: true, Automatic: true, Limit: 1000, Discovery: tracker.GitHubDiscovery{Repository: "example-org/example-project"}})
				if err != nil {
					t.Fatal(err)
				}
				if err := service.config.TenantRepository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
					return w.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), row, 0)
				}); err != nil {
					t.Fatal(err)
				}
			}
			for attempt, delay := range test.delays {
				*now = now.Add(10 * time.Minute)
				for _, project := range []tracker.ProjectID{"first", "second"} {
					err := service.maintainGitHubBatches(t.Context())
					if !errors.Is(err, errGitHubDiscoveryFailure) {
						t.Fatalf("failed discovery=%v", err)
					}
					t.Logf("discovery attempt error: %v", err)
					if err := service.config.TenantRepository.ReadTenant(t.Context(), "org", corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.(projectstore.IntakeBatchReader).ProjectIntakeBatch(t.Context(), project)
						if err == nil && (row.Attempts != int64(attempt+1) || !row.NextAttemptAt.Equal(now.Add(delay))) {
							t.Fatalf("project=%s attempt=%d next=%v want attempt=%d next=%v", project, row.Attempts, row.NextAttemptAt, attempt+1, now.Add(delay))
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			backend.available = true
			*now = now.Add(10 * time.Minute)
			for _, project := range []tracker.ProjectID{"first", "second"} {
				if err := service.maintainGitHubBatches(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := service.config.TenantRepository.ReadTenant(t.Context(), "org", corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.(projectstore.IntakeBatchReader).ProjectIntakeBatch(t.Context(), project)
					if err == nil && (row.Attempts != 0 || row.Status != "finished") {
						t.Fatalf("verified progress did not reset attempts: %+v", row)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestGitHubWebhookRetryJitter(t *testing.T) {
	service, now, _ := gitHubWebhookRecordFixture(t)
	row, payload := gitHubWebhookEdit(t, service, "issues")
	payload.Issue.NodeID = nil
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	row.Payload, err = eventstore.NewContent(service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org"}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.(projectstore.GitHubIntakeWriter).CreateGitHubWebhook(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for attempt, test := range []struct {
		jitter float64
		delay  time.Duration
	}{{0, time.Second}, {1, 4 * time.Second}, {0.5, 6 * time.Second}, {0.25, 10 * time.Second}, {0, 16 * time.Second}, {1, 64 * time.Second}, {0.5, 48 * time.Second}} {
		service.config.jitter = func() float64 { return test.jitter }
		if err := service.processGitHubWebhookRecord(t.Context(), row); err == nil {
			t.Fatal("invalid source accepted")
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), "org", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			row, err = r.(projectstore.GitHubIntakeReader).GitHubWebhook(t.Context(), row.DeliveryID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if row.Attempts != int64(attempt+1) || !row.NextAttemptAt.Equal(now.Add(test.delay)) {
			t.Fatalf("attempt=%d next=%v want attempt=%d next=%v", row.Attempts, row.NextAttemptAt, attempt+1, now.Add(test.delay))
		}
		*now = now.Add(2 * time.Minute)
	}
}

func TestGitHubBatchMissingTransportReturnsError(t *testing.T) {
	service, _, _ := gitHubWebhookRecordFixture(t)
	for _, project := range []tracker.ProjectID{"first", "second"} {
		scope, err := service.gitHubIntegrationScope(t.Context(), "org", project)
		if err != nil {
			t.Fatal(err)
		}
		row, err := service.prepareGitHubBatchRecord(t.Context(), scope, &tracker.GitHubBatch{ID: "batch_" + string(project), Revision: 1, HubOwned: true, Status: "discovering", Discovery: tracker.GitHubDiscovery{Repository: scope.integrationRepository}})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), row, 0)
		}); err != nil {
			t.Fatal(err)
		}
		for _, call := range []func(context.Context, nativeScope) error{service.completeRunnerGitHubBatch, service.advanceGitHubBatch} {
			if err := call(t.Context(), scope); err == nil {
				t.Fatal("discovery accepted an unavailable transport")
			}
		}
	}
}
