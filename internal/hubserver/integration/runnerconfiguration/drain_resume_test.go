package runnerconfiguration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
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
	mu       sync.Mutex
	paused   bool
	draining bool
	applied  []string
}

func (r *runnerRuntime) apply(desired runnerauth.ProjectConfiguration, requestID string) runnerauth.ProjectConfiguration {
	r.mu.Lock()
	defer r.mu.Unlock()
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

func TestEnrolledRunnerReceivesSuccessiveConfigurationCommands(t *testing.T) {
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	server := httptest.NewServer(f.Hub.Handler())
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
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	client, err := dispatch.New(dispatch.Config{URL: server.URL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	var clockMu sync.Mutex
	clock := time.Now().UTC()
	interval := 30 * time.Second
	scheduler, err := dispatch.NewScheduler(client, dispatch.SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"local": f.Project.ID}, Machine: machine, HeartbeatInterval: interval, LeaseTTL: 10 * time.Minute, Now: func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}})
	if err != nil {
		t.Fatal(err)
	}
	runtime := &runnerRuntime{}
	projectID := string(f.Project.ID)
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
	tick := func(t *testing.T) {
		t.Helper()
		clockMu.Lock()
		clock = clock.Add(interval)
		clockMu.Unlock()
		if err := scheduler.Heartbeat(t.Context()); err != nil {
			t.Fatalf("runner heartbeat: %v", err)
		}
	}
	operator := hubfixture.MCP(t, f.Hub.Handler(), hubfixture.MCPPath(organization), hubfixture.AdminToken)
	read := func(t *testing.T) runnerauth.ProjectConfiguration {
		t.Helper()
		var view runnerauth.ProjectConfiguration
		if err := json.Unmarshal(hubfixture.MCPData(t, operator("tools/call", operatortool.LocalProjectConfiguration, operatortool.LocalProjectArguments{ProjectID: projectID, RunnerID: identity.RunnerID}), false), &view); err != nil {
			t.Fatal(err)
		}
		return view
	}
	queue := func(t *testing.T, name, requestID string) {
		t.Helper()
		current := read(t)
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
	settled := func(t *testing.T, operation string, draining bool) {
		t.Helper()
		view := read(t)
		if view.Pending || view.Draining != draining || view.LastOperation == nil || view.LastOperation.Operation != operation || !view.LastOperation.Applied {
			t.Fatalf("%s readback=%+v last=%+v", operation, view, view.LastOperation)
		}
	}

	tick(t)
	tick(t)
	if read(t).RunnerID != identity.RunnerID {
		t.Fatal("runner configuration was not registered")
	}

	queue(t, "drain_local_project", "drain")
	tick(t)
	if draining, applied := runtime.state(); !draining || len(applied) != 1 || applied[0] != "drain" {
		t.Fatalf("drain delivery draining=%t applied=%v", draining, applied)
	}
	settled(t, "drain_local_project", true)
	tick(t)

	routing := f.Hub.Request(t, http.MethodPut, hubfixture.OrganizationBase(organization)+"/runners/"+identity.RunnerID+"/routing", hubfixture.AdminToken, map[string]any{"expected_revision": read(t).RunnerRevision, "display_name": "Renamed runner", "state": "active", "capacity_limit": 1, "project_ids": []tracker.ProjectID{f.Project.ID}})
	hubfixture.RequireStatus(t, routing, http.StatusOK)
	tick(t)

	queue(t, "resume_local_project", "resume")
	tick(t)
	if draining, applied := runtime.state(); draining || len(applied) != 2 || applied[1] != "resume" {
		t.Fatalf("resume delivery draining=%t applied=%v", draining, applied)
	}
	settled(t, "resume_local_project", false)
	tick(t)
	if _, applied := runtime.state(); len(applied) != 2 {
		t.Fatalf("settled command was delivered again: %v", applied)
	}
}
