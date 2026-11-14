package runnerconfiguration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/dispatch"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type runnerRuntime struct {
	mu           sync.Mutex
	paused       bool
	draining     bool
	applied      []string
	observations int
}

func (r *runnerRuntime) apply(desired runnerauth.ProjectConfiguration, requestID string) runnerauth.ProjectConfiguration {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.observations++
	r.paused, r.draining = desired.Paused, desired.Draining
	if requestID != "" {
		r.applied = append(r.applied, requestID)
	}
	view := desired
	view.Authority = "hub_configuration"
	view.RuntimeRegistered = true
	view.Paused, view.Draining = r.paused, r.draining
	view.Pending = false
	view.Applied, view.Saved = true, true
	view.ObservedAt = time.Now().UTC()
	return view
}

func (r *runnerRuntime) state() (bool, []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.draining, append([]string(nil), r.applied...)
}

type configuredRunner struct {
	runtime *runnerRuntime
	tick    func(*testing.T) error
	flush   func(*testing.T) error
	read    func(*testing.T) runnerauth.ProjectConfiguration
	queue   func(*testing.T, string, string)
	settled func(*testing.T, string, bool)
	rename  func(*testing.T, string)
	restart func(*testing.T)
}

func startConfiguredRunner(t *testing.T, hub func(http.Handler) http.Handler) configuredRunner {
	t.Helper()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	server := httptest.NewServer(hub(f.Hub.Handler()))
	t.Cleanup(server.Close)
	organization := f.Organization()
	path := filepath.Join(t.TempDir(), "private", "runner.json")
	file, err := runnerauth.Initialize(path, server.URL)
	if err != nil {
		t.Fatal(err)
	}
	enrollmentResponse := f.Hub.Request(t, http.MethodPost, hubfixture.OrganizationBase(organization)+"/runner-enrollments", hubfixture.AdminToken, runnerauth.EnrollmentRequest{Scope: "projects", Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{f.Project.ID}, Operations: []string{runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events}, TTLSeconds: 60})
	hubfixture.RequireStatus(t, enrollmentResponse, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	hubfixture.Decode(t, enrollmentResponse, &enrollment)
	machine := dispatch.Machine{ID: file.Identity.MachineID, Hostname: "example-host", DisplayName: "Example runner", Capacity: 1, Version: "test", BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}}
	identity, err := dispatch.EnrollRunner(t.Context(), path, organization, enrollment.Token, machine)
	if err != nil {
		t.Fatal(err)
	}
	var clockMu sync.Mutex
	clock := time.Now().UTC()
	interval := 30 * time.Second
	runtime := &runnerRuntime{}
	projectID := string(f.Project.ID)
	var client *dispatch.Client
	start := func(t *testing.T) *dispatch.Scheduler {
		t.Helper()
		transport := http.DefaultTransport.(*http.Transport).Clone()
		t.Cleanup(transport.CloseIdleConnections)
		started, err := dispatch.New(dispatch.Config{URL: server.URL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
		client = started
		if err != nil {
			t.Fatal(err)
		}
		scheduler, err := dispatch.NewScheduler(client, dispatch.SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"local": f.Project.ID}, Machine: machine, HeartbeatInterval: interval, LeaseTTL: 10 * time.Minute, Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return clock
		}})
		if err != nil {
			t.Fatal(err)
		}
		scheduler.SetProjectConfigurationOwner(func(_ context.Context, id string, request *runnerauth.ProjectConfigurationRequest) runnerauth.ProjectConfiguration {
			view := project.MissingConfigurationOwner(id)
			view.Authority = "hub_configuration"
			desired := client.CurrentProjectConfiguration(tracker.ProjectID(id))
			requestID := ""
			if request != nil {
				if request.ProjectID != id || request.Desired == nil {
					return view
				}
				desired, requestID = request.Desired, request.RequestID
			}
			if desired == nil || id != projectID {
				return view
			}
			view = runtime.apply(*desired, requestID)
			view.ProjectID = id
			if request != nil {
				view.RequestID = request.RequestID
			}
			return view
		})
		return scheduler
	}
	scheduler := start(t)
	operator := hubfixture.MCP(t, f.Hub.Handler(), hubfixture.MCPPath(organization), hubfixture.AdminToken)
	r := configuredRunner{runtime: runtime}
	r.restart = func(t *testing.T) { scheduler = start(t) }
	r.flush = func(t *testing.T) error {
		t.Helper()
		_, err := client.FlushHostMetrics(t.Context(), nil)
		return err
	}
	r.tick = func(t *testing.T) error {
		t.Helper()
		clockMu.Lock()
		clock = clock.Add(interval)
		clockMu.Unlock()
		return scheduler.Heartbeat(t.Context())
	}
	r.read = func(t *testing.T) runnerauth.ProjectConfiguration {
		t.Helper()
		var view runnerauth.ProjectConfiguration
		if err := json.Unmarshal(hubfixture.MCPData(t, operator("tools/call", operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: projectID, RunnerID: identity.RunnerID}), false), &view); err != nil {
			t.Fatal(err)
		}
		if view.RunnerID != identity.RunnerID {
			t.Fatalf("runner configuration was not registered: %+v", view)
		}
		return view
	}
	r.queue = func(t *testing.T, name, requestID string) {
		t.Helper()
		current := r.read(t)
		if current.EffectivePolicy == nil || current.Pending {
			t.Fatalf("%s precondition=%+v", name, current)
		}
		var queued runnerauth.ProjectConfiguration
		if err := json.Unmarshal(hubfixture.MCPData(t, operator("tools/call", name, operatortool.LocalProjectArguments{ProjectID: projectID, RunnerID: identity.RunnerID, ExpectedRunnerRevision: current.RunnerRevision, RequestID: requestID, ExpectedConfigRevision: current.ConfigRevision, ExpectedPolicyID: current.EffectivePolicy.ID}), false), &queued); err != nil {
			t.Fatal(err)
		}
		if !queued.Pending {
			t.Fatalf("%s was not queued: %+v", name, queued)
		}
	}
	r.settled = func(t *testing.T, operation string, draining bool) {
		t.Helper()
		view := r.read(t)
		if view.Pending || view.Draining != draining || view.LastOperation == nil || view.LastOperation.Operation != operation || !view.LastOperation.Applied {
			t.Fatalf("%s readback=%+v last=%+v", operation, view, view.LastOperation)
		}
	}
	r.rename = func(t *testing.T, name string) {
		t.Helper()
		routing := f.Hub.Request(t, http.MethodPut, hubfixture.OrganizationBase(organization)+"/runners/"+identity.RunnerID+"/routing", hubfixture.AdminToken, map[string]any{"expected_revision": r.read(t).RunnerRevision, "display_name": name, "state": "active", "capacity_limit": 1, "project_ids": []tracker.ProjectID{f.Project.ID}})
		hubfixture.RequireStatus(t, routing, http.StatusOK)
	}
	return r
}

func (r configuredRunner) mustTick(t *testing.T) {
	t.Helper()
	if err := r.tick(t); err != nil {
		t.Fatalf("runner heartbeat: %v", err)
	}
}

func TestEnrolledRunnerReceivesSuccessiveConfigurationCommands(t *testing.T) {
	r := startConfiguredRunner(t, func(hub http.Handler) http.Handler { return hub })
	r.mustTick(t)
	r.mustTick(t)
	r.read(t)

	r.queue(t, "drain_local_project", "drain")
	r.mustTick(t)
	if draining, applied := r.runtime.state(); !draining || len(applied) != 1 || applied[0] != "drain" {
		t.Fatalf("drain delivery draining=%t applied=%v", draining, applied)
	}
	r.settled(t, "drain_local_project", true)
	r.mustTick(t)

	r.rename(t, "Renamed runner")
	r.mustTick(t)

	r.queue(t, "resume_local_project", "resume")
	r.mustTick(t)
	if draining, applied := r.runtime.state(); draining || len(applied) != 2 || applied[1] != "resume" {
		t.Fatalf("resume delivery draining=%t applied=%v", draining, applied)
	}
	r.settled(t, "resume_local_project", false)
	r.mustTick(t)
	if _, applied := r.runtime.state(); len(applied) != 2 {
		t.Fatalf("settled command was delivered again: %v", applied)
	}
}

func TestTrappedRunnerRecoversAfterRestartOnUnboundHub(t *testing.T) {
	var bound atomic.Bool
	bound.Store(true)
	r := startConfiguredRunner(t, func(hub http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if !bound.Load() || req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/heartbeat") {
				hub.ServeHTTP(w, req)
				return
			}
			recorder := httptest.NewRecorder()
			hub.ServeHTTP(recorder, req)
			var snapshot runnerauth.RoutingSnapshot
			if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &snapshot) != nil || snapshot.ProjectConfiguration == nil {
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
			snapshot.ProjectConfiguration.RunnerRevision = snapshot.Revision
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(snapshot)
		})
	})
	r.mustTick(t)
	r.mustTick(t)
	r.queue(t, "drain_local_project", "drain")
	if err := r.tick(t); err == nil {
		t.Fatal("a report bound to the previous runner revision was accepted; the trapped runner precondition did not hold")
	}

	if _, applied := r.runtime.state(); len(applied) != 0 {
		t.Fatalf("trapped runner applied a command it could not acknowledge: %v", applied)
	}
	bound.Store(false)
	r.restart(t)
	r.mustTick(t)
	if draining, applied := r.runtime.state(); !draining || len(applied) != 1 || applied[0] != "drain" {
		t.Fatalf("restarted runner drain delivery draining=%t applied=%v", draining, applied)
	}
	r.settled(t, "drain_local_project", true)
	r.mustTick(t)
	if _, applied := r.runtime.state(); len(applied) != 1 {
		t.Fatalf("settled command was delivered again after restart: %v", applied)
	}
}
