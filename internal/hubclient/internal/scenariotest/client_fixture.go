package scenariotest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	"github.com/digitaldrywood/detent/internal/hubclient/dispatch"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"

	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func createTestClient(t testing.TB, config Config, constructors ...Constructors) (*Client, error) {
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
	if len(constructors) > 0 {
		fixture.constructors = constructors[0]
	}
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

func fixtureSchedulerMachine(scheduler *Scheduler) Machine {
	value, ok := schedulerFixtures.Load(scheduler)
	if !ok {
		panic("scheduler fixture is not registered")
	}
	return value.(*schedulerFixture).config.Machine
}

var schedulerFixtures sync.Map

type Scheduler = dispatch.Scheduler

type schedulerFixture struct {
	config SchedulerConfig
	now    atomic.Pointer[time.Time]
}

func fixtureClient(client *Client) *clientFixture {
	value, ok := clientFixtures.Load(client)
	if !ok {
		panic("client fixture is not registered")
	}
	return value.(*clientFixture)
}

type SchedulerConfig = dispatch.SchedulerConfig

func newTestScheduler(client *Client, config SchedulerConfig) (*Scheduler, error) {
	if config.Machine.DisplayName == "" {
		config.Machine.DisplayName = config.Machine.Hostname
	}
	if config.CapacityConfiguration == nil {
		limit := config.Machine.Capacity
		config.CapacityConfiguration = func(context.Context, *runnerauth.CapacityRequest) *runnerauth.CapacityConfig {
			return &runnerauth.CapacityConfig{Revision: policy.Digest([]byte("fixture capacity")), LocalLimit: limit, ClientLimit: limit, RuntimeLimit: limit, ObservedAt: time.Now().UTC()}
		}
	}
	clock := config.Now
	if clock == nil {
		clock = time.Now
	}
	state := &schedulerFixture{config: config}
	config.Now = func() time.Time {
		if instant := state.now.Load(); instant != nil {
			return *instant
		}
		return clock()
	}
	constructors := fixtureClient(client).constructors
	config.ExecutionFactory = func(host executioncontract.ExecutionHost, view executioncontract.ExecutionClaim) executioncontract.Execution {
		return wrap(constructors.New(host, view), view)
	}
	config.CompletionRestorer = func(host executioncontract.ExecutionHost, ctx context.Context, request orchestrator.SchedulingRequest, issue connector.Issue, saved json.RawMessage) (orchestrator.Claimed, error) {
		return checkpoint.RestoreCompletion(func(ctx context.Context, view executioncontract.ExecutionClaim, state checkpoint.State) (executioncontract.Execution, error) {
			actual, err := constructors.Recovered(host, ctx, view, state)
			if err != nil {
				return nil, err
			}
			return wrap(actual, view), nil
		}, host, ctx, request, issue, saved)
	}
	if config.CapacityConfiguration == nil && client.Enrolled() {
		capacity := max(1, config.Machine.Capacity)
		config.CapacityConfiguration = func(context.Context, *runnerauth.CapacityRequest) *runnerauth.CapacityConfig {
			return &runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, ObservedAt: config.Now()}
		}
	}
	scheduler, err := dispatch.NewScheduler(client, config)
	if err == nil {
		schedulerFixtures.Store(scheduler, state)
	}
	return scheduler, err
}

func emptyProcessSource(context.Context, string) ([]int, error) { return nil, nil }

var clientFixtures sync.Map

func runnerSpriteName(hostname string) string {
	info, err := os.Stat("/.sprite/api.sock")
	if err == nil && info.Mode()&os.ModeSocket != 0 {
		return hostname
	}
	return ""
}

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
	raw, err := createTestClient(t, config, fixture.constructors)
	if err != nil {
		return err
	}
	return raw.Request(t.Context(), method, path, input, output)
}

type clientFixture struct {
	requests        *testsupport.Transport
	renewals        *testsupport.Transport
	constructors    Constructors
	mu              sync.RWMutex
	config          Config
	baseURL         *url.URL
	offset          atomic.Int64
	httpClient      *http.Client
	leaseHTTPClient *http.Client
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

func fixtureConfigureScheduler(t testing.TB, h *nativeChangeHub, configure func(*SchedulerConfig)) {
	t.Helper()
	value, ok := schedulerFixtures.Load(h.scheduler)
	if !ok {
		panic("scheduler fixture is not registered")
	}
	config := value.(*schedulerFixture).config
	configure(&config)
	if config.Machine.Capacity != value.(*schedulerFixture).config.Machine.Capacity {
		config.CapacityConfiguration = nil
		file, err := runnerauth.Load(fixtureClient(h.scheduler.Transport()).config.IdentityFile)
		if err != nil {
			t.Fatal(err)
		}
		fleet, err := NewFleetClient(h.admin.Transport(), h.organization, nil)
		if err != nil {
			t.Fatal(err)
		}
		current, err := fleet.Fleet(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range current.Runners {
			if entry.RunnerID != file.Identity.RunnerID {
				continue
			}
			entry.Routing.CapacityLimit = config.Machine.Capacity
			if err := fleet.UpdateRunner(t.Context(), entry.RunnerID, runnerauth.RoutingChange{ExpectedRevision: entry.Revision, Routing: entry.Routing}); err != nil {
				t.Fatal(err)
			}
			if err := fleet.UpdateHost(t.Context(), entry.MachineID, runnerauth.HostChange{ExpectedRevision: entry.HostRevision, DisplayName: entry.HostDisplayName, Capacity: config.Machine.Capacity}); err != nil {
				t.Fatal(err)
			}
		}
	}
	scheduler, err := newTestScheduler(h.scheduler.Transport(), config)
	if err != nil {
		t.Fatal(err)
	}
	h.scheduler = scheduler
}

func SetSchedulerNow(scheduler *Scheduler, instant time.Time) {
	value, ok := schedulerFixtures.Load(scheduler)
	if !ok {
		panic("scheduler fixture is not registered")
	}
	if instant.IsZero() {
		value.(*schedulerFixture).now.Store(nil)
	} else {
		value.(*schedulerFixture).now.Store(&instant)
	}
}

func dispatchEnableEnrollment(t *testing.T, h *nativeChangeHub) {
	t.Helper()
	original := fixtureClient(h.native.Transport())
	config := original.config
	saved, _ := schedulerFixtures.Load(h.scheduler)
	schedulerConfig := saved.(*schedulerFixture).config
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := hubclientInitializeIdentity(path, config.URL)
	if err != nil {
		t.Fatal(err)
	}
	enrollment, err := h.admin.Transport().CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	report := isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}
	redemption := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: "host", DisplayName: "Fixture runner", Capacity: schedulerConfig.Machine.Capacity, Version: "test", BackendIsolation: report}
	if err := fixtureRunnerRequest(t, h.admin.Transport(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", redemption, &file.Identity); err != nil {
		t.Fatal(err)
	}
	if err := hubclientSaveIdentity(path, file); err != nil {
		t.Fatal(err)
	}
	config.IdentityFile = path

	client, err := createTestClient(t, config, original.constructors)
	if err != nil {
		t.Fatal(err)
	}
	h.native, err = client.Native(h.organization, h.project)
	if err != nil {
		t.Fatal(err)
	}
	h.connector.worker, err = NewNativeConnector(h.native)
	if err != nil {
		t.Fatal(err)
	}

	schedulerConfig.Machine.ID = file.Identity.MachineID
	schedulerConfig.Machine.BackendIsolation = report
	schedulerConfig.Machine.DisplayName = redemption.DisplayName
	h.scheduler, err = newTestScheduler(client, schedulerConfig)
	if err != nil {
		t.Fatal(err)
	}
}
