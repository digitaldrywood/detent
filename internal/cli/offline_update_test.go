package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/testutil/projectfixture"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

const offlineUpdateVersion = "1.2.3"
const offlineUpdateCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func testOfflineBusyControllerConvergence(t *testing.T, configPath, binary string, serviceConfig detentupdate.Config, options detentupdate.ApplyOptions) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	global, err := globalconfig.Read(configPath)
	if err != nil {
		t.Fatal(err)
	}
	serviceConfig.CurrentVersion, serviceConfig.CurrentCommit = "1.2.2", strings.Repeat("b", 40)
	service := detentupdate.NewService(serviceConfig)
	artifacts, err := detentupdate.NewWorkerArtifacts(service, filepath.Join(t.TempDir(), "workers"))
	if err != nil {
		t.Fatal(err)
	}
	active := make(chan struct{})
	runner := &shutdownBlockingRunner{started: make(chan struct{}, 1), canceled: make(chan struct{}, 1), release: active}
	cfg := workflowconfig.Default()
	cfg.Tracker.Kind = workflowconfig.TrackerHubNative
	cfg.Polling.IntervalMS = 60000
	cfg.Agent.MaxConcurrentAgents = 1
	tracker := fakehub.New(fakehub.Config{Stateful: true, Issues: []connector.Issue{{ID: "active-issue", Identifier: "owner/repository#1", Title: "Active implementation", State: "Todo", AssignedToWorker: true}}})
	project, err := projectfixture.New(projectpkg.Config{
		Project:  globalconfig.Project{ID: "selected", Workdir: t.TempDir(), Weight: 1},
		Workflow: workflowconfig.Workflow{Config: cfg, Prompt: "Test workflow prompt."},
	}, projectpkg.Dependencies{Runner: runner, Connector: tracker})
	if err != nil {
		t.Fatal(err)
	}
	if err := project.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	<-runner.started
	registry := projectpkg.NewRegistry()
	mustSetProject(t, registry, project)
	controller := NewShutdownController()
	replaced := make(chan error, 1)
	restart := &RestartRequest{control: &controllerConnection{}, reserve: func() func() { return func() {} }, execute: func(_ context.Context, path string) error {
		child := exec.CommandContext(ctx, path)
		child.Env = append(os.Environ(), "DETENT_OFFLINE_UPDATE_PHASE=heartbeat", "GOCOVERDIR="+t.TempDir())
		output, err := child.CombinedOutput()
		if err != nil {
			return fmt.Errorf("replacement controller: %w: %s", err, output)
		}
		return nil
	}}
	started, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		if err := runWithShutdown(ctx, runningShutdownConfig{Controller: controller, Registry: registry, DrainTimeout: time.Minute, Logger: logger, Output: io.Discard}, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}); err != nil {
			replaced <- err
			return
		}
		replaced <- nil
	}()
	defer func() { cancel(); <-finished }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	scheduler, err := detentupdate.NewScheduler(detentupdate.SchedulerConfig{
		WorkerArtifacts: artifacts, WorkerPreflight: options.Preflight, Updater: service, ApplyOptions: options, Logger: logger,
		CheckInterval: time.Hour, StatePath: runtimeUpdateStatePath(BootConfig{Global: global}),
		RunningBuild: runnerauth.BuildEvidence{Version: "1.2.2", Commit: strings.Repeat("b", 40), SHA256: strings.Repeat("c", 64), Source: "release", OS: runtime.GOOS, Architecture: runtime.GOARCH, ObservedAt: time.Now().UTC()},
		ReserveIdle:  func(ctx context.Context) (func(), bool) { return func() {}, runtimeUpdateIdle(ctx, registry) },
		ReserveDrain: func(context.Context) (func(), error) {
			return nil, errors.New("routine update must preserve active attempts")
		},
		RequestRestart: func(path string) bool {
			accepted, err := requestRuntimeUpdateRestart(ctx, controller, restart, path, nil)
			if err != nil {
				t.Error(err)
			}
			return accepted
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	reportPath := os.Getenv("DETENT_OFFLINE_UPDATE_REPORT")
	if err := os.Remove(reportPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DETENT_OFFLINE_UPDATE_PHASE", "preflight")
	before := scheduler.EnrolledUpdate(ctx, scheduler.RunningBuild(), nil)
	if before == nil {
		t.Fatal("enrolled update observation is missing before the busy update")
	}
	applied := scheduler.EnrolledUpdate(ctx, scheduler.RunningBuild(), &runnerauth.UpdateRequest{ID: "busy-update", Service: "detent", Version: offlineUpdateVersion, Release: true, FromRelease: true, FollowHub: true, ExpectedBuildRevision: before.Revision, RequestedAt: time.Now().UTC()})
	if applied == nil || applied.Receipt == nil || applied.Receipt.Applied == nil || !applied.Receipt.Applied.VerifiedRelease || applied.Receipt.Status != "restart_requested" || !applied.Receipt.ExecutionArtifact || applied.Running.Version != "1.2.2" || applied.Receipt.Applied.Version != offlineUpdateVersion {
		t.Fatalf("busy update did not apply the release: %+v", applied)
	}
	if restart.Binary() != "" || !controller.updateRestart.Load() {
		t.Fatal("controller replacement did not retain the old attempt owner")
	}
	state, err := project.Orchestrator().State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := state.Running["active-issue"]; !ok {
		t.Fatal("update lost the active attempt")
	}
	if _, ok := state.Claimed["active-issue"]; !ok {
		t.Fatal("update released the active claim before completion")
	}
	if _, err := os.Stat(reportPath); err != nil {
		t.Fatalf("replacement did not report while the active attempt continues: %v", err)
	}
	select {
	case err := <-replaced:
		t.Fatalf("controller exited while its attempt was active: %v", err)
	case <-runner.canceled:
		t.Fatal("update canceled the active attempt")
	default:
	}
	close(active)
	testenv.WaitUntil(t, ctx, func() error {
		if !runtimeUpdateIdle(ctx, registry) {
			return errors.New("active attempt still running")
		}
		return nil
	})
	select {
	case err := <-replaced:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	raw, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var running runnerauth.UpdateObservation
	if err := json.Unmarshal(raw, &running); err != nil {
		t.Fatal(err)
	}
	if running.Validate() != nil || !running.Running.VerifiedRelease || !running.Running.Matches(*applied.Receipt.Applied) || running.Receipt == nil || running.Receipt.Status != "running" || running.Receipt.Running == nil {
		t.Fatalf("replacement controller did not converge to the applied release: %+v", running)
	}
	select {
	case <-runner.canceled:
		t.Fatal("controller convergence canceled the completed attempt")
	default:
	}
}

func offlineUpdateControllerProcess() error {
	phase := os.Getenv("DETENT_OFFLINE_UPDATE_PHASE")
	global, err := globalconfig.Read(os.Getenv("DETENT_OFFLINE_UPDATE_CONFIG"))
	if err != nil {
		return err
	}
	if phase == "preflight" {
		return nil
	}
	cfg := BootConfig{Version: offlineUpdateVersion, Build: buildinfo.Info{Version: offlineUpdateVersion, Commit: offlineUpdateCommit}, Global: global, Shutdown: NewShutdownController(), Restart: NewRestartRequest()}
	drained := false
	scheduler, err := newRuntimeUpdateScheduler(context.Background(), cfg, nil, func(context.Context) (func(), bool) { return func() {}, true }, func(context.Context) (func(), error) {
		drained = true
		return nil, detentupdate.ErrRefused
	})
	if err != nil {
		return err
	}
	heartbeat := scheduler.EnrolledUpdate(context.Background(), scheduler.RunningBuild(), nil)
	if drained {
		return errors.New("offline repair unexpectedly drained")
	}
	raw, err := json.Marshal(heartbeat)
	if err != nil {
		return err
	}
	if err := os.WriteFile(os.Getenv("DETENT_OFFLINE_UPDATE_REPORT"), raw, 0o600); err != nil {
		return err
	}
	return nil
}

type offlineReleaseClient struct {
	release   detentupdate.Release
	downloads map[string][]byte
}

func (c offlineReleaseClient) ListReleases(context.Context) ([]detentupdate.Release, error) {
	return []detentupdate.Release{c.release}, nil
}

func (c offlineReleaseClient) GetRelease(context.Context, string) (detentupdate.Release, error) {
	return c.release, nil
}

func (c offlineReleaseClient) Download(_ context.Context, url string) ([]byte, error) {
	data, ok := c.downloads[url]
	if !ok {
		return nil, fmt.Errorf("missing fixture asset %s", strings.TrimSpace(url))
	}
	return data, nil
}
