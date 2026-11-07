package dispatch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type clientFixture struct {
	requests        *testsupport.Transport
	renewals        *testsupport.Transport
	mu              sync.RWMutex
	config          Config
	baseURL         *url.URL
	offset          atomic.Int64
	httpClient      *http.Client
	leaseHTTPClient *http.Client
}

var clientFixtures sync.Map

func newTestClient(t testing.TB, config Config) (*Client, error) {
	t.Helper()
	config.SessionTimings = api.SessionTimings{FlushInterval: 20 * time.Millisecond, ReportBackoff: 10 * time.Millisecond, PollWait: time.Second}
	if config.HTTPClient == nil {
		transport := http.DefaultTransport
		if standard, ok := transport.(*http.Transport); ok {
			transport = standard.Clone()
		}
		config.HTTPClient = &http.Client{Transport: transport}
	}
	lease := *config.HTTPClient
	if transport, ok := lease.Transport.(*http.Transport); ok {
		lease.Transport = transport.Clone()
	}
	config.LeaseHTTPClient = &lease
	fixture := &clientFixture{config: config, httpClient: config.HTTPClient, leaseHTTPClient: &lease}
	fixture.baseURL, _ = url.Parse(config.URL)
	clock := config.Now
	if clock == nil {
		clock = time.Now
	}
	config.Now = func() time.Time { return clock().Add(time.Duration(fixture.offset.Load())) }
	config.ArtifactSettings = func() (string, int64) {
		fixture.mu.RLock()
		defer fixture.mu.RUnlock()
		return fixture.config.ArtifactServiceID, fixture.config.ArtifactBytes
	}
	original := config.TokenSource
	if original != nil {
		config.TokenSource = func() string {
			fixture.mu.RLock()
			source := fixture.config.TokenSource
			fixture.mu.RUnlock()
			return source()
		}
	}
	requestClient := *config.HTTPClient
	fixture.requests = testsupport.NewTransport(requestClient.Transport)
	requestClient.Transport = fixture.requests
	fixture.httpClient = &requestClient
	config.HTTPClient = &requestClient
	renewalClient := *config.LeaseHTTPClient
	fixture.renewals = testsupport.NewTransport(renewalClient.Transport)
	renewalClient.Transport = fixture.renewals
	fixture.leaseHTTPClient = &renewalClient
	config.LeaseHTTPClient = &renewalClient

	client, err := New(config)
	if err != nil {
		return nil, err
	}
	clientFixtures.Store(client, fixture)
	t.Cleanup(func() {
		clientFixtures.Delete(client)
		fixture.httpClient.CloseIdleConnections()
		renewalClient.CloseIdleConnections()
	})
	return client, nil
}
func fixtureClient(client *Client) *clientFixture {
	value, ok := clientFixtures.Load(client)
	if !ok {
		panic("client fixture is not registered")
	}
	return value.(*clientFixture)
}

func emptyProcessSource(context.Context, string) ([]int, error) { return nil, nil }

const maxResponseBytes = 1 << 20

type nativeCapabilities struct {
	MinimumRunnerVersion string   `json:"minimum_runner_version"`
	Version              string   `json:"version"`
	ProtocolMajors       []int    `json:"protocol_majors"`
	EventSchemas         []int    `json:"event_schema_versions"`
	Features             []string `json:"features"`
}

func (f *clientFixture) bearerToken(ctx context.Context) (string, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.config.IdentityFile != "" {
		file, err := runnerauth.Load(f.config.IdentityFile)
		return "Bearer " + file.Credential, err
	}
	return "Bearer " + f.config.TokenSource(), nil
}

func fixtureConnector(native *NativeClient) *NativeConnector {
	connector, err := NewNativeConnector(native)
	if err != nil {
		panic(err)
	}
	return connector
}

func newTestScheduler(client *Client, config SchedulerConfig) (*Scheduler, error) {
	if config.Machine.DisplayName == "" {
		config.Machine.DisplayName = config.Machine.Hostname
	}
	config.ExecutionFactory = nativechange.NewExecution
	config.CompletionRestorer = nativechange.RestoreCompletion
	var scheduler *Scheduler
	if config.CapacityConfiguration == nil && client.Enrolled() {
		config.CapacityConfiguration = func(context.Context, *runnerauth.CapacityRequest) *runnerauth.CapacityConfig {
			scheduler.mu.Lock()
			capacity := max(1, scheduler.machine.Capacity)
			scheduler.mu.Unlock()
			return &runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, ObservedAt: scheduler.now()}
		}
	}
	var err error
	scheduler, err = NewScheduler(client, config)
	if err == nil {
		schedulerFixtures.Store(scheduler, config)
	}
	return scheduler, err
}

var schedulerFixtures sync.Map

func fixtureRunnerRequest(t testing.TB, client *Client, token, method, path string, input, output any) error {
	t.Helper()
	fixture := fixtureClient(client)
	fixture.mu.RLock()
	config := fixture.config
	fixture.mu.RUnlock()
	if err := runnerauth.ValidateHubURL(config.URL); err != nil {
		return err
	}
	config.IdentityFile = ""
	config.TokenSource = func() string { return token }
	raw, err := newTestClient(t, config)
	if err != nil {
		return err
	}
	return raw.Request(t.Context(), method, path, input, output)
}

func fixtureWithoutFeature(t testing.TB, client *Client, feature string) {
	t.Helper()
	fixture := fixtureClient(client)
	next := fixture.requests.Load()
	fixture.requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		response, err := next.RoundTrip(request)
		if err != nil || response == nil || !strings.HasSuffix(request.URL.Path, "/capabilities") {
			return response, err
		}
		var capabilities nativeCapabilities
		if err := json.NewDecoder(response.Body).Decode(&capabilities); err != nil {
			return nil, err
		}
		if err := response.Body.Close(); err != nil {
			return nil, err
		}
		capabilities.Features = slices.DeleteFunc(capabilities.Features, func(current string) bool {
			return current == feature
		})
		encoded, err := json.Marshal(capabilities)
		if err != nil {
			return nil, err
		}
		response.Body = io.NopCloser(strings.NewReader(string(encoded)))
		response.ContentLength = int64(len(encoded))
		return response, nil
	}))
	fixture.offset.Add(int64(2 * time.Hour))
}

func fixtureEnrollment(enabled bool, path string) *api.RunnerState {
	if !enabled {
		return nil
	}
	return api.NewRunnerState(path)
}

func fixtureLeaseCached(t testing.TB, client *Client, scope string, item tracker.NativeWorkItemID) (tracker.NativeLease, bool) {
	t.Helper()
	parts := strings.Split(scope, "/")
	native, err := client.Native(tracker.OrganizationID(parts[len(parts)-3]), tracker.ProjectID(parts[len(parts)-1]))
	if err != nil {
		t.Fatal(err)
	}
	mutation := native.FencedMutation(context.Background(), item, tracker.Mutation{})
	return tracker.NativeLease{ID: mutation.LeaseID, FencingToken: mutation.FencingToken}, mutation.LeaseID != ""
}

func fixtureEnableEnrollment(t testing.TB, h *nativeChangeHub) {
	t.Helper()
	original := fixtureClient(h.native.Transport())
	config := original.config
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, config.URL)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := h.admin.Transport().CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	report := isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
	redemption := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: "host", DisplayName: "Fixture runner", Capacity: h.scheduler.machine.Capacity, Version: "test", BackendIsolation: report}
	if err := fixtureRunnerRequest(t, h.admin.Transport(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", redemption, &file.Identity); err != nil {
		t.Fatal(err)
	}
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	config.IdentityFile = path

	client, err := newTestClient(t, config)
	if err != nil {
		t.Fatal(err)
	}
	h.native, err = client.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	h.connector = fixtureConnector(h.native)
	saved, _ := schedulerFixtures.Load(h.scheduler)
	schedulerConfig := saved.(SchedulerConfig)
	schedulerConfig.Machine = h.scheduler.machine
	schedulerConfig.Machine.ID = file.Identity.MachineID
	schedulerConfig.Machine.BackendIsolation = report
	schedulerConfig.Machine.DisplayName = redemption.DisplayName
	h.scheduler, err = newTestScheduler(client, schedulerConfig)
	if err != nil {
		t.Fatal(err)
	}
}

func fixtureCapacity(t testing.TB, h *nativeChangeHub, capacity int) {
	t.Helper()
	file, err := runnerauth.Load(h.native.Transport().IdentityFile())
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/organizations/" + string(h.organization) + "/runners/" + file.Identity.RunnerID + "/routing"
	var current runnerauth.Runner
	if err := h.admin.Transport().Request(t.Context(), http.MethodGet, path, nil, &current); err != nil {
		t.Fatal(err)
	}
	current.CapacityLimit = capacity
	if err := h.admin.Transport().Request(t.Context(), http.MethodPut, path, runnerauth.RoutingChange{ExpectedRevision: current.Revision, Routing: current.Routing}, nil); err != nil {
		t.Fatal(err)
	}
	if err := h.admin.Transport().Request(t.Context(), http.MethodPut, "/api/v2/organizations/"+string(h.organization)+"/machines/"+string(file.Identity.MachineID)+"/routing", runnerauth.HostChange{ExpectedRevision: current.HostRevision, DisplayName: current.HostDisplayName, Capacity: capacity}, nil); err != nil {
		t.Fatal(err)
	}
	h.scheduler.machine.Capacity = capacity
}
