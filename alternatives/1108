package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRepositoryList(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name              string
		authErr, errorAPI error
		pages             int
		want              int
		reason            string
	}{
		{name: "user and organization repositories", pages: 1, want: 2},
		{name: "bounded", pages: 20, want: runnerauth.RepositoryListLimit},
		{name: "CLI absent", authErr: os.ErrNotExist, reason: "no GitHub CLI sign-in"},
		{name: "signed out", authErr: errors.New("credential-secret"), reason: "no GitHub CLI sign-in"},
		{name: "API failure", errorAPI: errors.New("credential-secret example-org/private"), pages: 1, reason: "repository listing unavailable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			list := listRunnerRepositories(t.Context(), "request", func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "auth" {
					return []byte("credential-secret"), tt.authErr
				}
				calls++
				if !strings.Contains(args[len(args)-1], "affiliation=owner,collaborator,organization_member&sort=pushed&direction=desc") {
					t.Fatalf("missing scope or order: %v", args)
				}
				if tt.errorAPI != nil {
					return nil, tt.errorAPI
				}
				count := 2
				if tt.pages > 1 {
					count = 100
				}
				repos := make([]map[string]any, 0, count)
				for i := range count {
					repos = append(repos, map[string]any{"owner": map[string]string{"login": []string{"example-user", "example-org"}[i%2]}, "name": fmt.Sprintf("repository-%04d", calls*100+i), "private": i%2 == 1, "pushed_at": time.Date(2026, 1, 1, 0, 0, calls*100+i, 0, time.UTC), "token": "credential-secret"})
				}
				return json.Marshal(repos)
			})
			if len(list.Repositories) != tt.want || list.Reason != tt.reason || calls != tt.pages {
				t.Fatalf("report count=%d reason=%q calls=%d", len(list.Repositories), list.Reason, calls)
			}
			for i, repo := range list.Repositories {
				if repo.Owner == "" || repo.Name == "" || repo.PushedAt == nil {
					t.Fatalf("incomplete entry: %+v", repo)
				}
				if i > 0 && repo.PushedAt.After(*list.Repositories[i-1].PushedAt) {
					t.Fatal("not sorted newest first")
				}
			}
			if tt.want == 2 && (list.Repositories[0].Owner != "example-org" || !list.Repositories[0].Private || list.Repositories[1].Private || list.Repositories[0].Name != "repository-0101") {
				t.Fatalf("metadata = %+v", list.Repositories)
			}
			raw, err := json.Marshal(list)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "credential-secret") {
				t.Fatal("credentials in report")
			}
		})
	}
}

func TestRepositoryDiscoveryIsNotStarvedByGitHubIntake(t *testing.T) {
	t.Parallel()
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken), GitHubDisabled: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		service.Handler().ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})
	httpClient := &http.Client{Transport: transport}
	admin, err := New(Config{URL: "https://hub.example.test", TokenSource: func() string { return nativeChangeAdminToken }, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	request := func(t *testing.T, method, path string, input, output any) {
		t.Helper()
		if err := admin.request(t.Context(), method, path, input, output); err != nil {
			t.Fatal(err)
		}
	}
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	request(t, http.MethodGet, "/api/v2/organizations", nil, &organizations)
	organization := organizations.Items[0].ID
	base := "/api/v2/organizations/" + string(organization)
	createProject := func(name string) tracker.NativeProject {
		t.Helper()
		var project tracker.NativeProject
		request(t, http.MethodPost, base+"/projects", map[string]any{"name": name, "idempotency_key": name, "states": hubserver.HostedProjectStates()}, &project)
		return project
	}
	existing := createProject("existing")
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, admin.baseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := admin.CreateRunnerEnrollment(t.Context(), organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{existing.ID}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	err = admin.runnerRequest(t.Context(), enrollment.Token, http.MethodPost, base+"/runner-enrollments/redeem", runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: "example-host", DisplayName: "Example runner", Capacity: 1, Version: "test"}, &file.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(organization, existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	repository := "example-org/example-project"
	var integration hubserver.ProjectIntegration
	request(t, http.MethodPost, native.base()+"/onboarding/repository", map[string]any{"idempotency_key": "bind", "expected_revision": "1", "repository": repository, "source": "runner_checkout"}, &integration)
	request(t, http.MethodPut, native.base()+"/integration", map[string]any{"idempotency_key": "manual", "expected_revision": fmt.Sprint(integration.Revision), "intake": "manual", "projection": "disabled"}, nil)
	machine := Machine{ID: file.Identity.MachineID, Hostname: "example-host", DisplayName: "Example runner", Capacity: 1, Version: "test", CheckoutRepository: &repository}
	if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	request(t, http.MethodPost, native.base()+"/onboarding/issue-intake", tracker.GitHubBatchCommand{Mutation: tracker.Mutation{IdempotencyKey: "discover"}, Action: "discover", RunnerID: file.Identity.RunnerID}, nil)
	created := createProject("new-project")
	setupPath := base + "/projects/" + string(created.ID) + "/onboarding"
	intakeFailure := errors.New("intake unavailable")
	for _, test := range []struct {
		name             string
		intakeError      error
		listingError     bool
		unavailableReads bool
	}{
		{name: "pending intake"},
		{name: "failed intake", intakeError: intakeFailure},
		{name: "failed listing", listingError: true},
		{name: "unavailable project events and activity", unavailableReads: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var command struct {
				RequestID string `json:"request_id"`
			}
			request(t, http.MethodPost, setupPath+"/repositories", map[string]any{"idempotency_key": test.name, "runner_id": file.Identity.RunnerID}, &command)
			readList := func() *runnerauth.RepositoryList {
				t.Helper()
				var setup onboarding.Project
				request(t, http.MethodGet, setupPath, nil, &setup)
				for _, runner := range setup.Runners {
					if runner.Runner.RunnerID == file.Identity.RunnerID && runner.RepositoryList != nil {
						return runner.RepositoryList
					}
				}
				t.Fatal("runner repository list missing from new project setup")
				return nil
			}
			if list := readList(); !list.Pending || list.RequestID != command.RequestID {
				t.Fatalf("pending list=%+v", list)
			}
			intakes, listings := 0, 0
			native.githubBatch = func(_ context.Context, task tracker.GitHubBatchTask) error {
				if task.ProjectID != existing.ID || task.Discovery == nil {
					t.Fatalf("intake task=%+v", task)
				}
				intakes++
				return test.intakeError
			}
			native.repositoryCommand = func(_ context.Context, args ...string) ([]byte, error) {
				if args[0] == "auth" {
					return nil, nil
				}
				listings++
				if test.listingError {
					return nil, errors.New("external listing failed")
				}
				return []byte(`[{"owner":{"login":"example-org"},"name":"example-project","private":true}]`), nil
			}
			if test.unavailableReads {
				httpClient.Transport = executionRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodGet && (r.URL.Path == base+"/projects/"+string(created.ID) || strings.HasSuffix(r.URL.Path, "/events") || r.URL.Path == base+"/activity") {
						recorder := httptest.NewRecorder()
						status := http.StatusBadGateway
						if r.URL.Path == base+"/activity" {
							status = http.StatusInternalServerError
						}
						recorder.WriteHeader(status)
						return recorder.Result(), nil
					}
					return transport.RoundTrip(r)
				})
				client.httpClient.Transport = httpClient.Transport
				t.Cleanup(func() {
					httpClient.Transport = transport
					client.httpClient.Transport = transport
				})
				for _, read := range []struct {
					path   string
					status int
				}{
					{base + "/projects/" + string(created.ID), http.StatusBadGateway},
					{"/projects/" + string(created.ID) + "/events", http.StatusBadGateway},
					{"/projects/" + string(created.ID) + "/events?board=true&completed_window=1h", http.StatusBadGateway},
					{base + "/activity", http.StatusInternalServerError},
				} {
					err := client.request(t.Context(), http.MethodGet, read.path, nil, nil)
					var apiErr *APIError
					if !errors.As(err, &apiErr) || apiErr.Status != read.status {
						t.Fatalf("read %s: error=%v want status=%d", read.path, err, read.status)
					}
				}
			}
			if err := native.HeartbeatMachine(t.Context(), machine); !errors.Is(err, test.intakeError) {
				t.Fatalf("heartbeat error=%v want=%v", err, test.intakeError)
			}
			if intakes != 1 || listings != 1 {
				t.Fatalf("repository discovery skipped: intake calls=%d listing calls=%d", intakes, listings)
			}
			list := readList()
			if list.Pending || list.RequestID != command.RequestID || list.ListedAt == "" {
				t.Fatalf("repository result not served to new project: %+v", list)
			}
			if test.listingError {
				if list.Reason != "repository listing unavailable" || len(list.Repositories) != 0 {
					t.Fatalf("failed listing=%+v", list)
				}
			} else if len(list.Repositories) != 1 || list.Repositories[0].Owner != "example-org" || list.Repositories[0].Name != "example-project" || !list.Repositories[0].Private {
				t.Fatalf("completed listing=%+v", list)
			}
		})
	}
}
