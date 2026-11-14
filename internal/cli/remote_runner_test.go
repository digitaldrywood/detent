package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
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

func TestHubRunnerBootAppliesHubConfigurationCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, entry := range runnerBootEntries() {
		t.Run(entry.name, func(t *testing.T) {
			root := t.TempDir()
			identityPath := filepath.Join(root, "private", "identity.json")
			desired := runnerauth.ProjectConfiguration{ProjectID: "prj_example", Authority: "hub_configuration", ConfigRevision: strings.Repeat("d", 64), Source: "cloud_workflow", EffectivePolicy: &policy.Descriptor{ID: "policy_example"}, ObservedAt: time.Now().UTC()}
			request := runnerauth.ProjectConfigurationRequest{RequestID: "detach-example", Operation: "detach_local_project", ProjectID: "prj_example", Desired: &desired}
			var mu sync.Mutex
			var snapshot runnerauth.RoutingSnapshot
			var reports []*runnerauth.ProjectConfiguration
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/capabilities"):
					_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeProviderCapacityCapability, tracker.NativeProjectConfigurationCapability}})
				case strings.HasSuffix(r.URL.Path, "/projects/prj_example"):
					_ = json.NewEncoder(w).Encode(tracker.NativeProject{ID: "prj_example", Profile: "native"})
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					var body struct {
						ProjectConfiguration *runnerauth.ProjectConfiguration `json:"project_configuration"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					mu.Lock()
					defer mu.Unlock()
					reports = append(reports, body.ProjectConfiguration)
					response := snapshot
					if len(reports) == 1 && body.ProjectConfiguration != nil {
						response.ProjectConfiguration = &desired
						response.ProjectConfigurationRequest = &request
					}
					_ = json.NewEncoder(w).Encode(response)
				case strings.HasSuffix(r.URL.Path, "/update/receipt"):
					if r.Method == http.MethodGet {
						_, _ = w.Write([]byte("null"))
						return
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			file, err := runnerauth.Initialize(identityPath, server.URL)
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_example"
			file.Identity.ProjectIDs = []tracker.ProjectID{"prj_example"}
			file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
			if err := runnerauth.Save(identityPath, file); err != nil {
				t.Fatal(err)
			}
			snapshot = runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 1, ProjectIDs: file.Identity.ProjectIDs}.Normalized()}
			if err := runnerauth.SaveRoutingCache(identityPath, snapshot); err != nil {
				t.Fatal(err)
			}
			cfg, err := globalconfig.DefaultAt(filepath.Join(root, "global.yaml"), globalconfig.WithHome(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Global.MaxConcurrentAgents = 1
			cfg.Client.URL, cfg.Client.IdentityFile, cfg.Client.OrganizationID = server.URL, identityPath, "org_example"
			cfg.Client.Capacity, cfg.Client.HeartbeatIntervalSeconds = 1, 1
			cfg.Client.NativeProjects = map[string]string{"example": "prj_example"}
			controller := NewShutdownController()
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() {
				defer close(done)
				done <- entry.boot(ctx, BootConfig{Mode: BootModeRunning, Headless: true, Version: "dev", Shutdown: controller, Global: cfg})
			}()
			t.Cleanup(func() { cancel(); <-done })
			var stopped error
			var exited bool
			testenv.WaitUntil(t, ctx, func() error {
				mu.Lock()
				defer mu.Unlock()
				if len(reports) >= 2 || exited {
					return nil
				}
				select {
				case stopped, exited = <-done:
					exited = true
					return nil
				default:
					return errors.New("runner has not reported after the Hub configuration command")
				}
			})
			mu.Lock()
			defer mu.Unlock()
			if len(reports) < 2 {
				t.Fatalf("runner stopped before acknowledging the Hub configuration command: %v", stopped)
			}
			if reports[0] == nil {
				t.Fatal("booted runner heartbeat carried no project configuration report; the configuration owner is not installed")
			}
			report := reports[1]
			if report == nil || report.RequestID != request.RequestID || report.ProjectID != desired.ProjectID || report.Authority != "hub_configuration" || report.ConfigRevision != desired.ConfigRevision || report.Source != desired.Source || !report.Applied || !report.Saved || report.RuntimeRegistered || report.Constraint != "" {
				t.Fatalf("booted runner acknowledgment = %+v, want the runtime project manager to apply the delivered detach", report)
			}
		})
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
