package nativechange

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/diagnostics"
	"github.com/digitaldrywood/detent/internal/hubclient/dispatch"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
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
func fixtureArtifactConfiguration(t testing.TB, scheduler *Scheduler, service string, bytes int64) {
	t.Helper()
	fixture := fixtureClient(scheduler.Transport())
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.config.ArtifactServiceID, fixture.config.ArtifactBytes = service, bytes
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
func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func newTestScheduler(client *Client, config SchedulerConfig) (*Scheduler, error) {
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
	config.ExecutionFactory = NewExecution
	config.CompletionRestorer = RestoreCompletion
	scheduler, err := dispatch.NewScheduler(client, config)
	if err == nil {
		schedulerFixtures.Store(scheduler, state)
	}
	return scheduler, err
}

type Scheduler = dispatch.Scheduler
type SchedulerConfig = dispatch.SchedulerConfig

var schedulerFixtures sync.Map

func fixtureSchedulerMachine(scheduler *Scheduler) Machine {
	value, ok := schedulerFixtures.Load(scheduler)
	if !ok {
		panic("scheduler fixture is not registered")
	}
	return value.(*schedulerFixture).config.Machine
}

type fixtureNativeClaim struct {
	source    *NativeConnector
	lease     tracker.NativeLease
	recovery  tracker.NativeRecovery
	deadline  time.Time
	lost      bool
	execution *nativeExecution
}

func fixtureClaims(scheduler *Scheduler) map[string]fixtureNativeClaim {
	result := make(map[string]fixtureNativeClaim)
	for _, view := range scheduler.ActiveExecutions() {
		var owner *nativeExecution
		if view.Execution != nil {
			owner = view.Execution.(*nativeExecution)
		}
		result[string(view.Lease.WorkItemID)] = fixtureNativeClaim{source: view.Source, lease: view.Lease, recovery: view.Recovery, deadline: view.Deadline, lost: view.Lost, execution: owner}
	}
	return result
}
func fixtureInstallClaim(t testing.TB, scheduler *Scheduler, claim nativeClaim, descriptor policy.Descriptor, project string) {
	t.Helper()
	if err := scheduler.SetNativeProjects(claim.source.NativeClient().OrganizationID(), map[string]tracker.ProjectID{project: claim.source.NativeClient().ProjectID()}, nil); err != nil {
		t.Fatal(err)
	}
	source := scheduler.ExecutionSource(project)
	var owned executioncontract.Execution
	if claim.execution != nil {
		owned = claim.execution
	}
	if err := scheduler.RegisterExecution(t.Context(), orchestrator.SchedulingRequest{ProjectID: project, Policy: descriptor}, connector.Issue{ID: string(claim.lease.WorkItemID)}, executioncontract.ExecutionClaim{Source: source, Lease: claim.lease, Recovery: claim.recovery, Deadline: claim.deadline}, owned); err != nil {
		t.Fatal(err)
	}
}

type schedulerFixture struct {
	config SchedulerConfig
	now    atomic.Pointer[time.Time]
}

func fixtureSetNow(scheduler *Scheduler, instant time.Time) {
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

func fixtureConfigureScheduler(t testing.TB, h *nativeChangeHub, configure func(*SchedulerConfig)) {
	t.Helper()
	value, ok := schedulerFixtures.Load(h.scheduler)
	if !ok {
		panic("scheduler fixture is not registered")
	}
	config := value.(*schedulerFixture).config
	configure(&config)
	scheduler, err := newTestScheduler(h.scheduler.Transport(), config)
	if err != nil {
		t.Fatal(err)
	}
	h.scheduler = scheduler
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
	raw, err := newTestClient(t, config)
	if err != nil {
		return err
	}
	return raw.Request(t.Context(), method, path, input, output)
}
func fixtureRawToken(client *Client) string {
	fixture := fixtureClient(client)
	fixture.mu.RLock()
	defer fixture.mu.RUnlock()
	return fixture.config.TokenSource()
}

func schedulerFixturesValue(scheduler *Scheduler) SchedulerConfig {
	value, ok := schedulerFixtures.Load(scheduler)
	if !ok {
		panic("scheduler fixture is not registered")
	}
	return value.(*schedulerFixture).config
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

func fixtureDiagnostics(scheduler *Scheduler, source *NativeConnector) []runnerauth.DiagnosticAttempt {
	return diagnostics.Claims(scheduler.ActiveExecutions(), source, scheduler.Now(), scheduler.LeaseDuration())
}
