package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type executionTransport struct {
	next http.RoundTripper
	drop atomic.Bool
	down atomic.Bool
}

func TestControllerRetirementWaitsForOldMachineReport(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var workers sync.WaitGroup
	defer func() { releaseOnce.Do(func() { close(release) }); workers.Wait() }()
	var reports atomic.Int64
	client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "fixture-token" }, HTTPClient: &http.Client{Transport: executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		if reports.Add(1) == 1 {
			close(entered)
			<-release
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"fixture-machine","hostname":"fixture-host","capacity":1,"version":"1.0.0"}`)), Request: request}, nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "fixture-machine", Hostname: "fixture-host", Capacity: 1, Version: "1.0.0"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	reported := make(chan error, 1)
	workers.Go(func() { reported <- source.ensureMachine(t.Context()) })
	<-entered
	paused := make(chan func(), 1)
	workers.Go(func() { paused <- source.PauseController() })
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case <-paused:
			t.Fatal("controller retired before its old machine report completed")
		default:
		}
		if source.controlMu.TryRLock() {
			source.controlMu.RUnlock()
			return errors.New("controller retirement has not reached the in-flight report")
		}
		return nil
	})
	releaseOnce.Do(func() { close(release) })
	if err := <-reported; err != nil {
		t.Fatal(err)
	}
	resume := <-paused
	source.lastHeartbeat = time.Time{}
	if err := source.ensureMachine(t.Context()); err != nil || reports.Load() != 1 {
		t.Fatalf("retired controller published an old build: reports=%d err=%v", reports.Load(), err)
	}
	resume()
	if err := source.ensureMachine(t.Context()); err != nil || reports.Load() != 2 {
		t.Fatalf("failed adoption did not restore machine reporting: reports=%d err=%v", reports.Load(), err)
	}
}

func TestRetiredControllerRenewsClaimsWithoutReportingOldBuild(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Finish the active attempt")
	h.claim(t, issue.ID)
	source := h.scheduler.nativeProject("local")
	if source == nil || source.NativeClient() == nil {
		t.Fatal("missing native project source")
	}
	transport := fixtureClient(h.native.Transport()).requests.Load()
	var reports atomic.Int64
	fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost && (strings.Contains(request.URL.Path, "/machines/") || strings.HasSuffix(request.URL.Path, "/machines")) {
			reports.Add(1)
		}
		return transport.RoundTrip(request)
	}))
	t.Cleanup(func() { fixtureClient(h.native.Transport()).requests.Store(transport) })
	resume := h.scheduler.PauseController()
	for _, resumed := range []bool{false, true} {
		if resumed {
			resume()
		}
		h.scheduler.mu.Lock()
		h.scheduler.nativeHeartbeats[source.NativeClient().ProjectID()] = time.Time{}
		h.scheduler.mu.Unlock()
		before := reports.Load()
		if _, err := h.scheduler.RenewClaim(t.Context(), issue.ID, time.Now()); err != nil {
			t.Fatalf("renew active lease while resumed=%t: %v", resumed, err)
		}
		if sent := reports.Load() > before; sent != resumed {
			t.Fatalf("controller machine reports after renewal: sent=%t resumed=%t", sent, resumed)
		}
	}
	resume = h.scheduler.PauseController()
	defer resume()
	claim := h.scheduler.nativeClaims[issue.ID]
	if err := source.NativeClient().Release(t.Context(), claim.lease, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.scheduler.RenewClaim(t.Context(), issue.ID, time.Now()); !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
		t.Fatalf("retirement bypassed authoritative lease loss: %v", err)
	}
}

func (t *executionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.down.Load() {
		return nil, errors.New("injected Hub outage")
	}
	response, err := t.next.RoundTrip(request)
	if err == nil && strings.HasSuffix(request.URL.Path, "/events") && t.drop.Swap(false) {
		if closeErr := response.Body.Close(); closeErr != nil {
			return nil, closeErr
		}
		return nil, errors.New("injected acknowledgment loss after commit")
	}
	return response, err
}

type executionRoundTrip func(*http.Request) (*http.Response, error)

func (f executionRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestNativeLeaseReceiptDoesNotExtendReplayedClaim(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		server    time.Time
		remaining time.Duration
	}{
		{"fresh claim", started.Add(time.Hour), time.Minute},
		{"replayed claim", started.Add(time.Hour + 50*time.Second), 10 * time.Second},
		{"missing server time", time.Time{}, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			lease := tracker.NativeLease{ServerTime: test.server, RenewedAt: started.Add(time.Hour), ExpiresAt: started.Add(time.Hour + time.Minute)}
			if got := nativeLeaseDeadline(started, lease); !got.Equal(started.Add(test.remaining)) {
				t.Fatalf("local deadline = %v", got)
			}
		})
	}
}

func TestNativeMissingClaimNeverRunsUnguarded(t *testing.T) {
	t.Parallel()
	scheduler := &Scheduler{nativeClaims: make(map[string]nativeClaim), executionFactory: nativechange.NewExecution}
	execution := scheduler.RunExecution("wi_" + strings.Repeat("a", 32))
	if execution == nil {
		t.Fatal("missing native claim disabled the execution guard")
	}
	_, stop, err := execution.Guard(t.Context())
	defer stop()
	if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("guard = %v", err)
	}
	if err := scheduler.ReleaseClaim(t.Context(), "wi_"+strings.Repeat("a", 32), "completed"); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
		t.Fatalf("missing native completion owner reported settlement: %v", err)
	}
	if err := scheduler.ReleaseClaim(t.Context(), "github-node", "completed"); err != nil {
		t.Fatalf("legacy idempotent release = %v", err)
	}
	if scheduler.RunExecution("github-node") != nil {
		t.Fatal("legacy execution acquired a native guard")
	}
}

func TestDedicatedClaimRenewalDoesNotWaitForOtherWork(t *testing.T) {
	for _, operation := range []string{"change read", "machine report", "progress admission", "request budget"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{{Name: "In Progress", Dispatchable: true}}, true)
				issue := h.createInProgress(t, "Independent renewal")
				h.claim(t, issue.ID)
				entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				defer func() { once.Do(func() { close(release) }); <-done }()
				transport := fixtureClient(h.native.Transport()).requests.Load()
				var renewals atomic.Int64
				observedTransport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/changes/") {
						close(entered)
						<-release
						return nil, errors.New("read released")
					}
					if strings.HasSuffix(r.URL.Path, "/renew") {
						renewals.Add(1)
					}
					return transport.RoundTrip(r)
				})
				fixtureClient(h.native.Transport()).requests.Store(observedTransport)
				fixtureClient(h.native.Transport()).renewals.Store(observedTransport)
				switch operation {
				case "machine report":
					h.scheduler.isolationReport = func(context.Context) (isolation.Report, []runnerauth.Problem) {
						close(entered)
						<-release
						return nil, nil
					}
					h.scheduler.nativeHeartbeats[h.project] = time.Time{}
				case "progress admission":
					h.scheduler.sessionID = func() (string, error) {
						close(entered)
						<-release
						return "", errors.New("admission released")
					}
				case "request budget":
					h.native.Transport().DeferRequestBudget("/items", time.Now().Add(time.Minute))
				}
				go func() {
					defer close(done)
					switch operation {
					case "change read":
						_, _ = h.native.Change(t.Context(), tracker.NativeWorkItemID(issue.ID), "change_example")
					case "machine report":
						_ = h.scheduler.ensureNativeMachine(t.Context(), h.connector)
					case "progress admission":
						_, _, _ = h.scheduler.ReadmitRetryProgress(t.Context(), runner.RunRequest{Issue: issue, Execution: h.scheduler.RunExecution(issue.ID)}, nil)
					case "request budget":
						close(entered)
						<-release
					}
				}()
				<-entered
				result := make(chan error, 1)
				started := time.Now()
				go func() {
					ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
					defer cancel()
					var err error
					if source, ok := any(h.scheduler).(interface {
						RenewClaimHeartbeat(context.Context, string, time.Time) (orchestrator.Claimed, error)
					}); ok {
						_, err = source.RenewClaimHeartbeat(ctx, issue.ID, time.Now())
					} else {
						_, err = h.scheduler.RenewClaim(ctx, issue.ID, time.Now())
					}
					result <- err
				}()
				synctest.Wait()
				select {
				case err := <-result:
					if err != nil || renewals.Load() != 1 || !time.Now().Equal(started) {
						t.Errorf("renewal waited for %s: calls=%d elapsed=%s error=%v", operation, renewals.Load(), time.Since(started), err)
					}
				default:
					t.Errorf("renewal blocked behind %s", operation)
					once.Do(func() { close(release) })
					<-result
				}
			})
		})
	}
}

func TestDedicatedClaimRenewalUsesIndependentConnectionPool(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	lease := tracker.NativeLease{ID: "lease_example", WorkItemID: "wi_example", FencingToken: 5, ServerTime: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/changes/") {
			close(entered)
			<-release
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/renew") || r.Method != http.MethodPost {
			t.Errorf("unexpected heartbeat request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewEncoder(w).Encode(lease); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 1
	defer transport.CloseIdleConnections()
	client, err := newTestClient(t, Config{URL: server.URL, TokenSource: func() string { return "example" }, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	defer fixtureClient(client).leaseHTTPClient.CloseIdleConnections()
	native, err := client.Native("org_example", "prj_example")
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine_example", Hostname: "example", Capacity: 1, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	scheduler.nativeClaims["wi_example"] = nativeClaim{source: fixtureConnector(native), lease: lease}
	done := make(chan error, 1)
	ctx := testenv.WaitContext(t, t.Context())
	go func() { _, err := native.Change(ctx, "wi_example", "change_example"); done <- err }()
	defer func() { close(release); <-done }()
	<-entered
	renewed, err := scheduler.RenewClaimHeartbeat(ctx, "wi_example", time.Now())
	if err != nil || !renewed.LeaseExpiresAt.Equal(lease.ExpiresAt) {
		t.Fatalf("renewal waited behind the occupied connection: %v", err)
	}
}

func TestNativeExecutionContextKeepsItsOriginalFence(t *testing.T) {
	t.Parallel()
	client := &Client{}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	old := tracker.NativeLease{ID: "old", WorkItemID: "wi_test", FencingToken: 1}
	current := old
	current.ID, current.FencingToken = "new", 2
	client.RememberLease(native.Scope(), current.WorkItemID, current)
	bound := api.WithMutationAuthority(t.Context(), native.Scope(), old, nil)
	for _, test := range []struct {
		name  string
		ctx   func() context.Context
		token tracker.FencingToken
	}{
		{"old execution", func() context.Context { return bound }, 1},
		{"detached local epilogue", func() context.Context { return context.WithoutCancel(bound) }, 1},
		{"current controller", t.Context, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutation := native.FencedMutation(test.ctx(), "wi_test", tracker.Mutation{IdempotencyKey: test.name})
			if mutation.FencingToken != test.token {
				t.Fatalf("mutation acquired another execution's authority: %#v", mutation)
			}
		})
	}
}

func TestNativeAvailabilityDeadline(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(strconv.FormatBool(deadline), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				descriptor := clientTestPolicy()
				transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					body, err := json.Marshal(policy.Approval{Policy: descriptor})
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
				})
				client, err := newTestClient(t, Config{URL: "https://hub.example.test", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: 2 * time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				id := "wi_test"
				claim := nativeClaim{source: fixtureConnector(native), lease: tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(id), ID: "lease", FencingToken: 1, PolicyID: descriptor.ID}, deadline: time.Now().Add(2 * time.Minute)}
				if deadline {
					claim.availabilityDeadline = time.Now().Add(30 * time.Second)
				}
				scheduler.nativeClaims[id] = claim
				scheduler.claimPolicies[id] = claimPolicy{project: "project", descriptor: descriptor}
				scheduler.nativeProjects["project"] = claim.source
				execution := scheduler.RunExecution(id)
				guarded, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(29 * time.Second)
				synctest.Wait()
				if guarded.Err() != nil {
					t.Fatal("job stopped before hard deadline")
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if deadline {
					if !runner.IsAvailabilityInterruption(context.Cause(guarded)) {
						t.Fatalf("cause = %v", context.Cause(guarded))
					}
					if errors.Is(execution.Validate(guarded), runner.ErrExecutionAuthorityUnavailable) {
						t.Fatal("availability deadline revoked execution authority")
					}
				} else if guarded.Err() != nil {
					t.Fatal("job without hard deadline stopped")
				}
				if err := execution.Finish(context.WithoutCancel(guarded), "interrupted"); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestNativeAvailabilityDeadlineRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time lifecycle and timeout integration")
	}

	for _, change := range []string{"remove", "extend", "add", "shorten", "closed"} {
		t.Run(change, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				identityPath := filepath.Join(t.TempDir(), "private", "runner.json")
				file, err := runnerauth.Initialize(identityPath, "https://hub.example.test")
				if err != nil {
					t.Fatal(err)
				}
				descriptor := clientTestPolicy()
				transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					body, err := json.Marshal(policy.Approval{Policy: descriptor})
					if strings.HasSuffix(request.URL.Path, "/validate") {
						body, err = json.Marshal(runnerauth.Runner{Binding: file.Identity.Binding, OrganizationID: file.Identity.OrganizationID})
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, err
				})
				client, err := newTestClient(t, Config{URL: file.HubURL, Enrollment: api.NewRunnerState(identityPath), TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				file.Identity.OrganizationID = "org_test"
				file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
				if err := runnerauth.Save(identityPath, file); err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				closeTime := now.Truncate(time.Minute).Add(time.Minute)
				availability := runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun " + now.Add(-time.Minute).Format("15:04") + "-" + closeTime.Format("15:04")}, HardDeadline: "30s"}
				if change == "add" {
					availability.HardDeadline = ""
				}
				if change == "shorten" {
					availability.Windows[0] = "Mon-Sun " + now.Add(-time.Minute).Format("15:04") + "-" + closeTime.Add(3*time.Minute).Format("15:04")
				}
				client.Enrollment().ApplyRouting(runnerauth.RoutingSnapshot{Routing: runnerauth.Routing{Availability: availability}})
				scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: file.Identity.MachineID, Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: 10 * time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_test", "prj_test")
				if err != nil {
					t.Fatal(err)
				}
				claim := nativeClaim{source: fixtureConnector(native), lease: tracker.NativeLease{WorkItemID: "wi_test", ID: "lease", FencingToken: 1, PolicyID: descriptor.ID}, deadline: now.Add(10 * time.Minute)}
				scheduler.nativeClaims["wi_test"] = claim
				scheduler.claimPolicies["wi_test"] = claimPolicy{project: "project", descriptor: descriptor}
				scheduler.nativeProjects["project"] = claim.source
				execution := scheduler.RunExecution("wi_test")
				guarded, stop, err := execution.Guard(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer stop()
				synctest.Wait()
				time.Sleep(30 * time.Second)
				switch change {
				case "remove":
					availability.HardDeadline = ""
				case "extend":
					availability.Windows[0] = "Mon-Sun " + now.Add(-time.Minute).Format("15:04") + "-" + closeTime.Add(3*time.Minute).Format("15:04")
				case "add":
					availability.HardDeadline = "30s"
				case "shorten":
					availability.Windows[0] = "Mon-Sun " + now.Add(-time.Minute).Format("15:04") + "-" + closeTime.Format("15:04")
				case "closed":
					availability.Windows[0] = "Mon-Sun " + now.Add(-2*time.Minute).Format("15:04") + "-" + now.Format("15:04")
				}
				client.Enrollment().ApplyRouting(runnerauth.RoutingSnapshot{Routing: runnerauth.Routing{Availability: availability}})
				synctest.Wait()
				time.Sleep(time.Until(closeTime.Add(30 * time.Second)))
				synctest.Wait()
				wantStopped := change != "remove" && change != "extend"
				if (guarded.Err() != nil) != wantStopped {
					t.Fatalf("stopped = %v, want %t", guarded.Err(), wantStopped)
				}
				if wantStopped && !runner.IsAvailabilityInterruption(context.Cause(guarded)) {
					t.Fatalf("interruption cause = %v", context.Cause(guarded))
				}
			})
		})
	}
}

func TestNativeProgressStartupRecoverySnapshot(t *testing.T) {
	t.Parallel()
	h := newNativeChangeHub(t)
	issue := h.createInProgress(t, "Concurrent progress startup")
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID)
	publishing := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	releasePublication := func() { once.Do(func() { close(release) }) }
	transport := fixtureClient(h.native.Transport()).requests.Load()
	fixtureClient(h.native.Transport()).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events") {
			close(publishing)
			select {
			case <-release:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		}
		return transport.RoundTrip(request)
	}))
	finished := make(chan struct{})
	var readDone chan struct{}
	var startErr error
	t.Cleanup(func() {
		releasePublication()
		<-finished
		if readDone != nil {
			<-readDone
		}
	})
	go func() {
		defer close(finished)
		startErr = execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"})
	}()
	select {
	case <-publishing:
	case <-finished:
		t.Fatalf("progress startup failed before publication: %v", startErr)
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("progress startup did not publish its execution")
	}
	snapshots := make(chan tracker.NativeRecovery, 1)
	readDone = make(chan struct{})
	go func() { defer close(readDone); snapshots <- execution.Recovery() }()
	select {
	case snapshot := <-snapshots:
		if string(snapshot.Issue.WorkItemID) != issue.ID || snapshot.Lease.WorkItemID != snapshot.Issue.WorkItemID {
			t.Fatalf("progress startup changed recovery scope: %+v", snapshot.Issue.NativeReference)
		}
	case <-testenv.WaitContext(t, t.Context()).Done():
		t.Fatal("scheduler recovery read waited for progress publication")
	}
	releasePublication()
	<-finished
	if startErr != nil {
		t.Fatal(startErr)
	}
}

func TestNativeGuardDeadlineAndRenewal(t *testing.T) {
	for _, name := range []string{"expire", "renew", "renewal outage", "renewal outage 502", "renewal outage 503", "renew during native finish", "guard during native finish", "budget wait", "budget wait loses lease"} {
		renew := name == "renew"
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				descriptor := clientTestPolicy()
				var offline atomic.Bool
				var budgetWait atomic.Bool
				var budgetReads, budgetRenewals atomic.Int32
				var finished tracker.NativeRunEvent
				transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					if offline.Load() {
						status := 0
						switch name {
						case "renewal outage 502":
							status = http.StatusBadGateway
						case "renewal outage 503":
							status = http.StatusServiceUnavailable
						}
						if status != 0 {
							return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unavailable"}`)), Request: request}, nil
						}
						return nil, errors.New("renewal disconnected")
					}
					body := []byte(`{}`)
					if strings.HasSuffix(request.URL.Path, "/renew") {
						if budgetWait.Load() {
							budgetRenewals.Add(1)
							if name == "budget wait" {
								lease := tracker.NativeLease{ID: "lease", FencingToken: 1, PolicyID: descriptor.ID, ServerTime: time.Now(), RenewedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
								body, err := json.Marshal(lease)
								return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, err
							}
						}
						return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"stale_fencing_token"}`)), Request: request}, nil
					}
					if strings.HasSuffix(request.URL.Path, "/policy") {
						if budgetWait.Load() && budgetReads.Add(1) == 1 {
							return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"60"}}, Body: io.NopCloser(strings.NewReader(`{"code":"runner_request_budget","message":"Runner request budget exceeded; retry after the current minute"}`)), Request: request}, nil
						}
						var err error
						body, err = json.Marshal(policy.Approval{Policy: descriptor})
						if err != nil {
							return nil, err
						}
					}
					if strings.HasPrefix(name, "budget wait") && strings.HasSuffix(request.URL.Path, "/events") {
						var event tracker.NativeRunEvent
						if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
							return nil, err
						}
						if event.Type == "run.finished" {
							finished = event
						}
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
				})
				client, err := newTestClient(t, Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native(tracker.OrganizationID("org_"+strings.Repeat("a", 32)), tracker.ProjectID("prj_"+strings.Repeat("b", 32)))
				if err != nil {
					t.Fatal(err)
				}
				source, _ := NewNativeConnector(native)
				id := "wi_" + strings.Repeat("c", 32)
				scheduler.nativeProjects["project"] = source
				scheduler.nativeClaims[id] = nativeClaim{source: source, lease: tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(id), ID: "lease", FencingToken: 1, PolicyID: descriptor.ID}, deadline: time.Now().Add(time.Minute)}
				scheduler.claimPolicies[id] = claimPolicy{project: "project", descriptor: descriptor}
				execution := scheduler.RunExecution(id)
				guarded := t.Context()
				if name != "renew during native finish" && name != "guard during native finish" {
					var stop func()
					guarded, stop, err = execution.Guard(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer stop()
				}
				if strings.HasPrefix(name, "budget wait") {
					scheduler.heartbeatInterval = 2 * time.Minute
					scheduler.nativeHeartbeats[native.ProjectID()] = time.Now()
					if err := execution.Start(guarded, tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
						t.Fatal(err)
					}
					budgetWait.Store(true)
					start := time.Now()
					execution.(fixtureLifecycle).HostOperation("policy.read", start, nil, true)
					_, err := native.ProjectPolicy(guarded)
					execution.(fixtureLifecycle).HostOperation("policy.read", time.Now(), err, false)
					if budgetRenewals.Load() != 1 {
						t.Fatalf("budget wait blocked lease renewal: renewals=%d reads=%d elapsed=%s error=%v cause=%v", budgetRenewals.Load(), budgetReads.Load(), time.Since(start), err, context.Cause(guarded))
					}
					if name == "budget wait loses lease" {
						if !errors.Is(err, context.Canceled) || !errors.Is(context.Cause(guarded), runner.ErrExecutionAuthorityUnavailable) || budgetReads.Load() != 1 || time.Since(start) != 55*time.Second {
							t.Fatalf("wait outlived lease: error=%v cause=%v reads=%d elapsed=%s", err, context.Cause(guarded), budgetReads.Load(), time.Since(start))
						}
						return
					}
					if err != nil || guarded.Err() != nil || budgetReads.Load() != 2 || time.Since(start) != time.Minute {
						t.Fatalf("budget retry: error=%v cause=%v reads=%d elapsed=%s", err, context.Cause(guarded), budgetReads.Load(), time.Since(start))
					}
					if err := execution.Finish(guarded, "succeeded"); err != nil {
						t.Fatal(err)
					}
					if finished.Data.Outcome != "succeeded" || finished.Data.Sequence != 2 {
						t.Fatalf("attempt did not complete: %+v", finished)
					}
					return
				}
				if name == "renew during native finish" || name == "guard during native finish" {
					claim := scheduler.nativeClaims[id]
					scheduler.claims[id] = nativeTrackerLease(claim.lease)
					scheduler.nativeHeartbeats[native.ProjectID()] = time.Now()
					nativeExecution := fixtureRecoverExecution(t, scheduler, id, func(state *checkpoint.State) {
						state.Data.Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
						state.PreparedOutcome = "failed"
					})
					execution = nativeExecution
					var stopGuard func()
					defer func() {
						if stopGuard != nil {
							stopGuard()
						}
					}()
					started, allowFinish := make(chan struct{}), make(chan struct{})
					defer func() {
						select {
						case <-allowFinish:
						default:
							close(allowFinish)
						}
					}()
					original := fixtureClient(client).requests.Load()
					finishedEvents := 0
					fixtureClient(client).requests.Store(executionRoundTrip(func(request *http.Request) (*http.Response, error) {
						if strings.HasSuffix(request.URL.Path, "/renew") {
							lease := claim.lease
							lease.ServerTime, lease.RenewedAt = time.Now(), time.Now()
							lease.ExpiresAt = time.Now().Add(time.Minute)
							body, err := json.Marshal(lease)
							if err != nil {
								return nil, err
							}
							return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
						}
						if strings.HasSuffix(request.URL.Path, "/events") {
							var event tracker.NativeRunEvent
							if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
								return nil, err
							}
							if event.Type == "run.finished" {
								finishedEvents++
								close(started)
								<-allowFinish
							}
						}
						return original.RoundTrip(request)
					}))
					if name == "guard during native finish" {
						guarded, stopGuard, err = execution.Guard(t.Context())
						if err != nil {
							t.Fatal(err)
						}
						synctest.Wait()
					}
					finished := make(chan error, 1)
					go func() { finished <- scheduler.ReleaseClaim(guarded, id, "released") }()
					<-started
					for range 7 {
						time.Sleep(20 * time.Second)
						if name == "guard during native finish" {
							synctest.Wait()
							scheduler.mu.Lock()
							deadline := scheduler.nativeClaims[id].deadline
							scheduler.mu.Unlock()
							if deadline.Before(time.Now()) {
								t.Fatal("completion publication blocked the worker lease guard")
							}
							continue
						}
						renewed := make(chan error, 1)
						go func() {
							_, err := scheduler.RenewClaim(guarded, id, time.Now())
							renewed <- err
						}()
						synctest.Wait()
						select {
						case err := <-renewed:
							if err != nil {
								t.Fatalf("renew existing native lease during Finish: %v", err)
							}
						default:
							t.Fatal("renewal waited for completion publication")
						}
						current := scheduler.nativeClaims[id]
						if current.lease.ID != claim.lease.ID || current.lease.FencingToken != claim.lease.FencingToken {
							t.Fatal("slow Finish replaced existing lease authority")
						}
						if guarded.Err() != nil || scheduler.RemainingExecution(claim.lease) <= 0 {
							t.Fatal("slow Finish lost the existing fenced owner")
						}
					}
					close(allowFinish)
					if err := <-finished; err != nil || finishedEvents != 1 {
						t.Fatalf("Finish publication: events=%d error=%v", finishedEvents, err)
					}
					if stopGuard != nil {
						stopGuard()
						stopGuard = nil
					}
					if scheduler.RemainingExecution(claim.lease) != 0 || len(scheduler.nativeClaims) != 0 || len(scheduler.claims) != 0 {
						t.Fatal("terminal acknowledgement retained lease ownership")
					}
					return
				}
				time.Sleep(30 * time.Second)
				if strings.HasPrefix(name, "renewal outage") {
					offline.Store(true)
					_, err := scheduler.renewNativeClaim(t.Context(), id, scheduler.nativeClaims[id], 0)
					if err == nil || errors.Is(err, orchestrator.ErrSchedulingClaimLost) || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("transient renewal revoked current lease: %v", err)
					}
					if err := execution.Validate(guarded); err != nil || guarded.Err() != nil {
						t.Fatalf("outage ended valid worker: %v", err)
					}
				}
				if renew {
					scheduler.mu.Lock()
					claim := scheduler.nativeClaims[id]
					claim.deadline = time.Now().Add(time.Minute)
					scheduler.nativeClaims[id] = claim
					scheduler.mu.Unlock()
				}
				time.Sleep(25 * time.Second)
				synctest.Wait()
				if renew {
					if guarded.Err() != nil {
						t.Fatal("renewed owner stopped at old deadline")
					}
					time.Sleep(30 * time.Second)
					synctest.Wait()
				}
				if strings.HasPrefix(name, "renewal outage") {
					time.Sleep(4 * time.Minute)
					synctest.Wait()
					if guarded.Err() != nil {
						t.Fatalf("outage canceled worker: %v", context.Cause(guarded))
					}
					offline.Store(false)
					time.Sleep(30 * time.Second)
					synctest.Wait()
				}
				if !errors.Is(context.Cause(guarded), runner.ErrExecutionAuthorityUnavailable) {
					t.Fatalf("expiry cause = %v", context.Cause(guarded))
				}
			})
		})
	}
}

func TestDedicatedClaimRenewalPreservesFencingDuringReadmission(t *testing.T) {
	for _, outcome := range []string{"rejected during admission", "rejected after replacement", "acknowledged after replacement", "acknowledged after newer renewal", "revoked", "expired runtime"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newNativeChangeHubTransport(t, "In Review", []tracker.NativeState{{Name: "In Progress", Dispatchable: true}}, true)
				issue := h.createInProgress(t, "Fenced renewal")
				h.claim(t, issue.ID)
				h.scheduler.RunExecution(issue.ID)
				claim := h.scheduler.nativeClaims[issue.ID]
				if outcome == "revoked" {
					if err := h.native.Release(t.Context(), claim.lease, "cancelled"); err != nil {
						t.Fatal(err)
					}
				}
				if outcome == "expired runtime" {
					fixtureRecoverExecution(t, h.scheduler, issue.ID, func(state *checkpoint.State) {
						state.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: "implementation", HeartbeatAt: time.Now().Add(-2 * h.scheduler.leaseTTL)}
					})
				}
				if outcome == "rejected during admission" {
					claim.lifecycle.Lock()
					defer claim.lifecycle.Unlock()
				}
				var successor tracker.NativeLease
				transport := fixtureClient(h.native.Transport()).renewals.Load()
				fixtureClient(h.native.Transport()).renewals.Store(executionRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(outcome, "after replacement") || outcome == "acknowledged after newer renewal" {
						successor = claim.lease
						if outcome != "acknowledged after newer renewal" {
							successor.ID = "lease_successor"
							successor.FencingToken++
						}
						successor.RenewedAt = time.Now().Add(time.Second)
						successor.ExpiresAt = time.Now().Add(2 * time.Minute)
						h.scheduler.mu.Lock()
						current := h.scheduler.nativeClaims[issue.ID]
						current.lease = successor
						h.scheduler.nativeClaims[issue.ID] = current
						h.scheduler.claims[issue.ID] = nativeTrackerLease(successor)
						h.scheduler.mu.Unlock()
					}
					if strings.HasPrefix(outcome, "rejected") {
						return &http.Response{StatusCode: http.StatusConflict, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"stale_fencing_token"}`)), Request: r}, nil
					}
					return transport.RoundTrip(r)
				}))
				renewed, err := h.scheduler.RenewClaimHeartbeat(t.Context(), issue.ID, time.Now())
				switch outcome {
				case "rejected during admission":
					if !errors.Is(err, ErrUnavailable) || errors.Is(err, orchestrator.ErrSchedulingClaimLost) || h.scheduler.nativeClaims[issue.ID].lost {
						t.Fatalf("in-flight admission lost its claim: %v", err)
					}
				case "rejected after replacement", "acknowledged after replacement", "acknowledged after newer renewal":
					current := h.scheduler.nativeClaims[issue.ID]
					if err != nil || current.lost || current.lease.ID != successor.ID || current.lease.FencingToken != successor.FencingToken || !renewed.LeaseExpiresAt.Equal(successor.ExpiresAt) {
						t.Fatalf("obsolete reply damaged successor: claim=%+v error=%v", current.lease, err)
					}
				default:
					if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
						t.Fatalf("inactive lease retained authority: %v", err)
					}
				}
			})
		})
	}
}

func TestNativeDelayedResponsesPreserveSuccessor(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"renew", "renew_landed", "renew_availability", "release", "release_finish_lost", "release_finish_ack_retry", "lost", "lost_release"} {
		t.Run(operation, func(t *testing.T) {
			descriptor := clientTestPolicy()
			id := "wi_" + strings.Repeat("a", 32)
			lease := tracker.NativeLease{ID: "old", WorkItemID: tracker.NativeWorkItemID(id), FencingToken: 1, PolicyID: descriptor.ID, ServerTime: time.Now(), ExpiresAt: time.Now().Add(time.Minute)}
			next := lease
			next.ID, next.FencingToken = "new", 2
			availabilityDeadline := time.Now().Add(30 * time.Second)
			if operation == "renew_availability" {
				next = lease
			}
			var scheduler *Scheduler
			var releaseCalls, finishedEvents int
			var observedAttempt tracker.NativeRunData
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				var payload any = policy.Approval{Policy: descriptor}
				if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/work-items/") {
					payload = tracker.NativeIssue{SchedulingEligible: new(true), DependencyReady: true}
					if operation == "renew_landed" {
						payload = tracker.NativeIssue{Terminal: true, SchedulingEligible: new(false), DependencyReady: true}
					}
				}
				if strings.HasSuffix(request.URL.Path, "/events") {
					var event tracker.NativeRunEvent
					if err := json.NewDecoder(request.Body).Decode(&event); err != nil {
						t.Fatal(err)
					}
					if event.Type == "run.finished" {
						finishedEvents++
					}
				}
				if strings.HasSuffix(request.URL.Path, "/release") && operation == "release_finish_ack_retry" {
					releaseCalls++
					if releaseCalls == 1 {
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"tenant_unavailable"}`)), Request: request}, nil
					}
				}
				endpoint := strings.Split(operation, "_")[0]
				if operation == "lost_release" {
					endpoint = "release"
				}
				attemptRead := operation == "release_finish_lost" && request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/attempts/")
				if strings.HasSuffix(request.URL.Path, "/"+endpoint) || attemptRead {
					scheduler.mu.Lock()
					claim := scheduler.nativeClaims[id]
					payload = lease
					if attemptRead {
						payload = tracker.NativeAttempt{NativeRunData: observedAttempt}
					}
					claim.lease = next
					if operation == "lost_release" {
						claim.lost = false
					}
					if operation == "renew_availability" {
						claim.availabilityDeadline = availabilityDeadline
					}
					scheduler.nativeClaims[id] = claim
					scheduler.claims[id] = nativeTrackerLease(next)
					scheduler.mu.Unlock()
				}
				body, err := json.Marshal(payload)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
			})
			client, err := newTestClient(t, Config{URL: "http://hub.example", TokenSource: func() string { return "test" }, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err = newTestScheduler(client, SchedulerConfig{Machine: Machine{ID: "machine", Hostname: "host", Version: "test", Capacity: 1}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_test", "prj_test")
			if err != nil {
				t.Fatal(err)
			}
			source, _ := NewNativeConnector(native)
			scheduler.nativeProjects["project"] = source
			scheduler.nativeHeartbeats[native.ProjectID()] = time.Now()
			scheduler.nativeClaims[id] = nativeClaim{source: source, lease: lease}
			scheduler.claims[id] = nativeTrackerLease(lease)
			scheduler.claimPolicies[id] = claimPolicy{project: "project", descriptor: descriptor}
			if operation == "renew_landed" {
				execution := scheduler.RunExecution(id).(Execution)
				claim := scheduler.nativeClaims[id]
				claim.execution = landedFixture{execution}
				scheduler.nativeClaims[id] = claim
			}
			switch operation {
			case "renew_availability":
				_, err = scheduler.renewNativeClaim(t.Context(), id, scheduler.nativeClaims[id], 0)
				if err != nil || !scheduler.nativeClaims[id].availabilityDeadline.Equal(availabilityDeadline) {
					t.Fatalf("delayed renewal replaced refreshed availability: %v, %v", scheduler.nativeClaims[id].availabilityDeadline, err)
				}
			case "renew", "renew_landed":
				_, err = scheduler.renewNativeClaim(t.Context(), id, scheduler.nativeClaims[id], 0)
				if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
					t.Fatalf("old renewal = %v", err)
				}
			case "release_finish_ack_retry":
				claim := scheduler.nativeClaims[id]
				claim.deadline = time.Now().Add(time.Minute)
				scheduler.nativeClaims[id] = claim
				execution := fixtureRecoverExecution(t, scheduler, id, func(state *checkpoint.State) {
					state.PreparedOutcome = "failed"
					state.Data.Identity = &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
				})
				cleanupCalls := 0
				if err := execution.KeepWorker(func(context.Context) error {
					cleanupCalls++
					if execution.CompletionData().Outcome != "failed" || finishedEvents != 1 {
						t.Fatal("worker ownership ended before terminal publication was acknowledged")
					}
					return nil
				}, nil); err != nil {
					t.Fatal(err)
				}
				if err := scheduler.ReleaseClaim(t.Context(), id, "released"); err == nil || errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
					t.Fatalf("release outage = %v", err)
				}
				if execution.CompletionData().Outcome != "failed" || finishedEvents != 1 {
					t.Fatalf("Finish acknowledgement = %q, events = %d", execution.CompletionData().Outcome, finishedEvents)
				}
				claim = scheduler.nativeClaims[id]
				claim.deadline = time.Now().Add(-time.Second)
				scheduler.nativeClaims[id] = claim
				if err := scheduler.ReleaseClaim(t.Context(), id, "released"); err != nil {
					t.Fatalf("acknowledged Finish lost on release retry: %v", err)
				}
				if finishedEvents != 1 {
					t.Fatalf("Finish published again: %d events", finishedEvents)
				}
				if cleanupCalls != 1 {
					t.Fatalf("worker cleanup repeated after a lease-release retry: %d", cleanupCalls)
				}
			case "release", "release_finish_lost":
				if operation == "release_finish_lost" {
					claim := scheduler.nativeClaims[id]
					claim.deadline = time.Now().Add(-time.Second)
					scheduler.nativeClaims[id] = claim
					owned := fixtureRecoverExecution(t, scheduler, id, func(state *checkpoint.State) { state.PreparedOutcome = "failed" })
					observedAttempt = owned.CompletionData()
				}
				err := scheduler.ReleaseClaim(t.Context(), id, "released")
				if operation == "release_finish_lost" {
					if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
						t.Fatalf("failed Finish reported settlement: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case "lost":
				claim := scheduler.nativeClaims[id]
				claim.lease = next
				scheduler.nativeClaims[id], scheduler.claims[id] = claim, nativeTrackerLease(next)
				err = scheduler.nativeClaimError(t.Context(), id, lease.FencingToken, &APIError{Status: http.StatusConflict, Code: "stale_fencing_token"})
				if !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
					t.Fatalf("old claim loss = %v", err)
				}
			case "lost_release":
				if err := scheduler.nativeClaimError(t.Context(), id, lease.FencingToken, orchestrator.ErrSchedulingClaimLost); !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
					t.Fatalf("claim cleanup = %v", err)
				}
				if scheduler.nativeClaims[id].lost {
					t.Fatal("delayed cleanup invalidated the successor")
				}
			}
			if scheduler.nativeClaims[id].lease.FencingToken != next.FencingToken || scheduler.claims[id].FencingToken != next.FencingToken {
				t.Fatal("delayed response changed the successor")
			}
		})
	}
}
