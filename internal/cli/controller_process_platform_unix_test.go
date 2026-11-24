//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestControllerSupervisorCleanExitRestarts(t *testing.T) {
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

func TestControllerAdoptionPreservesWorkerAndClaimsOnNewBuild(t *testing.T) {
	var fixture controllerFixture
	var hubServer *httptest.Server
	_, native, _, _, create, _ := localWorkerHubFixture(t, func(handler http.Handler, organization tracker.OrganizationID, project tracker.ProjectID, token string, workflow config.Workflow) {
		hubServer = httptest.NewServer(handler)
		fixture = controllerFixture{URL: hubServer.URL, Token: token, Organization: organization, Project: project, Workflow: workflow, Policy: workflow.Config.Policy}
	})
	defer hubServer.Close()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, testenv.TempDir(t))
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
	initial, err := clihangRuntimeEvidence(ctx, native, tracker.NativeWorkItemID(first.ID))
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
	if got := call(oldAddress, "/candidate"); got != second.ID {
		t.Fatalf("old controller pending claim = %q, want %q", got, second.ID)
	}
	if got := call(oldAddress, "/adopt?binary="+url.QueryEscape(binaries["updated"])); got != "adopted" {
		t.Fatalf("busy replacement = %q", got)
	}
	newAddress := target.Load().String()
	if newAddress == oldAddress || call(newAddress, "/health") != "1.0.1" {
		t.Fatal("health did not adopt the applied build")
	}
	if got := call(oldAddress, "/release-candidate"); got != "released" {
		t.Fatalf("retired controller stranded the pending claim: %q", got)
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
	native.projectPages()
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
