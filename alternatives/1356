package hubclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
		project   bool
	}{{name: "discovery", discovery: true}, {name: "ProjectV2 discovery", discovery: true, project: true}, {name: "complete snapshot"}, {name: "source throttled", failure: true}} {
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
					} else if test.project && (len(result.Page.Issues) != 1 || result.Page.Issues[0].ProjectStatus != "Ready") {
						t.Errorf("runner lost ProjectV2 Status: %+v", result.Page)
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
			}, githubDiscovery: func(_ context.Context, request tracker.GitHubDiscovery) (tracker.GitHubDiscoveryPage, error) {
				reads++
				if test.project {
					if request.ProjectV2 == nil || request.ProjectV2.ID != "PVT_source" || request.ProjectV2.StatusLanes["Ready"] != "Blocked" {
						t.Fatalf("runner lost ProjectV2 source: %+v", request)
					}
					return tracker.GitHubDiscoveryPage{Total: 1, Issues: []tracker.GitHubIssuePreview{{Number: 12, ProjectStatus: "Ready"}}}, nil
				}
				return tracker.GitHubDiscoveryPage{Total: 1}, nil
			}}
			task := tracker.GitHubBatchTask{ProjectID: "prj_test", BatchID: "intake_test", Revision: 4}
			if test.discovery {
				task.Discovery = &tracker.GitHubDiscovery{Repository: "example-org/example-project"}
				if test.project {
					task.Discovery.ProjectV2 = &tracker.GitHubProjectV2Source{ID: "PVT_source", StatusLanes: map[string]string{"Ready": "Blocked"}}
				}
			} else {
				task.Item = &tracker.GitHubIssuePreview{Number: 12, URL: "https://github.com/example-org/example-project/issues/12"}
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

func TestRunnerGitHubBatchRunsOutsideHeartbeatOneAtATime(t *testing.T) {
	t.Parallel()
	var reports atomic.Int32
	httpClient := &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		reports.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
	})}
	client, err := New(Config{URL: "https://hub.test", TokenSource: func() string { return "fixture-token" }, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	release := make(chan struct{})
	scheduler := &Scheduler{githubIntake: func(context.Context, string) (tracker.GitHubIssueSnapshot, error) {
		reads.Add(1)
		<-release
		return intakeSnapshot(), nil
	}}
	task := tracker.GitHubBatchTask{ProjectID: "prj_test", BatchID: "intake_test", Revision: 1, Item: &tracker.GitHubIssuePreview{Number: 12, URL: "https://github.com/example-org/example-project/issues/12"}}

	done := scheduler.startGitHubBatch(t.Context(), native, task)
	if done == nil {
		t.Fatal("first offer did not start the intake")
	}
	if again := scheduler.startGitHubBatch(t.Context(), native, task); again != nil {
		t.Fatal("repeated offer started a second intake")
	}
	close(release)
	<-done
	if reads.Load() != 1 || reports.Load() != 1 {
		t.Fatalf("reads=%d reports=%d", reads.Load(), reports.Load())
	}
	next := scheduler.startGitHubBatch(t.Context(), native, task)
	if next == nil {
		t.Fatal("lane did not free after the intake finished")
	}
	<-next
}

func TestRunnerGitHubBatchContinuesPagesWithoutHeartbeat(t *testing.T) {
	t.Parallel()
	reports, reads := 0, 0
	httpClient := &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		reports++
		var result tracker.GitHubBatchResult
		if err := json.NewDecoder(request.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		first, count := 1+(reports-1)*100, 100
		if reports == 4 {
			count = 85
		}
		if len(result.Results) != count || result.Results[0].Number != first || result.Results[count-1].Number != first+count-1 || result.Revision != int64(reports) {
			t.Fatalf("page=%+v", result)
		}
		receipt := tracker.GitHubBatchReceipt{Revision: int64(reports + 1), Status: "importing"}
		if reports < 4 {
			task := tracker.GitHubBatchTask{ProjectID: "prj_test", BatchID: "intake_test", Revision: int64(reports + 1)}
			for number := first + count; number <= min(first+count+99, 385); number++ {
				task.Items = append(task.Items, tracker.GitHubIssuePreview{Number: number, URL: fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", number)})
			}
			receipt.Next = &task
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), Request: request}, nil
	})}
	client, err := New(Config{URL: "https://hub.test", TokenSource: func() string { return "fixture-token" }, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	scheduler := &Scheduler{githubIntake: func(_ context.Context, url string) (tracker.GitHubIssueSnapshot, error) {
		reads++
		snapshot := intakeSnapshot()
		snapshot.URL = url
		return snapshot, nil
	}}
	task := tracker.GitHubBatchTask{ProjectID: "prj_test", BatchID: "intake_test", Revision: 1}
	for number := 1; number <= 100; number++ {
		task.Items = append(task.Items, tracker.GitHubIssuePreview{Number: number, URL: fmt.Sprintf("https://github.com/example-org/example-project/issues/%d", number)})
	}
	if err := scheduler.processGitHubBatch(t.Context(), native, task); err != nil {
		t.Fatal(err)
	}
	if reports != 4 || reads != 385 {
		t.Fatalf("reports=%d reads=%d", reports, reads)
	}
}
