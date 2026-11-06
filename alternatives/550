package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestCollectRunnerLocalChecks(t *testing.T) {
	if testing.Short() {
		t.Skip("git subprocess integration")
	}

	t.Parallel()
	for _, tt := range []struct {
		name                                   string
		configFixture                          bool
		invalidYAML                            bool
		pi                                     bool
		startup                                bool
		checkout                               bool
		externalWorkflow                       bool
		missingOrigin                          bool
		unsupportedOrigin                      bool
		missingWorkflow                        bool
		refWorkflow                            bool
		runnerSetup                            bool
		doctor                                 doctorStatus
		auth                                   bool
		wantCheckout, wantDoctor, wantProvider string
	}{
		{name: "retired configuration keys", checkout: true, configFixture: true, wantCheckout: "passed", wantDoctor: "pending", wantProvider: "pending"},
		{name: "invalid configuration YAML", checkout: true, invalidYAML: true, wantCheckout: "passed", wantDoctor: "pending", wantProvider: "pending"},
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
		{name: "Git ref workflow absent locally", checkout: true, refWorkflow: true, doctor: doctorOK, auth: true, wantCheckout: "passed", wantDoctor: "passed", wantProvider: "passed"},
		{name: "missing configured workflow", checkout: true, externalWorkflow: true, missingWorkflow: true, doctor: doctorOK, auth: true, wantCheckout: "passed", wantDoctor: "pending", wantProvider: "pending"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "global.yaml")
			cfg, err := globalconfig.DefaultAt(path, globalconfig.WithHome(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Path = path
			cfg.Client.URL = "https://hub.example.test"
			workdir := filepath.Join(root, "checkout")
			workflowPath := filepath.Join(workdir, "WORKFLOW.md")
			if tt.externalWorkflow {
				workflowPath = filepath.Join(root, "WORKFLOW.md")
			}
			cfg.Projects = []globalconfig.Project{{ID: "orders", Workflow: workflowPath, Workdir: workdir, Weight: 1}}
			if tt.refWorkflow {
				cfg.Projects[0].WorkflowRef = "HEAD"
			}
			if err := globalconfig.Write(path, cfg, globalconfig.WithMissingProjectPaths()); err != nil {
				t.Fatal(err)
			}
			if tt.checkout {
				if err := os.MkdirAll(workdir, 0700); err != nil {
					t.Fatal(err)
				}
				body := "---\ntracker:\n  kind: hub_native\n  repository: acme/orders\n"
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
				if tt.configFixture || tt.invalidYAML {
					raw, err := os.ReadFile("../config/testdata/local-era.yaml")
					if err != nil {
						t.Fatal(err)
					}
					if tt.invalidYAML {
						raw = []byte("schema: 1\nagent:\n  max_turns: PRIVATE\n")
					}
					if err := os.WriteFile(filepath.Join(workdir, "detent.yaml"), raw, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(workflowPath, []byte("Instructions.\n"), 0600); err != nil {
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
				if !tt.missingWorkflow && !tt.configFixture && !tt.invalidYAML {
					if _, _, _, err := resolveRunnerProjectPolicy(t.Context(), cfg, "orders"); err != nil {
						t.Fatalf("configured workflow cannot resolve: %v", err)
					}
				}
				if status := runDoctorWorkflowSourceGit(t, workdir, "status", "--porcelain"); status != "" {
					t.Fatalf("source checkout is dirty: %s", status)
				}
			}
			if tt.refWorkflow {
				if err := os.Remove(workflowPath); err != nil {
					t.Fatal(err)
				}
			}
			var doctors, auths int
			checks := collectRunnerLocalChecks(t.Context(), cfg, "orders", func(_ context.Context, c doctorConfig) doctorReport {
				doctors++
				if c.ProjectID != "orders" || c.ConfigPath != path || c.AllowWriteProbes || c.Flags.Port.Value != 0 {
					t.Fatalf("wrong doctor context: %+v", c)
				}
				if tt.startup {
					opts := options{readDoctorProject: func(string, string) (globalconfig.Config, []string, error) {
						return cfg, nil, nil
					}}
					return runDoctorStartupPreflight(t.Context(), c, opts, doctorDeps{
						gitLandingCredential: func(context.Context, string) error { return nil },
					})
				}
				return doctorReport{Checks: []doctorCheck{{Status: tt.doctor, Detail: "secret-token PRIVATE WORKFLOW", Hint: "secret-hint"}}}
			}, func(_ context.Context, b workflowconfig.AgentBackend) bool { auths++; return tt.auth })
			if checks.Checkout != tt.wantCheckout || checks.Doctor != tt.wantDoctor || checks.Provider != tt.wantProvider {
				t.Fatalf("checks=%+v", checks)
			}
			if (checks.RunnerSetupDeclared != nil) != (tt.wantCheckout == "passed" && !tt.missingWorkflow && !tt.configFixture && !tt.invalidYAML) || checks.RunnerSetupDeclared != nil && *checks.RunnerSetupDeclared != tt.runnerSetup {
				t.Fatalf("runner setup declaration = %v", checks.RunnerSetupDeclared)
			}
			if tt.wantCheckout == "passed" && !tt.missingWorkflow && !tt.configFixture && !tt.invalidYAML && (doctors != 1 || !tt.pi && auths == 0 || tt.pi && auths != 0) {
				t.Fatalf("probes doctor=%d auth=%d", doctors, auths)
			}
			if (tt.wantCheckout != "passed" || tt.missingWorkflow || tt.configFixture || tt.invalidYAML) && (doctors != 0 || auths != 0) {
				t.Fatal("probed before checkout")
			}
			if tt.missingWorkflow && checks.Configuration != "missing" {
				t.Fatalf("configuration not missing: %+v", checks)
			}
			if tt.configFixture || tt.invalidYAML {
				if checks.Configuration != "failed" || checks.ConfigurationMessage == "" {
					t.Fatalf("missing configuration detail: %+v", checks)
				}
				if err := checks.Validate(); err != nil {
					t.Fatal(err)
				}
				if tt.invalidYAML && strings.Contains(checks.ConfigurationMessage, "PRIVATE") {
					t.Fatalf("configuration value escaped: %+v", checks)
				}
				if tt.configFixture {
					for _, key := range []string{"intake", "routines", "retro", "backlog_admission", "tracker.github_status_source", "tracker.project_slug", "tracker.status_field", "tracker.status_label_prefix", "tracker.write_probe_issue", "server.host", "server.port", "server.board_snapshot_stale_after_seconds"} {
						if !strings.Contains(checks.ConfigurationMessage, key+": retired configuration key") {
							t.Fatalf("missing %s: %+v", key, checks)
						}
					}
					if !strings.Contains(checks.ConfigurationFix, "detent config migrate detent.yaml") {
						t.Fatalf("missing fix: %+v", checks)
					}
				}
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

// Catch a separate startup heartbeat bypassing negotiation or dropping the
// startup observations before the scheduler's first enrolled heartbeat.
func TestRunnerSetupHeartbeatOwnership(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, test := range []struct {
		name               string
		supported, commits bool
	}{
		{name: "older Hub"},
		{name: "older Hub with local checks", supported: true},
		{name: "current Hub", supported: true, commits: true},
	} {
		supported, name := test.supported, test.name
		t.Run(name, func(t *testing.T) {
			var heartbeats atomic.Int32
			var commitIdentity workspace.CommitIdentity
			var snapshot runnerauth.RoutingSnapshot
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/v2/capabilities":
					features := []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeProviderCapacityCapability}
					if supported {
						features = append(features, tracker.NativeLocalChecksCapability)
						if test.commits {
							features = append(features, tracker.NativeRunnerCommitPolicyCapability)
						}
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
					if test.commits && (body.LocalChecks.CommitSigning != "unsigned" || body.LocalChecks.CommitName != commitIdentity.Name || body.LocalChecks.CommitEmail != commitIdentity.Email) {
						t.Errorf("commit policy report missing: %+v", body.LocalChecks)
					}
					if supported && !test.commits && body.LocalChecks.CommitSigning != "" {
						t.Errorf("older Hub received commit policy fields: %+v", body.LocalChecks)
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
			config := runnerConfig(server.URL, "org_test", "Runner", 1, paths)
			if _, err := writeRunnerConfig(paths.config, config); err != nil {
				t.Fatal(err)
			}
			cfg, err := readRunnerSetupConfig(paths.config)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Client.ProviderCapacityFile = writeProviderCapacityFixture(t, root)
			cfg.Client.NativeProjects = map[string]string{"native": "prj_test"}
			cfg.Projects = []globalconfig.Project{{ID: "native", Workflow: filepath.Join(paths.workspaces, "missing", "WORKFLOW.md"), Workdir: filepath.Join(paths.workspaces, "missing"), Weight: 1}}
			source, err := newHubScheduling(t.Context(), cfg, "test", hubSchedulingOptions{workspaceSupport: testWorkspaceSupport})
			if err != nil {
				t.Fatal(err)
			}
			if heartbeats.Load() != 0 {
				t.Fatal("startup sent a separate heartbeat")
			}
			scheduler := source.(*hubclient.Scheduler)
			commitIdentity = scheduler.CommitIdentity()
			scheduler.ReportCommitPolicy("native", workspace.CommitReport{Identity: commitIdentity, Signing: "unsigned"})
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

func TestRunnerStartupIsolatesProjectPolicyFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, status := range []int{0, http.StatusConflict, http.StatusInternalServerError, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			root := t.TempDir()
			var failed atomic.Bool
			failed.Store(true)
			checks := make(map[string]runnerauth.LocalChecks)
			problems := make(map[string][]runnerauth.Problem)
			claims := make(map[string]int)
			var snapshot runnerauth.RoutingSnapshot
			var identity runnerauth.Identity
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				name := "healthy"
				if strings.Contains(r.URL.Path, "/prj_failed") {
					name = "failed"
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/runners/"+identity.RunnerID):
					_ = json.NewEncoder(w).Encode(identity)
				case strings.HasSuffix(r.URL.Path, "/capabilities"):
					_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeLocalChecksCapability, tracker.NativeProviderCapacityCapability}})
				case strings.HasSuffix(r.URL.Path, "/policy/observed"):
					if name == "failed" && failed.Load() && status != 0 {
						w.WriteHeader(status)
						_, _ = w.Write([]byte(`{"code":"policy_refused","message":"policy observation refused"}`))
						return
					}
					w.WriteHeader(http.StatusNoContent)
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					var body struct {
						LocalChecks runnerauth.LocalChecks `json:"local_checks"`
						Problems    []runnerauth.Problem   `json:"problems"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					checks[name], problems[name] = body.LocalChecks, body.Problems
					_ = json.NewEncoder(w).Encode(snapshot)
				case strings.HasSuffix(r.URL.Path, "/claims/preview"):
					if checks[name].Configuration != "passed" {
						w.WriteHeader(http.StatusConflict)
						_, _ = w.Write([]byte(`{"code":"runner_setup_required"}`))
						return
					}
					_ = json.NewEncoder(w).Encode(tracker.NativeCapacityPage{Items: []tracker.NativeIssue{{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: tracker.ProjectID("prj_" + name), WorkItemID: tracker.NativeWorkItemID("wi_" + name)}, State: "Todo", SchedulingEligible: new(true), DependencyReady: true}}})
				case strings.HasSuffix(r.URL.Path, "/claims"):
					claims[name]++
					_ = json.NewEncoder(w).Encode(tracker.NativeLease{ID: tracker.LeaseID("lease_" + name), WorkItemID: tracker.NativeWorkItemID("wi_" + name), FencingToken: 1, ServerTime: time.Now(), ExpiresAt: time.Now().Add(time.Hour), ProviderReservation: &providercapacity.Reservation{}, IsolationPolicy: &isolation.Policy{Tier: isolation.NativeTrusted}})
				case strings.Contains(r.URL.Path, "/work-items/wi_"):
					_ = json.NewEncoder(w).Encode(tracker.NativeRecovery{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: tracker.ProjectID("prj_" + name), WorkItemID: tracker.NativeWorkItemID("wi_" + name)}, State: "Todo", SchedulingEligible: new(true), DependencyReady: true}})
				case strings.HasSuffix(r.URL.Path, "/projects/prj_failed"), strings.HasSuffix(r.URL.Path, "/projects/prj_healthy"):
					_ = json.NewEncoder(w).Encode(tracker.NativeProject{ID: tracker.ProjectID("prj_" + name), Profile: "native"})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			identityPath := filepath.Join(root, "private", "identity.json")
			file, err := runnerauth.Initialize(identityPath, server.URL)
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_example"
			file.Identity.ProjectIDs = []tracker.ProjectID{"prj_failed", "prj_healthy"}
			file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
			identity = file.Identity
			if err := runnerauth.Save(identityPath, file); err != nil {
				t.Fatal(err)
			}
			snapshot = runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 2, ProjectIDs: file.Identity.ProjectIDs}.Normalized()}
			if err := runnerauth.SaveRoutingCache(identityPath, snapshot); err != nil {
				t.Fatal(err)
			}
			cfg, err := globalconfig.DefaultAt(filepath.Join(root, "global.yaml"), globalconfig.WithHome(root))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Client.URL, cfg.Client.IdentityFile, cfg.Client.OrganizationID = server.URL, identityPath, "org_example"
			cfg.Client.Capacity = 2
			cfg.Client.ProviderCapacityFile = writeProviderCapacityFixture(t, root)
			cfg.Client.NativeProjects = map[string]string{"failed": "prj_failed", "healthy": "prj_healthy"}
			for _, name := range []string{"failed", "healthy"} {
				dir := filepath.Join(root, name)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				workflow := filepath.Join(dir, "WORKFLOW.md")
				body := "---\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\nagents:\n  backends:\n    - id: pi\n      kind: pi_agent\n      command: 'true'\n  routes:\n    - backend: pi\n      default: true\n---\nInstructions.\n"
				if err := os.WriteFile(workflow, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				if name == "failed" && status == 0 {
					if err := os.WriteFile(workflow, []byte("Instructions.\n"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "detent.yaml"), []byte("schema: 1\nroutines: {}\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				runDoctorWorkflowSourceGit(t, dir, "init")
				runDoctorWorkflowSourceGit(t, dir, "add", ".")
				runDoctorWorkflowSourceGit(t, dir, "-c", "user.name=Example", "-c", "user.email=example@example.test", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
				cfg.Projects = append(cfg.Projects, globalconfig.Project{ID: name, Workdir: dir, Workflow: workflow, Weight: 1})
			}
			if err := globalconfig.Write(cfg.Path, cfg); err != nil {
				t.Fatal(err)
			}
			source, err := newHubScheduling(t.Context(), cfg, "test", hubSchedulingOptions{workspaceSupport: testWorkspaceSupport})
			if status == http.StatusUnauthorized || status == http.StatusForbidden {
				var apiErr *hubclient.APIError
				if !errors.As(err, &apiErr) || apiErr.Status != status {
					t.Fatalf("startup error = %v, want fatal %d", err, status)
				}
				return
			}
			if err != nil {
				t.Fatalf("project failure aborted startup: %v", err)
			}
			scheduler := source.(*hubclient.Scheduler)
			if err := scheduler.Heartbeat(t.Context()); err != nil {
				t.Fatal(err)
			}
			if checks["failed"].Configuration != "failed" || checks["healthy"].Configuration != "passed" {
				t.Fatalf("configuration checks: %+v", checks)
			}
			if status == 0 && (!strings.Contains(checks["failed"].ConfigurationMessage, "retired configuration key") || checks["failed"].ConfigurationFix == "") {
				t.Fatalf("retired-key guidance missing: %+v", checks["failed"])
			}
			if !slices.ContainsFunc(problems["failed"], func(p runnerauth.Problem) bool { return p.Code == "settings_invalid" && p.ProjectID == "prj_failed" }) {
				t.Fatalf("failed project problems: %+v", problems["failed"])
			}
			descriptor := policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "human_review", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("stop")), MergeMethod: "squash"}}.WithID()
			for _, name := range []string{"failed", "healthy"} {
				issues, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: name, Policy: descriptor, ProviderRequirement: func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
					return providercapacity.Requirement{Backend: "codex", Model: "provider_default"}, nil
				}})
				if name == "healthy" && (err != nil || len(issues) != 1) {
					t.Fatalf("healthy project claim: %v, %v", issues, err)
				}
				if name == "failed" && len(issues) != 0 {
					t.Fatalf("failed project dispatched: %v", issues)
				}
			}
			if claims["healthy"] != 1 || claims["failed"] != 0 {
				t.Fatalf("claims: %+v", claims)
			}
			if status == 0 {
				return
			}
			failed.Store(false)
			client, err := hubclient.New(hubclient.Config{URL: server.URL, IdentityFile: identityPath, HTTPClient: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			projects := &runnerProjects{cfg: cfg, client: client, setup: project.NewRunnerSetup(file.Identity.RunnerID, nil, slog.Default()), logger: slog.Default()}
			run := projects.startPreparation(t.Context(), cfg, cfg.Projects[0], false)
			t.Cleanup(func() { run.cancel(); <-run.done })
			<-run.done
			if run.err != nil || run.checks.Configuration != "passed" {
				t.Fatalf("observation retry: %+v, %v", run.checks, run.err)
			}
			if err := scheduler.SetNativeProjects("org_example", map[string]tracker.ProjectID{"healthy": "prj_healthy", "failed": "prj_failed"}, map[string]runnerauth.LocalChecks{"healthy": checks["healthy"], "failed": run.checks}); err != nil {
				t.Fatal(err)
			}
			issues, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "failed", Policy: descriptor, ProviderRequirement: func(context.Context, connector.Issue, []providercapacity.Report) (providercapacity.Requirement, error) {
				return providercapacity.Requirement{Backend: "codex", Model: "provider_default"}, nil
			}})
			if err != nil || len(issues) != 1 || claims["failed"] != 1 {
				t.Fatalf("claim after successful observation: %v, %v, claims=%v", issues, err, claims)
			}
		})
	}
}
