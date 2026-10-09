package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCollectRunnerLocalChecks(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name                                   string
		pi                                     bool
		startup                                bool
		checkout                               bool
		externalWorkflow                       bool
		missingOrigin                          bool
		unsupportedOrigin                      bool
		missingWorkflow                        bool
		runnerSetup                            bool
		doctor                                 doctorStatus
		auth                                   bool
		wantCheckout, wantDoctor, wantProvider string
	}{
		{name: "missing checkout", doctor: doctorOK, auth: true, wantCheckout: "failed", wantDoctor: "pending", wantProvider: "pending"},
		{name: "failed doctor", checkout: true, doctor: doctorFail, auth: true, wantCheckout: "passed", wantDoctor: "failed", wantProvider: "passed"},
		{name: "missing provider", checkout: true, doctor: doctorOK, wantCheckout: "passed", wantDoctor: "passed", wantProvider: "failed"},
		{name: "declared runner setup", runnerSetup: true, checkout: true, doctor: doctorOK, auth: true, wantCheckout: "passed", wantDoctor: "passed", wantProvider: "passed"},
		{name: "success", checkout: true, doctor: doctorOK, auth: true, wantCheckout: "passed", wantDoctor: "passed", wantProvider: "passed"},
		{name: "startup readiness ignores operational instruction estimate", startup: true, checkout: true, auth: true, wantCheckout: "passed", wantDoctor: "passed", wantProvider: "passed"},
		{name: "warnings", checkout: true, doctor: doctorWarn, auth: true, wantCheckout: "passed", wantDoctor: "warning", wantProvider: "passed"},
		{name: "Pi auth unknown", pi: true, checkout: true, doctor: doctorOK, wantCheckout: "passed", wantDoctor: "passed", wantProvider: "pending"},
		{name: "configured workflow outside committed checkout", checkout: true, externalWorkflow: true, doctor: doctorOK, auth: true, wantCheckout: "passed", wantDoctor: "passed", wantProvider: "passed"},
		{name: "missing Git origin", checkout: true, missingOrigin: true, doctor: doctorOK, auth: true, wantCheckout: "passed", wantDoctor: "passed", wantProvider: "passed"},
		{name: "GitLab origin with external workflow", checkout: true, externalWorkflow: true, unsupportedOrigin: true, doctor: doctorOK, auth: true, wantCheckout: "passed", wantDoctor: "passed", wantProvider: "passed"},
		{name: "missing configured workflow", checkout: true, externalWorkflow: true, missingWorkflow: true, doctor: doctorOK, auth: true, wantCheckout: "failed", wantDoctor: "pending", wantProvider: "pending"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "global.yaml")
			cfg, err := globalconfig.DefaultAt(path, globalconfig.WithHome(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Path = path
			workdir := filepath.Join(root, "checkout")
			workflowPath := filepath.Join(workdir, "WORKFLOW.md")
			if tt.externalWorkflow {
				workflowPath = filepath.Join(root, "WORKFLOW.md")
			}
			cfg.Projects = []globalconfig.Project{{ID: "orders", Workflow: workflowPath, Workdir: workdir, Weight: 1}}
			if err := globalconfig.Write(path, cfg, globalconfig.WithMissingProjectPaths()); err != nil {
				t.Fatal(err)
			}
			if tt.checkout {
				if err := os.MkdirAll(workdir, 0700); err != nil {
					t.Fatal(err)
				}
				body := "---\ntracker:\n  kind: memory\n  repository: acme/orders\n"
				if tt.runnerSetup {
					body += "hooks:\n  runner_setup: setup.sh\n"
				}
				if tt.pi {
					body += "agents:\n  backends:\n    - id: pi\n      kind: pi_agent\n  routes:\n    - backend: pi\n      default: true\n"
				}
				body += "---\nPRIVATE WORKFLOW\n"
				if tt.startup {
					body += strings.Repeat("Read `gate.run` before review.\n", 3000)
				}
				if !tt.missingWorkflow {
					if err := os.WriteFile(workflowPath, []byte(body), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(workdir, "source.go"), []byte("package orders\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runDoctorWorkflowSourceGit(t, workdir, "init")
				runDoctorWorkflowSourceGit(t, workdir, "add", ".")
				runDoctorWorkflowSourceGit(t, workdir, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "source checkout")
				if !tt.missingOrigin {
					remote := "git@github.com:acme/orders.git"
					if tt.unsupportedOrigin {
						remote = "git@gitlab.com:acme/orders.git"
					}
					runDoctorWorkflowSourceGit(t, workdir, "remote", "add", "origin", remote)
				}
				if !tt.missingWorkflow {
					if _, _, _, err := resolveRunnerSetupPolicy(t.Context(), path, "orders"); err != nil {
						t.Fatalf("configured workflow cannot resolve: %v", err)
					}
				}
				if status := runDoctorWorkflowSourceGit(t, workdir, "status", "--porcelain"); status != "" {
					t.Fatalf("source checkout is dirty: %s", status)
				}
			}
			var doctors, auths int
			checks := collectRunnerLocalChecks(t.Context(), cfg, "orders", func(_ context.Context, c doctorConfig) doctorReport {
				doctors++
				if c.ProjectID != "orders" || c.ConfigPath != path || c.AllowWriteProbes || c.Flags.Port.Value != 0 {
					t.Fatalf("wrong doctor context: %+v", c)
				}
				if tt.startup {
					return runDoctorStartupPreflight(t.Context(), c, options{}, doctorDeps{})
				}
				return doctorReport{Checks: []doctorCheck{{Status: tt.doctor, Detail: "secret-token PRIVATE WORKFLOW", Hint: "secret-hint"}}}
			}, func(_ context.Context, b workflowconfig.AgentBackend) bool { auths++; return tt.auth })
			if checks.Checkout != tt.wantCheckout || checks.Doctor != tt.wantDoctor || checks.Provider != tt.wantProvider {
				t.Fatalf("checks=%+v", checks)
			}
			if (checks.RunnerSetupDeclared != nil) != (tt.wantCheckout == "passed") || checks.RunnerSetupDeclared != nil && *checks.RunnerSetupDeclared != tt.runnerSetup {
				t.Fatalf("runner setup declaration = %v", checks.RunnerSetupDeclared)
			}
			if tt.wantCheckout == "passed" && (doctors != 1 || !tt.pi && auths == 0 || tt.pi && auths != 0) {
				t.Fatalf("probes doctor=%d auth=%d", doctors, auths)
			}
			if tt.wantCheckout != "passed" && (doctors != 0 || auths != 0) {
				t.Fatal("probed before checkout")
			}
			encoded, err := json.Marshal(checks)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "PRIVATE") {
				t.Fatalf("private output escaped: %s", encoded)
			}
		})
	}
}

func TestReadRunnerSetupConfigMissingCheckout(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name              string
		homePaths         bool
		invalidWeight     bool
		workflowDirectory bool
		workdirFile       bool
		wantError         string
	}{
		{name: "absent checkout"},
		{name: "absent home-relative paths", homePaths: true},
		{name: "invalid config with absent checkout", invalidWeight: true, wantError: "weight: must be a positive integer"},
		{name: "workflow is a directory", workflowDirectory: true, wantError: "workflow: path does not exist"},
		{name: "workdir is a file", workdirFile: true, wantError: "workdir: path does not exist"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			paths := runnerPaths{config: filepath.Join(root, "global.yaml"), identity: filepath.Join(root, "identity.json"), workspaces: filepath.Join(root, "missing")}
			workdir := filepath.Join(paths.workspaces, "orders")
			workflow := filepath.Join(workdir, "WORKFLOW.md")
			config := runnerConfig("http://127.0.0.1:1", "org_test", "host", 1, paths, []runnerRegisteredCheck{{Name: "orders", ID: "prj_orders", Workdir: workdir}})
			if test.homePaths {
				home, err := os.UserHomeDir()
				if err != nil {
					t.Fatal(err)
				}
				relative, err := filepath.Rel(home, workdir)
				if err != nil {
					t.Fatal(err)
				}
				config.Projects[0].Workdir = "~/" + relative
				config.Projects[0].Workflow = "~/" + filepath.Join(relative, "WORKFLOW.md")
			}
			if test.invalidWeight {
				config.Projects[0].Weight = 0
			}
			if test.workflowDirectory {
				if err := os.MkdirAll(workflow, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if test.workdirFile {
				if err := os.MkdirAll(paths.workspaces, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(workdir, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := writeRunnerConfig(paths.config, config); err != nil {
				t.Fatal(err)
			}
			cfg, err := readRunnerSetupConfig(paths.config)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Path != paths.config || len(cfg.Projects) != 1 || cfg.Client.NativeProjects["orders"] != "prj_orders" || cfg.Projects[0].Workdir != workdir || cfg.Projects[0].Workflow != workflow || cfg.Global.Cache.MaxAge == 0 {
				t.Fatalf("config=%+v", cfg)
			}
		})
	}
}

// Catch a separate startup heartbeat bypassing negotiation or dropping the
// startup observations before the scheduler's first enrolled heartbeat.
func TestRunnerSetupHeartbeatOwnership(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	t.Parallel()
	for _, supported := range []bool{false, true} {
		name := "older Hub"
		if supported {
			name = "current Hub"
		}
		t.Run(name, func(t *testing.T) {
			var heartbeats atomic.Int32
			var snapshot runnerauth.RoutingSnapshot
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/v2/capabilities":
					features := []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeProviderCapacityCapability}
					if supported {
						features = append(features, tracker.NativeLocalChecksCapability)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": features})
				case r.URL.Path == "/api/v2/organizations/org_test/projects/prj_test":
					_ = json.NewEncoder(w).Encode(tracker.NativeProject{Profile: "native"})
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					var body struct {
						LocalChecks *runnerauth.LocalChecks `json:"local_checks"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if !supported && body.LocalChecks != nil {
						w.WriteHeader(http.StatusUnprocessableEntity)
						return
					}
					if supported && (body.LocalChecks == nil || body.LocalChecks.Checkout != "failed" || body.LocalChecks.Doctor != "pending" || body.LocalChecks.Provider != "pending") {
						t.Errorf("startup observations missing or forged: %+v", body.LocalChecks)
					}
					heartbeats.Add(1)
					_ = json.NewEncoder(w).Encode(snapshot)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			root := t.TempDir()
			paths := runnerPaths{config: filepath.Join(root, "global.yaml"), identity: filepath.Join(root, "private", "identity.json"), workspaces: filepath.Join(root, "workspaces")}
			file, err := runnerauth.Initialize(paths.identity, server.URL)
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_test"
			file.Identity.ProjectIDs = []tracker.ProjectID{"prj_test"}
			file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
			if err := runnerauth.Save(paths.identity, file); err != nil {
				t.Fatal(err)
			}
			snapshot = runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 1}.Normalized()}
			if err := runnerauth.SaveRoutingCache(paths.identity, snapshot); err != nil {
				t.Fatal(err)
			}
			config := runnerConfig(server.URL, "org_test", "Runner", 1, paths, []runnerRegisteredCheck{{Name: "native", ID: "prj_test", Workdir: filepath.Join(paths.workspaces, "missing")}})
			if _, err := writeRunnerConfig(paths.config, config); err != nil {
				t.Fatal(err)
			}
			cfg, err := readRunnerSetupConfig(paths.config)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Client.ProviderCapacityFile = writeProviderCapacityFixture(t, root)
			source, err := newHubScheduling(t.Context(), cfg, "test")
			if err != nil {
				t.Fatal(err)
			}
			if heartbeats.Load() != 0 {
				t.Fatal("startup sent a separate heartbeat")
			}
			scheduler := source.(*hubclient.Scheduler)
			if err := scheduler.Heartbeat(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := scheduler.Heartbeat(t.Context()); err != nil {
				t.Fatal(err)
			}
			if heartbeats.Load() != 1 {
				t.Fatalf("startup heartbeats = %d, want 1", heartbeats.Load())
			}
			if err := reportRunnerSetup(t.Context(), cfg, "test"); err != nil {
				t.Fatal(err)
			}
			if heartbeats.Load() != 2 {
				t.Fatalf("registration heartbeats = %d, want 2", heartbeats.Load())
			}
		})
	}
}
