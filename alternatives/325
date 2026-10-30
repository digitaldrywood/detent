package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/telemetry"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type controllerFixture struct {
	URL          string
	Token        string
	Organization tracker.OrganizationID
	Project      tracker.ProjectID
	Workflow     config.Workflow
	Policy       policy.Descriptor
	Database     string
}

func TestControllerRestartCancellationWaitsForRejection(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		committed bool
		cancelErr error
	}{
		{name: "canceled replacement exits before admission resumes"},
		{name: "committed handoff wins cancellation", committed: true},
		{name: "committed handoff survives cancellation response failure", committed: true, cancelErr: errors.New("cancellation response unavailable")},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				local, remote := net.Pipe()
				defer local.Close()
				defer remote.Close()
				started := make(chan controllerRestart, 1)
				canceled := make(chan uint64, 1)
				result := make(chan error, 1)
				defer close(result)
				server := runnerpkg.NewSSHPeer(t.Context(), remote, remote, func(_ context.Context, method string, args []json.RawMessage) (any, error) {
					switch method {
					case "restart":
						var request controllerRestart
						if err := json.Unmarshal(args[0], &request); err != nil {
							return nil, err
						}
						started <- request
						return nil, <-result
					case "cancel_restart":
						var id uint64
						if err := json.Unmarshal(args[0], &id); err != nil {
							return nil, err
						}
						canceled <- id
						return nil, test.cancelErr
					}
					return nil, errors.New("unsupported fixture request")
				})
				defer server.Close()
				peer := runnerpkg.NewSSHPeer(t.Context(), local, local, nil)
				defer peer.Close()
				connection := &controllerConnection{peer: peer}
				done := make(chan error, 1)
				go func() { done <- connection.restart(ctx, "verified-controller") }()
				request := <-started
				if request.ID == 0 || request.Binary != "verified-controller" {
					t.Fatalf("invalid restart request: %+v", request)
				}
				cancel()
				if id := <-canceled; id != request.ID {
					t.Fatalf("canceled request %d, want %d", id, request.ID)
				}
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatalf("restart returned before replacement was committed or rejected: %v", err)
				default:
				}
				var wantErr error
				if !test.committed {
					wantErr = context.Canceled
				}
				result <- wantErr
				if err := <-done; !errors.Is(err, wantErr) {
					t.Fatalf("reconciled restart = %v, want %v", err, wantErr)
				}
			})
		})
	}
}

func TestControllerSupervisorCleanExitRestarts(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("POSIX controller supervision")
	}
	for _, test := range []struct {
		name       string
		mode       string
		wantBinary bool
		wantError  bool
	}{
		{name: "unexpected clean exit", mode: "clean", wantBinary: true},
		{name: "applied replacement exits cleanly", mode: "adopt", wantBinary: true},
		{name: "failed controller exits nonzero", mode: "failure", wantError: true},
		{name: "operator shutdown stays stopped", mode: "operator"},
	} {
		t.Run(test.name, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			binary := filepath.Join(root, "current")
			updated := filepath.Join(root, "updated")
			for _, path := range []string{binary, updated} {
				if err := os.Link(executable, path); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("DETENT_TEST_CONTROLLER_EXIT", test.mode)
			t.Setenv("DETENT_TEST_CONTROLLER_UPDATED", updated)
			t.Setenv("GOCOVERDIR", t.TempDir())
			ctx, cancel := context.WithCancel(t.Context())
			shutdown := NewShutdownController()
			restart := NewRestartRequest()
			var target atomic.Pointer[url.URL]
			done := make(chan error, 1)
			go func() {
				done <- runControllerSupervisor(ctx, shutdown, binary, []string{"-test.run=^TestControllerSupervisorExitProcess$"}, io.Discard, os.Stderr, &target, controllerSupervisorConfig{restart: restart})
			}()
			t.Cleanup(func() { cancel(); <-done })
			if test.mode == "operator" {
				testenv.WaitUntil(t, ctx, func() error {
					if target.Load() == nil {
						return errors.New("controller is not ready")
					}
					return nil
				})
				shutdown.RequestDrain()
			}
			err = <-done
			done <- err
			if (err != nil) != test.wantError {
				t.Fatalf("supervisor error = %v, want error %t", err, test.wantError)
			}
			wantBinary := ""
			if test.wantBinary {
				wantBinary = binary
				if test.mode == "adopt" {
					wantBinary = updated
				}
			}
			if restart.Binary() != wantBinary {
				t.Fatalf("restart binary = %q, want %q", restart.Binary(), wantBinary)
			}
		})
	}
}

func TestControllerSupervisorExitProcess(t *testing.T) {
	mode := os.Getenv("DETENT_TEST_CONTROLLER_EXIT")
	if mode == "" || os.Getenv(controllerPipeEnvironment) == "" {
		return
	}
	restart := ControllerRestartRequest()
	if restart == nil || restart.control == nil {
		t.Fatal("missing controller connection")
	}
	defer restart.control.close()
	shutdown := NewShutdownController()
	defer shutdown.activate()()
	if err := restart.control.ready(t.Context(), shutdown, "127.0.0.1:12345"); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(os.Args[0]) == "updated" || mode == "clean" {
		return
	}
	if mode == "failure" {
		os.Exit(17)
	}
	if mode == "adopt" {
		restart.reserve = func() func() { return func() {} }
		accepted, err := requestRuntimeUpdateRestart(t.Context(), shutdown, restart, os.Getenv("DETENT_TEST_CONTROLLER_UPDATED"), nil)
		if err != nil || !accepted {
			t.Fatalf("adopt replacement: accepted=%t error=%v", accepted, err)
		}
	}
	select {
	case <-shutdown.Requests():
	case <-t.Context().Done():
		t.Fatal("controller was not drained")
	}
}

func TestControllerTerminalViewKeepsRetiredAttemptsVisible(t *testing.T) {
	t.Parallel()
	view := &controllerView{snapshots: hub.New[telemetry.Snapshot]()}
	old, current := &controllerProcess{}, &controllerProcess{}
	owners := map[*controllerProcess]*telemetry.Snapshot{
		old:     {Running: []telemetry.Running{{Issue: telemetry.Issue{ID: "old-attempt"}}}},
		current: {Running: []telemetry.Running{{Issue: telemetry.Issue{ID: "new-attempt"}}}},
	}
	base := telemetry.Snapshot{Counts: telemetry.Counts{Queue: 3}, Instance: telemetry.Instance{Name: "new-controller"}}
	for _, test := range []struct {
		name            string
		retiredFinished bool
		stopping        bool
		wantRunning     int
	}{
		{name: "new controller serves while old attempt runs", wantRunning: 2},
		{name: "operator drain includes both owners", stopping: true, wantRunning: 2},
		{name: "retired owner exits after completion", retiredFinished: true, wantRunning: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.retiredFinished {
				delete(owners, old)
			}
			if err := view.publish(base, owners, test.stopping); err != nil {
				t.Fatal(err)
			}
			got, _ := view.snapshots.Latest()
			if len(got.Running) != test.wantRunning || got.Counts.Running != test.wantRunning || got.Instance.Name != "new-controller" || got.Counts.Queue != 3 {
				t.Fatalf("terminal lost current ownership: %+v", got)
			}
			if test.stopping && (!got.Shutdown.Draining || got.Shutdown.SessionsRemaining != 2) {
				t.Fatalf("operator drain lost retired attempts: %+v", got.Shutdown)
			}
		})
	}
}

func TestControllerProxyPreservesClientAuthority(t *testing.T) {
	t.Parallel()
	settings := controllerSettings{Key: "private-fixture-key"}
	connection := &controllerConnection{settings: settings}
	child := httptest.NewServer(connection.preserveRequestOrigin(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get(controllerProxyKeyHeader) != "" || request.Header.Get(controllerRemoteAddressHeader) != "" {
			t.Error("internal proxy credentials reached the application handler")
		}
		fmt.Fprintf(w, "%s/%s", request.RemoteAddr, request.Host)
	})))
	defer child.Close()
	address, err := url.Parse(child.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	var target atomic.Pointer[url.URL]
	proxy := controllerProxy(settings, &target, transport)
	for _, test := range []struct {
		name   string
		ready  bool
		key    string
		remote string
	}{
		{name: "starting"},
		{name: "external client keeps external authority", ready: true, remote: "203.0.113.5:10000"},
		{name: "spoofed client metadata is overwritten", ready: true, key: settings.Key, remote: "203.0.113.6:10001"},
		{name: "local client keeps local authority", ready: true, remote: "127.0.0.1:10002"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.ready {
				target.Store(address)
			}
			request := httptest.NewRequest(http.MethodGet, "http://runner.example.test/health", nil)
			request.RemoteAddr = test.remote
			request.Header.Set(controllerProxyKeyHeader, test.key)
			request.Header.Set(controllerRemoteAddressHeader, "127.0.0.1:1")
			recorder := httptest.NewRecorder()
			proxy.ServeHTTP(recorder, request)
			if !test.ready {
				if recorder.Code != http.StatusServiceUnavailable {
					t.Fatalf("startup status = %d", recorder.Code)
				}
			} else if recorder.Code != http.StatusOK || recorder.Body.String() != test.remote+"/runner.example.test" {
				t.Fatalf("proxy changed client authority: %d %q", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestControllerAdoptionPreservesWorkerAndClaimsOnNewBuild(t *testing.T) {
	if testing.Short() {
		t.Skip("controller and execution process integration")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("POSIX controller handoff")
	}
	var fixture controllerFixture
	var hubServer *httptest.Server
	_, native, _, _, create, _ := localWorkerHubFixture(t, func(handler http.Handler, organization tracker.OrganizationID, project tracker.ProjectID, token string, workflow config.Workflow) {
		hubServer = httptest.NewServer(handler)
		fixture = controllerFixture{URL: hubServer.URL, Token: token, Organization: organization, Project: project, Workflow: workflow, Policy: workflow.Config.Policy, Database: filepath.Join(t.TempDir(), "runtime.db")}
	})
	defer hubServer.Close()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, t.TempDir())
	}
	bootstrap, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("DETENT_CONTROLLER_FIXTURE", string(bootstrap))
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binaries := make(map[string]string)
	for _, version := range []string{"old", "updated", "failed", "stalled"} {
		path := filepath.Join(t.TempDir(), version)
		if err := os.Link(executable, path); err != nil {
			t.Fatal(err)
		}
		binaries[version] = path
	}
	first := create("Compile first source")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	controller := NewShutdownController()
	var target atomic.Pointer[url.URL]
	done := make(chan error, 1)
	var calls sync.WaitGroup
	type stalledStartup struct {
		process *controllerProcess
		events  chan<- controllerEvent
		expire  context.CancelFunc
	}
	stalled := make(chan stalledStartup, 1)
	var launched *controllerProcess
	var launchedEvents chan<- controllerEvent
	var launchedBinary string
	starter := func(ctx context.Context, binary string, args []string, events chan<- controllerEvent, stdout, stderr io.Writer) (*controllerProcess, error) {
		t.Setenv("GOCOVERDIR", t.TempDir())
		process, err := startControllerProcess(ctx, binary, args, events, stdout, stderr)
		launched, launchedEvents, launchedBinary = process, events, binary
		return process, err
	}
	startupContext := func(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
		if limit != time.Minute {
			t.Errorf("controller startup bound = %s, want one minute", limit)
		}
		if launchedBinary == binaries["stalled"] {
			ctx, cancel := context.WithCancelCause(ctx)
			stalled <- stalledStartup{process: launched, events: launchedEvents, expire: func() { cancel(context.DeadlineExceeded) }}
			return ctx, func() { cancel(context.Canceled) }
		}
		return context.WithTimeout(ctx, limit)
	}
	go func() {
		done <- runControllerSupervisor(ctx, controller, binaries["old"], []string{"-test.run=^TestControllerAdoptionProcess$"}, io.Discard, os.Stderr, &target, controllerSupervisorConfig{start: starter, startupContext: startupContext})
	}()
	defer func() { cancel(); <-done; calls.Wait() }()
	client := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	defer client.CloseIdleConnections()
	callResult := func(address, path string) (string, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, address+path, nil)
		if err != nil {
			return "", err
		}
		response, err := client.Do(request)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return "", err
		}
		if response.StatusCode != http.StatusOK {
			return "", fmt.Errorf("controller %s: HTTP %d: %s", path, response.StatusCode, data)
		}
		return string(data), nil
	}
	call := func(address, path string) string {
		t.Helper()
		result, err := callResult(address, path)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	testenv.WaitUntil(t, ctx, func() error {
		if target.Load() == nil {
			return errors.New("initial controller not ready")
		}
		return nil
	})
	oldAddress := target.Load().String()
	oldProcess := launched
	if got := call(oldAddress, "/claim"); got != first.ID {
		t.Fatalf("old controller claim = %q, want %q", got, first.ID)
	}
	initial, err := native.RuntimeEvidence(ctx, tracker.NativeWorkItemID(first.ID), "")
	if err != nil || initial.Attempt == nil || initial.Attempt.Runtime == nil || initial.Attempt.Runtime.WorkerLaunch == nil {
		t.Fatalf("old worker identity: %+v %v", initial, err)
	}
	launch := initial.Attempt.Runtime.WorkerLaunch
	identity := procgroup.Identity{PID: launch.PID, GroupID: launch.GroupID, StartedAt: launch.ProcessStart}
	if got := call(oldAddress, "/adopt?binary="+url.QueryEscape(binaries["failed"])); got != "declined" {
		t.Fatalf("failed replacement = %q", got)
	}
	if target.Load().String() != oldAddress || call(oldAddress, "/health") != "1.0.0" || call(oldAddress, "/admission") != "claiming" {
		t.Fatal("failed startup changed the serving controller or left claims paused")
	}
	for _, canceled := range []bool{false, true} {
		stalledResult := make(chan error, 1)
		calls.Go(func() {
			got, err := callResult(oldAddress, "/adopt?binary="+url.QueryEscape(binaries["stalled"]))
			if err == nil && got != "declined" {
				err = fmt.Errorf("stalled replacement = %q", got)
			}
			stalledResult <- err
		})
		startup := <-stalled
		identityStalled, err := procgroup.Inspect(startup.process.cmd)
		if err != nil {
			t.Fatal(err)
		}
		if alive, err := procgroup.Alive(identityStalled); err != nil || !alive {
			t.Fatalf("replacement exited instead of stalling before readiness: %t %v", alive, err)
		}
		if canceled {
			id, err := strconv.ParseUint(call(oldAddress, "/adoption-id"), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
			reply := make(chan error, 1)
			startup.events <- controllerEvent{process: oldProcess, method: "cancel_restart", restartID: id, reply: reply}
			if err := <-reply; err != nil {
				t.Fatal(err)
			}
		} else {
			startup.expire()
		}
		lateReady := make(chan error, 1)
		startup.events <- controllerEvent{process: startup.process, method: "ready", value: "127.0.0.1:1", reply: lateReady}
		if err := <-lateReady; err == nil {
			t.Fatal("stalled replacement took over after its startup deadline")
		}
		if err := <-stalledResult; err != nil {
			t.Fatal(err)
		}
		if alive, err := procgroup.Alive(identityStalled); err != nil || alive {
			t.Fatalf("old admission resumed before stalled replacement exited: %t %v", alive, err)
		}
		if target.Load().String() != oldAddress || call(oldAddress, "/health") != "1.0.0" || call(oldAddress, "/admission") != "claiming" {
			t.Fatal("stalled startup changed the serving controller or left claims paused")
		}
		if alive, err := procgroup.Alive(identity); err != nil || !alive {
			t.Fatalf("rejecting stalled replacement killed active execution worker: %t %v", alive, err)
		}
	}
	id, err := strconv.ParseUint(call(oldAddress, "/adoption-id"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	reply := make(chan error, 1)
	launchedEvents <- controllerEvent{process: oldProcess, method: "cancel_restart", restartID: id + 1, reply: reply}
	if err := <-reply; err != nil {
		t.Fatal(err)
	}
	if got := call(oldAddress, "/adopt?binary="+url.QueryEscape(binaries["stalled"])); got != "declined" {
		t.Fatal("cancellation delivered before launch did not reject adoption")
	}
	select {
	case <-stalled:
		t.Fatal("supervisor launched a canceled adoption")
	default:
	}
	second := create("Render second source")
	if got := call(oldAddress, "/adopt?binary="+url.QueryEscape(binaries["updated"])); got != "adopted" {
		t.Fatalf("busy replacement = %q", got)
	}
	newAddress := target.Load().String()
	if newAddress == oldAddress || call(newAddress, "/health") != "1.0.1" {
		t.Fatal("health did not adopt the applied build")
	}
	if got := call(newAddress, "/claim"); got != second.ID {
		t.Fatalf("new controller failed to claim while first attempt runs: %q", got)
	}
	if call(oldAddress, "/admission") != "paused" {
		t.Fatal("retired controller still accepts new claims")
	}
	if alive, err := procgroup.Alive(identity); err != nil || !alive {
		t.Fatalf("replacement killed old execution worker: %t %v", alive, err)
	}
	if got := call(oldAddress, "/renew"); got != "renewed" {
		t.Fatalf("old attempt lease lost during handoff: %q", got)
	}
	if got := call(oldAddress, "/finish"); got != "completed" {
		t.Fatalf("old worker failed to complete over its retained IPC: %q", got)
	}
	if got := call(newAddress, "/finish"); got != "completed" {
		t.Fatalf("new worker failed to complete: %q", got)
	}
	controller.RequestDrain()
}

func TestControllerAdoptionProcess(t *testing.T) {
	raw := os.Getenv("DETENT_CONTROLLER_FIXTURE")
	if raw == "" || os.Getenv(controllerPipeEnvironment) == "" {
		return
	}
	if filepath.Base(os.Args[0]) == "failed" {
		os.Exit(17)
	}
	version := "1.0.0"
	if filepath.Base(os.Args[0]) == "updated" {
		version = "1.0.1"
	}
	var fixture controllerFixture
	if err := json.Unmarshal([]byte(raw), &fixture); err != nil {
		t.Fatal(err)
	}
	fixture.Workflow.Config.Policy = fixture.Policy
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	restart := ControllerRestartRequest()
	if restart == nil || restart.control == nil {
		t.Fatal("missing controller restart connection")
	}
	defer restart.control.close()
	if filepath.Base(os.Args[0]) == "stalled" {
		<-restart.control.peer.Context().Done()
		return
	}
	controller := NewShutdownController()
	defer controller.activate()()
	restart.control.controller.Store(controller)
	client, err := hubclient.New(hubclient.Config{URL: fixture.URL, TokenSource: func() string { return fixture.Token }, HTTPClient: &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(fixture.Organization, fixture.Project)
	if err != nil {
		t.Fatal(err)
	}
	source, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{OrganizationID: fixture.Organization, NativeProjects: map[string]tracker.ProjectID{"local": fixture.Project}, Machine: hubclient.Machine{ID: "fixture-machine", Hostname: "fixture-host", Capacity: 2, Version: version}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 2}}}, []scheduler.ProjectCandidate{{ID: "local"}})
	if err != nil {
		t.Fatal(err)
	}
	source.SetClaimAdmission(gate.ReserveDispatch)
	restart.reserve = func() func() {
		release := gate.PauseDispatch()
		resume := source.PauseController()
		return func() { resume(); release() }
	}
	backend, err := store.Open(ctx, store.Config{Path: fixture.Database})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	if _, err := startWorkerGeneration(ctx, backend, version, time.Now()); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	defer exitWorkerGeneration(backend, logger, time.Now())
	if err := reapWorkerProcesses(ctx, backend, logger, "startup", procgroup.DefaultTerminationGrace, time.Now, nil); err != nil {
		t.Fatal(err)
	}
	artifacts, publish, _ := localWorkerArtifacts(t)
	publish("1.0.0")
	prepared, err := artifacts.Acquire(ctx, workerProtocolPreflight)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Release()
	fixture.Workflow.Config.Workspace.Root = t.TempDir()
	built, err := buildRunner(fixture.Workflow, "local", t.TempDir(), globalconfig.Memory{}, gobudget.Budget{}, nil, logger, serviceapi.Connection{})
	if err != nil {
		t.Fatal(err)
	}
	run := built.(*sshRunner)
	run.artifacts = artifacts
	var issue connector.Issue
	var attempt int64
	var mu sync.Mutex
	var workers sync.WaitGroup
	finalized := make(chan struct{})
	ready, finish, completed := make(chan struct{}), make(chan struct{}), make(chan runnerpkg.Completion, 1)
	var finishOnce sync.Once
	defer func() { finishOnce.Do(func() { close(finish) }); cancel(); workers.Wait() }()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/adoption-id" {
			fmt.Fprint(w, restart.control.nextRestart.Load())
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fail := func(err error) { http.Error(w, err.Error(), http.StatusInternalServerError) }
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, version)
		case "/admission":
			if release, admitted := gate.ReserveDispatch(); admitted {
				release()
				fmt.Fprint(w, "claiming")
			} else {
				fmt.Fprint(w, "paused")
			}
		case "/adopt":
			accepted, err := requestRuntimeUpdateRestart(ctx, controller, restart, r.URL.Query().Get("binary"), nil)
			if err == nil && accepted {
				fmt.Fprint(w, "adopted")
			} else {
				fmt.Fprint(w, "declined")
			}
		case "/claim":
			issues, err := source.FetchCandidateIssues(ctx, orchestrator.SchedulingRequest{ProjectID: "local", Policy: fixture.Workflow.Config.Policy, WorkflowStates: []string{"Todo"}, CandidateLimit: 1})
			if err != nil || len(issues) != 1 {
				fail(fmt.Errorf("claim candidates %d: %w", len(issues), err))
				return
			}
			claim, err := source.AdoptClaim(ctx, issues[0], time.Now())
			if err != nil {
				fail(err)
				return
			}
			issue = claim.Issue
			attempt, err = backend.StartWorkAttempt(ctx, store.WorkAttemptStart{ProjectID: "local", IssueID: issue.ID, WorkerType: "agent", StartedAt: time.Now(), LeaseExpiresAt: time.Now().Add(time.Minute)})
			if err != nil {
				fail(err)
				return
			}
			request := runnerpkg.RunRequest{ProjectID: "local", Issue: issue, Policy: fixture.Workflow.Config.Policy, Execution: source.RunExecution(issue.ID), Mode: runnerpkg.RunModeImplement, WorkAttemptID: attempt, Generation: 1, DeferExecutionFinish: true, ProgressProbe: func(ctx context.Context) (string, error) {
				close(ready)
				select {
				case <-finish:
					return "finish", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}}
			workers.Go(func() {
				result, err := run.Run(ctx, request)
				completed <- runnerpkg.Completion{Result: result, Err: err}
			})
			select {
			case <-ready:
				fmt.Fprint(w, issue.ID)
			case result := <-completed:
				fail(fmt.Errorf("worker startup: %w", result.Err))
			}
		case "/renew":
			if _, err := source.RenewClaim(ctx, issue.ID, time.Now()); err != nil {
				fail(err)
				return
			}
			fmt.Fprint(w, "renewed")
		case "/finish":
			finishOnce.Do(func() { close(finish) })
			result := <-completed
			if result.Err != nil || result.Result.FinalState != runnerpkg.FinalStateCompleted {
				fail(fmt.Errorf("worker result: %w", result.Err))
				return
			}
			connection, _ := hubclient.NewNativeConnector(native)
			if err := connection.UpdateIssueState(ctx, issue.ID, "Done"); err != nil {
				fail(err)
				return
			}
			if err := source.ReleaseClaim(ctx, issue.ID, "completed"); err != nil {
				fail(err)
				return
			}
			if err := backend.CompleteWorkAttempt(ctx, store.WorkAttemptCompletion{AttemptID: attempt, TerminalState: store.WorkAttemptTerminalSuccess, CompletedAt: time.Now()}); err != nil {
				fail(err)
				return
			}
			fmt.Fprint(w, "completed")
			close(finalized)
		default:
			http.NotFound(w, r)
		}
	})}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), defaultShutdownHardTimeout)
		defer stop()
		_ = server.Shutdown(shutdownCtx)
		_ = server.Close()
		<-serverDone
	}()
	if err := restart.control.ready(ctx, controller, listener.Addr().String()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-restart.control.peer.Context().Done():
	case <-controller.Requests():
		mu.Lock()
		busy := issue.ID != ""
		mu.Unlock()
		if busy {
			select {
			case <-finalized:
			case <-restart.control.peer.Context().Done():
			}
		}
	}
}
