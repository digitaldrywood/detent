package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
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
	config := testsupport.DispatchPostgresConfig(t, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken), Content: newHubContentStore(), RunDetails: &repodiscDetails{}})
	service, err := hubserver.Open(t.Context(), config)
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
	organization := tracker.OrganizationID(config.Hosted.OrganizationID)
	base := "/api/v2/organizations/" + string(organization)
	createProject := func(t *testing.T, name string) tracker.NativeProject {
		t.Helper()
		var project tracker.NativeProject
		request(t, http.MethodPost, base+"/projects", map[string]any{"name": name, "idempotency_key": name, "states": hubserver.HostedProjectStates(), "grant_access": true}, &project)
		return project
	}
	existing := createProject(t, "existing")
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
	machine := Machine{ID: file.Identity.MachineID, Hostname: "example-host", DisplayName: "Example runner", Capacity: 1, Version: "test", CheckoutRepository: &repository}
	if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	var intake struct {
		Batch *tracker.GitHubBatch `json:"batch"`
	}
	request(t, http.MethodGet, native.base()+"/onboarding/issue-intake", nil, &intake)
	if intake.Batch != nil {
		t.Fatalf("unexpected intake batch=%+v", intake.Batch)
	}
	readList := func(t *testing.T, setupPath string) *runnerauth.RepositoryList {
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
	t.Run("pending onboarding response", func(t *testing.T) {
		created := createProject(t, "pending onboarding response")
		setupPath := base + "/projects/" + string(created.ID) + "/onboarding"
		var command struct {
			RequestID string `json:"request_id"`
		}
		request(t, http.MethodPost, setupPath+"/repositories", map[string]any{"idempotency_key": t.Name(), "runner_id": file.Identity.RunnerID}, &command)
		started, proceed, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
		release := sync.OnceFunc(func() { close(proceed) })
		var heartbeatErr error
		native.repositoryCommand = func(ctx context.Context, args ...string) ([]byte, error) {
			if args[0] == "auth" {
				return nil, nil
			}
			close(started)
			select {
			case <-proceed:
				return []byte(`[]`), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		t.Cleanup(func() { release(); <-finished })
		go func() {
			heartbeatErr = native.HeartbeatMachine(t.Context(), machine)
			close(finished)
		}()
		select {
		case <-started:
		case <-finished:
			t.Fatalf("heartbeat ended before repository listing: %v", heartbeatErr)
		}
		if list := readList(t, setupPath); !list.Pending || list.RequestID != command.RequestID {
			t.Fatalf("pending list=%+v", list)
		}
		release()
		<-finished
		if heartbeatErr != nil {
			t.Fatal(heartbeatErr)
		}
	})
	intakeFailure := errors.New("intake unavailable")
	for _, test := range []struct {
		name             string
		injectIntake     bool
		intakeError      error
		listingError     bool
		unavailableReads bool
	}{
		{name: "disabled PostgreSQL intake"},
		{name: "injected pending intake", injectIntake: true},
		{name: "injected failed intake", injectIntake: true, intakeError: intakeFailure},
		{name: "failed listing", listingError: true},
		{name: "unavailable project events and activity", unavailableReads: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			created := createProject(t, test.name)
			setupPath := base + "/projects/" + string(created.ID) + "/onboarding"
			var command struct {
				RequestID string `json:"request_id"`
			}
			request(t, http.MethodPost, setupPath+"/repositories", map[string]any{"idempotency_key": test.name, "runner_id": file.Identity.RunnerID}, &command)
			intakes, listings, heartbeats := 0, 0, 0
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
			httpClient.Transport = executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if test.unavailableReads && r.Method == http.MethodGet && (r.URL.Path == base+"/projects/"+string(created.ID) || strings.HasSuffix(r.URL.Path, "/events") || r.URL.Path == base+"/activity") {
					recorder := httptest.NewRecorder()
					status := http.StatusBadGateway
					if r.URL.Path == base+"/activity" {
						status = http.StatusInternalServerError
					}
					recorder.WriteHeader(status)
					return recorder.Result(), nil
				}
				response, err := transport.RoundTrip(r)
				if err != nil || r.Method != http.MethodPost || r.URL.Path != native.base()+"/machines/"+string(machine.ID)+"/heartbeat" || response.StatusCode != http.StatusOK {
					return response, err
				}
				defer response.Body.Close()
				var snapshot runnerauth.RoutingSnapshot
				if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
					return nil, err
				}
				if snapshot.GitHubIntake != nil || snapshot.RepositoryListRequest != command.RequestID {
					t.Fatalf("PostgreSQL heartbeat intake=%+v repository request=%q want=%q", snapshot.GitHubIntake, snapshot.RepositoryListRequest, command.RequestID)
				}
				heartbeats++
				if test.injectIntake {
					snapshot.GitHubIntake = &tracker.GitHubBatchTask{ProjectID: existing.ID, Discovery: &tracker.GitHubDiscovery{Repository: repository}}
				}
				raw, err := json.Marshal(snapshot)
				if err != nil {
					return nil, err
				}
				response.Body = io.NopCloser(bytes.NewReader(raw))
				response.ContentLength = int64(len(raw))
				response.Header.Set("Content-Length", fmt.Sprint(len(raw)))
				return response, nil
			})
			client.httpClient.Transport = httpClient.Transport
			t.Cleanup(func() {
				httpClient.Transport = transport
				client.httpClient.Transport = transport
			})
			if test.unavailableReads {
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
			wantIntakes := 0
			if test.injectIntake {
				wantIntakes = 1
			}
			if intakes != wantIntakes || listings != 1 || heartbeats != 1 {
				t.Fatalf("repository discovery skipped: intake calls=%d want=%d listing calls=%d heartbeats=%d", intakes, wantIntakes, listings, heartbeats)
			}
			list := readList(t, setupPath)
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
