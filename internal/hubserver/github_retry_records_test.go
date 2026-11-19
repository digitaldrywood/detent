package hubserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
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
	t.Parallel()
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
			service.config.Deps.Jitter = func() float64 { return test.jitter }
			for _, project := range []tracker.ProjectID{"first", "second"} {
				row, err := service.prepareGitHubBatchRecord(t.Context(), testGitHubBatchIssuer(t, service, "org", project), &tracker.GitHubBatch{ID: "batch_" + string(project), Revision: 1, Status: "discovering", HubOwned: true, RequestedBy: new(testGitHubBatchIssuer(t, service, "org", project).Actor()), Automatic: true, Limit: 1000, Discovery: tracker.GitHubDiscovery{Repository: "example-org/example-project"}})
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

func TestGitHubBatchMissingTransportReturnsError(t *testing.T) {
	t.Parallel()
	service, _, _ := gitHubWebhookRecordFixture(t)
	for _, project := range []tracker.ProjectID{"first", "second"} {
		scope, err := service.gitHubIntegrationScope(t.Context(), "org", project)
		if err != nil {
			t.Fatal(err)
		}
		row, err := service.prepareGitHubBatchRecord(t.Context(), testGitHubBatchIssuer(t, service, "org", project), &tracker.GitHubBatch{ID: "batch_" + string(project), Revision: 1, HubOwned: true, Status: "discovering", RequestedBy: new(testGitHubBatchIssuer(t, service, "org", project).Actor()), Discovery: tracker.GitHubDiscovery{Repository: scope.IntegrationRepository}})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			return w.(projectstore.IntakeBatchWriter).SaveProjectIntakeBatch(t.Context(), row, 0)
		}); err != nil {
			t.Fatal(err)
		}
		for _, call := range []func(context.Context, hubscope.NativeScope) error{service.completeRunnerGitHubBatch, service.advanceGitHubBatch} {
			if err := call(t.Context(), scope); err == nil {
				t.Fatal("discovery accepted an unavailable transport")
			}
		}
	}
}
