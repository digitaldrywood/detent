package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cli/internal/workspacefixture"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestHubRunnerDoesNotOpenLocalRuntimeState(t *testing.T) {
	for _, headless := range []bool{true, false} {
		for _, entry := range runnerBootEntries() {
			t.Run(fmt.Sprintf("headless=%t/%s", headless, entry.name), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "detent.db")
				port := -1
				cfg := BootConfig{
					Mode: BootModeRunning, Headless: headless, Host: "127.0.0.1", Port: &port,
					RuntimeDBPath: path,
					Global: globalconfig.Config{Path: filepath.Join(root, "runner.yaml"),
						Global: globalconfig.Settings{MaxConcurrentAgents: 1},
						Client: globalconfig.HubClient{URL: "https://hub.example.test", TokenEnvironment: "DETENT_TEST_MISSING_WORKER_TOKEN"},
					},
				}
				t.Setenv("DETENT_TEST_MISSING_WORKER_TOKEN", "")
				if err := entry.boot(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "hub worker token") {
					t.Errorf("runner did not fail through Hub credential validation: %v", err)
				}
				for _, statePath := range []string{path, runtimeStoreLockPath(path), runtimeUpdateStatePath(cfg), detentupdate.RecoveryStatePath(cfg.Global.Path)} {
					if _, err := os.Stat(statePath); !os.IsNotExist(err) {
						t.Errorf("remote runner created local state %s: %v", statePath, err)
					}
				}
			})
		}
	}
}

func TestHubRunnerBootsWithoutLocalDatabase(t *testing.T) {
	for _, headless := range []bool{true, false} {
		for _, entry := range runnerBootEntries() {
			t.Run(fmt.Sprintf("headless=%t/%s", headless, entry.name), func(t *testing.T) {
				root := t.TempDir()
				path := filepath.Join(root, "detent.db")
				controller := NewShutdownController()
				cfg := BootConfig{
					Mode: BootModeRunning, Headless: headless, Version: "dev", RuntimeDBPath: path, Shutdown: controller,
					Global: globalconfig.Config{Path: filepath.Join(root, "runner.yaml"),
						Global: globalconfig.Settings{MaxConcurrentAgents: 1},
						Client: globalconfig.HubClient{URL: "https://hub.example.test", TokenEnvironment: "DETENT_TEST_HUB_WORKER_TOKEN"},
					},
				}
				t.Setenv("DETENT_TEST_HUB_WORKER_TOKEN", "test-token")
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() {
					defer close(done)
					done <- entry.boot(ctx, cfg)
				}()
				t.Cleanup(func() { cancel(); <-done })
				testenv.WaitUntil(t, ctx, func() error {
					if controller.Active() {
						return nil
					}
					select {
					case err := <-done:
						return fmt.Errorf("runner stopped before activation: %v", err)
					default:
						return errors.New("runner has not activated")
					}
				})
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("remote runner opened a local database: %v", err)
				}
				if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
					t.Fatalf("remote runner opened a local database lock: %v", err)
				}
				if _, err := os.Stat(runtimeUpdateStatePath(cfg)); !os.IsNotExist(err) {
					t.Fatalf("remote runner persisted a local update receipt: %v", err)
				}
				if _, err := os.Stat(detentupdate.RecoveryStatePath(cfg.Global.Path)); !os.IsNotExist(err) {
					t.Fatalf("remote runner persisted local update recovery state: %v", err)
				}
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("runner shutdown = %v, want cancellation after joining workers", err)
				}
			})
		}
	}
}

func runnerBootEntries() []struct {
	name string
	boot func(context.Context, BootConfig) error
} {
	return []struct {
		name string
		boot func(context.Context, BootConfig) error
	}{
		{name: "default boot", boot: defaultBoot},
		{name: "running boot", boot: startRunning},
		{name: "running dependencies", boot: func(ctx context.Context, cfg BootConfig) error {
			return startRunningWithDependencies(ctx, cfg, startRunningDependencies{})
		}},
	}
}

func TestRunnerRequiresHubBeforeLocalStartup(t *testing.T) {
	for _, headless := range []bool{true, false} {
		for _, entry := range runnerBootEntries() {
			for _, projectKind := range []string{"empty", "hub native", "unreadable"} {
				t.Run(fmt.Sprintf("headless=%t/%s/%s", headless, entry.name, projectKind), func(t *testing.T) {
					root := t.TempDir()
					var output bytes.Buffer
					port := -1
					cfg := BootConfig{
						Mode: BootModeRunning, Headless: headless, Host: "127.0.0.1", Port: &port,
						Output: &output, RuntimeDBPath: filepath.Join(root, "runtime.db"),
						Global: globalconfig.Config{Path: filepath.Join(root, "runner.yaml")},
					}
					if projectKind != "empty" {
						workflow := filepath.Join(root, "WORKFLOW.md")
						if projectKind == "hub native" {
							if err := os.WriteFile(workflow, []byte("---\ntracker:\n  kind: hub_native\n---\nWork the issue.\n"), 0o600); err != nil {
								t.Fatal(err)
							}
						}
						cfg.Global.Projects = []globalconfig.Project{{ID: "example", Workflow: workflow}}
					}
					err := entry.boot(t.Context(), cfg)
					problem := ProblemForError(err)
					if err == nil || problem.Code != "validation" || !strings.Contains(problem.Detail, "client.hub_url") {
						t.Errorf("startup error = %v, want Hub configuration validation before binding", err)
					}
					if !strings.Contains(problem.SuggestedFix, "enroll a runner") {
						t.Errorf("startup hint = %q, want Hub enrollment guidance", problem.SuggestedFix)
					}
					for _, path := range []string{cfg.RuntimeDBPath, runtimeStoreLockPath(cfg.RuntimeDBPath), runtimeUpdateStatePath(cfg), detentupdate.RecoveryStatePath(cfg.Global.Path)} {
						if _, err := os.Stat(path); !os.IsNotExist(err) {
							t.Errorf("startup created local state %s: %v", path, err)
						}
					}
					if output.Len() != 0 {
						t.Errorf("startup announced a listener: %s", output.String())
					}
				})
			}
		}
	}
}

type runnerUpdateReceiptFailure struct {
	err error
}

func (s runnerUpdateReceiptFailure) Load() (*runnerauth.UpdateReceipt, error) {
	return nil, s.err
}

func (s runnerUpdateReceiptFailure) Save(*runnerauth.UpdateReceipt) error {
	return s.err
}

func TestHubRunnerUpdateUsesRemoteReceiptOwner(t *testing.T) {
	for _, headless := range []bool{true, false} {
		t.Run(fmt.Sprintf("headless=%t", headless), func(t *testing.T) {
			root := t.TempDir()
			cfg := BootConfig{Headless: headless, Version: "dev", Global: globalconfig.Config{
				Path:   filepath.Join(root, "runner.yaml"),
				Client: globalconfig.HubClient{URL: "https://hub.example.test"},
			}}
			ownerErr := errors.New("remote receipt unavailable")
			_, err := newRuntimeUpdateScheduler(t.Context(), cfg, nil, nil, nil, runnerUpdateReceiptFailure{err: ownerErr})
			if !errors.Is(err, ownerErr) {
				t.Fatalf("scheduler startup = %v, want remote receipt owner failure", err)
			}
			for _, path := range []string{runtimeUpdateStatePath(cfg), detentupdate.RecoveryStatePath(cfg.Global.Path)} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("scheduler created local state %s: %v", path, err)
				}
			}
		})
	}
}

func TestRemoteRunnerWorkspaceLane(t *testing.T) {
	identityPath := filepath.Join(t.TempDir(), "private", "runner.json")
	identity, err := runnerauth.Initialize(identityPath, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	identity.Identity.OrganizationID = "org_test"
	identity.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
	identity.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
	if err := runnerauth.Save(identityPath, identity); err != nil {
		t.Fatal(err)
	}

	seenSessions := map[string]bool{}
	for _, scenario := range []struct {
		name  string
		valid bool
	}{{"serving", true}, {"restarted", true}, {"unresolvable policy", false}} {
		valid := scenario.valid
		t.Run(scenario.name, func(t *testing.T) {
			root := t.TempDir()
			selected := writeWorkspaceLaneProject(t, "example")
			runDoctorWorkflowSourceGit(t, selected.Workdir, "init")
			if !valid {
				selected.Workflow = filepath.Join(root, "missing", "WORKFLOW.md")
			}
			ticks := make(chan time.Time, 1)
			tickerReady := make(chan struct{})
			hub := workspacefixture.New(t, identity)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			cfg := BootConfig{Version: "dev", Headless: true, Shutdown: NewShutdownController(), remoteHubHTTPClient: hub.Client}
			cfg.remoteHeartbeatTicker = func(time.Duration) (<-chan time.Time, func()) { close(tickerReady); return ticks, func() {} }
			cfg.Global, err = globalconfig.Default()
			if err != nil {
				t.Fatal(err)
			}
			selected.Weight = 1
			cfg.Global.Projects = []globalconfig.Project{selected}
			cfg.Global.Client = globalconfig.HubClient{URL: identity.HubURL, IdentityFile: identityPath, OrganizationID: "org_test", NativeProjects: map[string]string{"example": "prj_test"}, ProviderCapacityFile: writeProviderCapacityFixture(t, root)}
			cfg.Global.Path = ""
			done := make(chan error, 1)
			go func() { done <- startRemoteRunner(ctx, cfg) }()
			joined := false
			t.Cleanup(func() {
				cancel()
				hub.Unblock()
				if !joined {
					<-done
				}
			})
			waitCtx := testenv.WaitContext(t, ctx)
			wait := func(ch <-chan struct{}) {
				t.Helper()
				select {
				case <-ch:
				case err := <-done:
					joined = true
					t.Fatalf("runner stopped: %v", err)
				case <-waitCtx.Done():
					t.Fatal("workspace lane did not advance")
				}
			}
			wait(tickerReady)
		drain:
			for {
				select {
				case <-hub.Reports:
				default:
					break drain
				}
			}
			ticks <- time.Now()
			seen := false
			for !seen {
				select {
				case report := <-hub.Reports:
					if valid {
						if report == nil || !report.Files {
							t.Fatalf("serving lane advertised no capabilities: %+v", report)
						}
						seen = true
					} else {
						if report != nil && *report != (workspacesession.Capabilities{}) {
							t.Fatalf("no lane advertised capabilities: %+v", report)
						}
						seen = true
					}
				case err := <-done:
					joined = true
					t.Fatalf("runner stopped before heartbeat: %v", err)
				case <-waitCtx.Done():
					t.Fatal("heartbeat not observed")
				}
			}
			if valid {
				wait(hub.Released)
			}
			cancel()
			if valid {
				select {
				case err := <-done:
					joined = true
					t.Fatalf("runner did not join workspace release: %v", err)
				default:
				}
			}
			hub.Unblock()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Errorf("shutdown = %v", err)
			}
			joined = true
			if valid {
				if seenSessions[hub.WorkspaceSession] {
					t.Fatalf("runner restart reused session %q", hub.WorkspaceSession)
				}
				seenSessions[hub.WorkspaceSession] = true
				for _, id := range hub.IssueSessions {
					if id == hub.WorkspaceSession {
						t.Fatal("workspace reused issue session")
					}
				}
			}
			if valid && hub.Claims != 1 {
				t.Errorf("claims = %d, want one", hub.Claims)
			}
		})
	}
}
