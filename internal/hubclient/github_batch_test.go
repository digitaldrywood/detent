package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	githubconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerGitHubBatchReportsOneBoundedStep(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	for _, test := range []struct {
		name      string
		discovery bool
		failure   bool
	}{{name: "discovery", discovery: true}, {name: "complete snapshot"}, {name: "source throttled", failure: true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reports := 0
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reports++
				if r.URL.Path != "/api/v2/organizations/org_test/projects/prj_test/onboarding/issue-intake/result" {
					t.Errorf("path=%s", r.URL.Path)
				}
				var result tracker.GitHubBatchResult
				if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
					t.Error(err)
				}
				if result.BatchID != "intake_test" || result.Revision != 4 || result.IdempotencyKey != "intake_test-4" {
					t.Errorf("identity=%#v", result)
				}
				if test.failure {
					if result.Snapshot != nil || result.Error == "" || result.RetryAt == "" {
						t.Errorf("failure=%#v", result)
					}
				} else if test.discovery {
					if result.Page == nil {
						t.Error("missing preview")
					}
				} else if result.Snapshot == nil || result.Number != 12 {
					t.Error("missing complete snapshot")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{}`))
			}))
			t.Cleanup(server.Close)
			client, err := New(Config{URL: server.URL, TokenSource: func() string { return "hub-token" }, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			scheduler := &Scheduler{githubIntake: func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
				reads++
				if test.failure {
					return tracker.GitHubIssueSnapshot{}, &githubconnector.StatusError{StatusCode: 429, Err: githubconnector.ErrRateLimited, RetryAfter: time.Hour}
				}
				return intakeSnapshot(), nil
			}, githubDiscovery: func(context.Context, tracker.GitHubDiscovery) (tracker.GitHubDiscoveryPage, error) {
				reads++
				return tracker.GitHubDiscoveryPage{Total: 1}, nil
			}}
			task := tracker.GitHubBatchTask{ProjectID: "prj_test", BatchID: "intake_test", Revision: 4}
			if test.discovery {
				task.Discovery = &tracker.GitHubDiscovery{Repository: "acme/orders"}
			} else {
				task.Item = &tracker.GitHubIssuePreview{Number: 12, URL: "https://github.com/acme/orders/issues/12"}
			}
			if err := scheduler.processGitHubBatch(t.Context(), native, task); err != nil {
				t.Fatal(err)
			}
			if reads != 1 || reports != 1 {
				t.Fatalf("reads=%d reports=%d", reads, reports)
			}
		})
	}
}
