package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/blake2b"

	"github.com/digitaldrywood/detent/internal/buildinfo"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/testutil/projectfixture"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

const offlineUpdateVersion = "1.2.3"
const offlineUpdateCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestOfflineCLIUpdateReportsVerifiedController(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows replacement completes in a detached process")
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "selected.yaml")
	if err := os.WriteFile(configPath, []byte("apiVersion: detent/v1\nkind: GlobalConfig\nglobal:\n  max_concurrent_agents: 2\n  scheduling: weighted\nupdate:\n  auto_check_enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DETENT_INSTALL_LOCK", filepath.Join(dir, "install.lock"))
	t.Setenv("DETENT_OFFLINE_UPDATE_CONFIG", configPath)
	t.Setenv("DETENT_OFFLINE_UPDATE_REPORT", filepath.Join(dir, "heartbeat.json"))
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyID := []byte("12345678")
	publicKey := base64.StdEncoding.EncodeToString(append(append([]byte("Ed"), keyID...), public...))
	candidate := filepath.Join(dir, "candidate")
	buildCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	build := exec.CommandContext(buildCtx, "go", "test", "-c", "-p", "4", "-ldflags", "-s -w -X github.com/digitaldrywood/detent/internal/update.defaultChecksumMinisignPublicKey="+publicKey, "-o", candidate, ".")
	build.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "GOCACHE=") })
	if fixtureBuildCache != "" {
		build.Env = append(build.Env, "GOCACHE="+fixtureBuildCache)
	}
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build controller fixture: %v\n%s", err, output)
	}
	payload, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	gz, err := gzip.NewWriterLevel(&archive, gzip.NoCompression)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "detent", Mode: 0o755, Size: int64(len(payload))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := provenance.Marshal(provenance.Manifest{Schema: provenance.Schema, Repository: "digitaldrywood/detent", Tag: "v" + offlineUpdateVersion, Commit: offlineUpdateCommit,
		Checks: []provenance.Check{{Name: "CI", Status: "completed", Conclusion: "success", CheckRunID: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	archiveName := fmt.Sprintf("detent_%s_%s_%s.tar.gz", offlineUpdateVersion, runtime.GOOS, runtime.GOARCH)
	checksumName := "detent_" + offlineUpdateVersion + "_checksums.txt"
	checksums := fmt.Appendf(nil, "%x  %s\n%x  detent_release_provenance.json\n", sha256.Sum256(archive.Bytes()), archiveName, sha256.Sum256(manifest))
	digest := blake2b.Sum512(checksums)
	messageSignature := ed25519.Sign(private, digest[:])
	packet := append(append([]byte("ED"), keyID...), messageSignature...)
	trusted := "offline fixture"
	signature := fmt.Appendf(nil, "untrusted comment: test\n%s\ntrusted comment: %s\n%s\n", base64.StdEncoding.EncodeToString(packet), trusted,
		base64.StdEncoding.EncodeToString(ed25519.Sign(private, append(messageSignature, []byte(trusted)...))))
	client := offlineReleaseClient{downloads: map[string][]byte{archiveName: archive.Bytes(), checksumName: checksums, checksumName + ".minisig": signature, "detent_release_provenance.json": manifest}}
	for name := range client.downloads {
		client.release.Assets = append(client.release.Assets, detentupdate.Asset{Name: name, BrowserDownloadURL: name})
	}
	client.release.TagName = "v" + offlineUpdateVersion
	binary := filepath.Join(dir, "detent")
	if err := os.WriteFile(binary, []byte("unverified old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("DETENT_INSTALL_LOCK"), []byte("binary="+binary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceConfig := detentupdate.Config{CurrentVersion: offlineUpdateVersion, CurrentCommit: offlineUpdateCommit, ExecutablePath: binary, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Client: client,
		ChecksumSignatureVerifier: func(_ context.Context, checksums, signature []byte) error {
			return detentupdate.VerifyMinisignSignature(publicKey, checksums, signature)
		},
		BinaryVerifier: func(context.Context, string) (string, error) {
			return "version: v" + offlineUpdateVersion + "\ncommit: " + offlineUpdateCommit + "\n", nil
		},
	}
	service := detentupdate.NewService(serviceConfig)
	options := detentupdate.ApplyOptions{AssumeYes: true, FromRelease: true, RecoveryStatePath: detentupdate.RecoveryStatePath(configPath),
		Preflight: detentupdate.CommandPreflight("-test.run=^TestOfflineUpdateControllerProcess$"),
	}
	command := &cobra.Command{}
	command.Flags().String("config", configPath, "")
	if _, handled, err := ApplyRunningUpdate(t.Context(), command, options); err != nil || handled {
		t.Fatalf("offline CLI coordination: handled=%t err=%v", handled, err)
	}
	t.Setenv("DETENT_OFFLINE_UPDATE_PHASE", "preflight")
	applied, err := service.Apply(t.Context(), options)
	if err != nil || !applied.VerifiedRelease || applied.Action != detentupdate.ActionUpdated {
		t.Fatalf("offline CLI apply: %+v %v", applied, err)
	}
	if _, err := os.Stat(options.RecoveryStatePath); err != nil {
		t.Fatalf("selected config lost startup recovery: %v", err)
	}
	t.Setenv("DETENT_OFFLINE_UPDATE_PHASE", "heartbeat")
	child := exec.CommandContext(t.Context(), binary, "-test.run=^TestOfflineUpdateControllerProcess$")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("fresh controller: %v\n%s", err, output)
	}
	raw, err := os.ReadFile(os.Getenv("DETENT_OFFLINE_UPDATE_REPORT"))
	if err != nil {
		t.Fatal(err)
	}
	var heartbeat runnerauth.UpdateObservation
	if err := json.Unmarshal(raw, &heartbeat); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if heartbeat.Validate() != nil || !heartbeat.Running.VerifiedRelease || heartbeat.Running.Source != "release" || heartbeat.Running.Version != offlineUpdateVersion || heartbeat.Running.Commit != offlineUpdateCommit || heartbeat.Running.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("offline evidence did not reach fresh heartbeat: %+v", heartbeat)
	}
	t.Run("busy slots converge to the applied controller", func(t *testing.T) {
		testOfflineBusyControllerConvergence(t, configPath, binary, serviceConfig, options)
	})
}

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
		child := exec.CommandContext(ctx, path, "-test.run=^TestOfflineUpdateControllerProcess$")
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

func TestOfflineUpdateControllerProcess(t *testing.T) {
	phase := os.Getenv("DETENT_OFFLINE_UPDATE_PHASE")
	if phase == "" {
		t.Skip("controller subprocess fixture")
	}
	global, err := globalconfig.Read(os.Getenv("DETENT_OFFLINE_UPDATE_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if phase == "preflight" {
		return
	}
	cfg := BootConfig{Version: offlineUpdateVersion, Build: buildinfo.Info{Version: offlineUpdateVersion, Commit: offlineUpdateCommit}, Global: global, Shutdown: NewShutdownController(), Restart: NewRestartRequest()}
	scheduler, err := newRuntimeUpdateScheduler(t.Context(), cfg, nil, func(context.Context) (func(), bool) { return func() {}, true }, func(context.Context) (func(), error) {
		t.Fatal("offline repair unexpectedly drained")
		return nil, detentupdate.ErrRefused
	})
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := enrolledUpdateOwner(t.Context(), scheduler, scheduler.RunningBuild())(t.Context(), nil)
	raw, err := json.Marshal(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv("DETENT_OFFLINE_UPDATE_REPORT"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
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
