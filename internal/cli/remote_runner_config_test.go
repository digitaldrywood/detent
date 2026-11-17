package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRemoteRunnerConfigLiveReload(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	identityPath := filepath.Join(root, "identity.json")
	file, err := runnerauth.Initialize(identityPath, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	file.Identity.OrganizationID = "org_example"
	file.Identity.ProjectIDs = []tracker.ProjectID{"prj_example"}
	file.Identity.ExpiresAt = time.Now().Add(48 * time.Hour)
	if err := runnerauth.Save(identityPath, file); err != nil {
		t.Fatal(err)
	}
	cfg, err := globalconfig.DefaultAt(filepath.Join(root, "global.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Global.MaxConcurrentAgents = 2
	cfg.Client = globalconfig.HubClient{URL: file.HubURL, IdentityFile: identityPath, OrganizationID: "org_example", Capacity: 2}
	if err := globalconfig.Write(cfg.Path, cfg); err != nil {
		t.Fatal(err)
	}
	cfg, err = readRunnerConfiguration(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	state := newGlobalConfigState(cfg)
	gate, err := buildGlobalDispatchPools(cfg)
	if err != nil {
		t.Fatal(err)
	}
	level := &slog.LevelVar{}
	manager := &globalReloadManager{}
	runtime := &remoteRunnerConfig{state: state, gate: gate, logLevel: level, manager: manager, registry: project.NewRegistry()}
	ctx, cancel := context.WithCancel(t.Context())
	done := startGlobalConfigWatcher(ctx, cfg, manager, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil, nil, func(r *globalConfigReloader) { r.applyConfiguration = runtime.applyFile })
	if done == nil {
		cancel()
		t.Fatal("reload owner did not start")
	}
	defer func() { cancel(); waitGlobalConfigWatcher(done) }()
	owner := runnerCapacityOwner(cfg, state.get)
	now := time.Now()
	reported := 0
	transport := readinessRoundTripper(func(req *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		response.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Path == "/api/v2/capabilities":
			_ = json.NewEncoder(response).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeRunnerCapacityCapability}})
		case strings.HasSuffix(req.URL.Path, "/heartbeat"):
			var body struct {
				Capacity int `json:"capacity"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				return nil, err
			}
			reported = body.Capacity
			_ = json.NewEncoder(response).Encode(runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Example", State: "active", CapacityLimit: 20, ProjectIDs: file.Identity.ProjectIDs}.Normalized()})
		case strings.HasSuffix(req.URL.Path, "/projects/prj_example"):
			_ = json.NewEncoder(response).Encode(tracker.NativeProject{Profile: "native"})
		default:
			return nil, fmt.Errorf("unexpected Hub request: %s", req.URL.Path)
		}
		return response.Result(), nil
	})
	client, err := hubclient.New(hubclient.Config{URL: file.HubURL, IdentityFile: identityPath, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	source, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{
		OrganizationID: "org_example", NativeProjects: map[string]tracker.ProjectID{"routed": "prj_example"},
		Machine:           hubclient.Machine{ID: file.Identity.MachineID, Hostname: "example", Version: "test", Capacity: 2},
		HeartbeatInterval: time.Second, LeaseTTL: time.Minute, Now: func() time.Time { return now }, CapacityConfiguration: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCapacity := func(want int) {
		t.Helper()
		testenv.WaitUntil(t, ctx, func() error {
			report := owner(ctx, nil)
			if report == nil || report.RuntimeLimit != want || report.Constraint != "" {
				return fmt.Errorf("capacity not applied: %+v", report)
			}
			return nil
		})
		if snapshots := gate.PoolSnapshots(); len(snapshots) != 1 || snapshots[0].Capacity != want {
			t.Fatalf("dispatch pools = %+v", snapshots)
		}
		now = now.Add(2 * time.Second)
		if err := source.Heartbeat(ctx); err != nil {
			t.Fatal(err)
		}
		if reported != want {
			t.Fatalf("heartbeat capacity = %d, want %d", reported, want)
		}
	}
	assertCapacity(2)
	for _, capacity := range []int{6, 1} {
		report := owner(ctx, nil)
		result := owner(ctx, &runnerauth.CapacityRequest{ExpectedConfigRevision: report.Revision, Capacity: capacity, Backend: "codex"})
		if result == nil || result.LocalLimit != capacity {
			t.Fatalf("capacity save = %+v", result)
		}
		assertCapacity(capacity)
	}
	routed := state.get()
	routed.Projects = []globalconfig.Project{{ID: "routed", Weight: 1, Paused: true}}
	routed.Client.NativeProjects = map[string]string{"routed": "prj_example"}
	if err := runtime.applyProjects(ctx, routed); err != nil {
		t.Fatal(err)
	}
	saved, err := readRunnerConfiguration(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	saved.Global.MaxConcurrentAgents, saved.Client.Capacity, saved.LogLevel = 4, 4, "debug"
	if err := globalconfig.Write(cfg.Path, saved); err != nil {
		t.Fatal(err)
	}
	assertCapacity(4)
	if level.Level() != slog.LevelDebug {
		t.Fatalf("log level = %s", level.Level())
	}
	if current := state.get(); len(current.Projects) != 1 || current.Projects[0].ID != "routed" || current.Client.NativeProjects["routed"] != "prj_example" {
		t.Fatalf("file reload lost Hub routing: %+v", current.Projects)
	}
	routed.Projects[0].Paused = false
	if err := runtime.applyProjects(ctx, routed); err != nil {
		t.Fatal(err)
	}
	if got := state.get().Global.MaxConcurrentAgents; got != 4 {
		t.Fatalf("Hub refresh reverted reloaded capacity: %d", got)
	}
	assertCapacity(4)
	saved.Global.AgentPools = []globalconfig.AgentPool{{Name: "routed", MaxConcurrentAgents: 2}}
	if _, err := runtime.applyFile(ctx, saved); err != nil {
		t.Fatal(err)
	}
	routed.Projects[0].Pool = "routed"
	if err := runtime.applyProjects(ctx, routed); err != nil {
		t.Fatal(err)
	}
	if got := gate.PoolSnapshotFor("routed").Name; got != "routed" {
		t.Fatalf("Hub project pool = %s", got)
	}
	if _, err := runtime.applyFile(ctx, saved); err != nil {
		t.Fatal(err)
	}
	if got := gate.PoolSnapshotFor("routed").Name; got != "routed" {
		t.Fatalf("reloaded project pool = %s", got)
	}
	routed.Projects[0].Pool = "default"
	if err := runtime.applyProjects(ctx, routed); err != nil {
		t.Fatal(err)
	}
	if got := gate.PoolSnapshotFor("routed").Name; got != "default" {
		t.Fatalf("refreshed project pool = %s", got)
	}
}
