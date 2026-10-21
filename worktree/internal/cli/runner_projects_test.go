package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	schedulerpkg "github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/tracker"
	workspacepkg "github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerProjectObservationDue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	signedOut := runnerauth.LocalChecks{Checkout: "passed", Setup: "passed", Doctor: "passed", Provider: "failed", ProviderKinds: []string{"codex"}}
	ready := runnerauth.LocalChecks{Checkout: "passed", Setup: "passed", Doctor: "passed", Provider: "passed", ProviderKinds: []string{"codex"}}
	for _, test := range []struct {
		name              string
		previous          runnerauth.LocalChecks
		reported          bool
		observedAgo       time.Duration
		prepare, observed bool
	}{
		{"unreported project prepares", runnerauth.LocalChecks{}, false, 0, true, false},
		{"failed checkout prepares again", runnerauth.LocalChecks{Checkout: "failed", Setup: "failed", Doctor: "pending", Provider: "pending"}, true, 0, true, false},
		{"signed-out provider is re-observed after console login", signedOut, true, runnerReadinessRecheck, false, true},
		{"recent signed-out observation waits", signedOut, true, time.Second, false, false},
		{"ready project is not re-observed", ready, true, time.Hour, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepare, observe := runnerProjectObservationDue(test.previous, test.reported, now.Add(-test.observedAgo), now)
			if prepare != test.prepare || observe != test.observed {
				t.Fatalf("prepare=%t observe=%t, want %t %t", prepare, observe, test.prepare, test.observed)
			}
		})
	}
}

func TestRunnerProjectsAssignmentRefresh(t *testing.T) {
	if testing.Short() {
		t.Skip("git checkout and setup-hook integration")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX setup hook")
	}
	t.Parallel()
	for _, test := range []struct {
		name         string
		cloneFailure bool
		setupFailure bool
		unbound      bool
	}{
		{name: "new assignment"}, {name: "one clone fails", cloneFailure: true}, {name: "one setup hook fails", setupFailure: true}, {name: "wait for repository binding", unbound: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_orders": "orders"})
			hub.features = []string{tracker.NativeProjectCheckoutCapability}
			hub.cloneURLs = map[tracker.ProjectID]string{"prj_later": "https://github.com/acme/later.git"}
			root := t.TempDir()
			configPath := filepath.Join(root, "config", "global.yaml")
			workspaceRoot := filepath.Join(root, "checkouts")
			checkout(t, filepath.Join(workspaceRoot, "orders"))
			runDoctorWorkflowSourceGit(t, filepath.Join(workspaceRoot, "orders"), "remote", "set-url", "origin", "https://github.com/acme/prj_orders.git")
			runDoctorWorkflowSourceGit(t, filepath.Join(workspaceRoot, "orders"), "config", "url."+filepath.Join(workspaceRoot, "orders")+".insteadOf", "https://github.com/acme/prj_orders.git")
			_, err := runRegisterInTestWorkspace(t, nil, func(*cobra.Command, string) error { return nil }, "--url", hub.server.URL+"/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", configPath, "--workspace-root", workspaceRoot)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := readRunnerRuntimeConfig(t.Context(), configPath)
			if err != nil {
				t.Fatal(err)
			}
			client, err := hubclient.New(hubclient.Config{URL: cfg.Client.URL, IdentityFile: cfg.Client.IdentityFile})
			if err != nil {
				t.Fatal(err)
			}
			localStore, err := store.Open(t.Context(), store.Config{Path: filepath.Join(root, "runner.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := localStore.Close(); err != nil {
					t.Error(err)
				}
			})
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			t.Cleanup(func() {
				if t.Failed() {
					t.Log(logs.String())
				}
			})
			applied := cfg
			p := &runnerProjects{client: client, cfg: cfg, setup: project.NewRunnerSetup("test-runner", localStore, logger), logger: logger, checks: make(map[string]runnerauth.LocalChecks), apply: func(_ context.Context, next globalconfig.Config) error { applied = next; return nil }}
			source := filepath.Join(root, "source")
			checkout(t, source)
			cloneCalls := 0
			p.checkout = func(ctx context.Context, selected globalconfig.Project, url string) error {
				return prepareRunnerCheckoutWithClone(ctx, selected, url, func(ctx context.Context, _ string, target string) error {
					cloneCalls++
					if test.cloneFailure && cloneCalls == 1 {
						return &runnerCheckoutError{message: "Cannot clone " + url, fix: "gh auth login"}
					}
					if err := os.CopyFS(target, os.DirFS(source)); err != nil {
						return err
					}
					runGit := func(args ...string) error {
						cmd := exec.CommandContext(ctx, "git")
						cmd.Dir = target
						cmd.Args = append(cmd.Args, args...)
						output, err := cmd.CombinedOutput()
						if err != nil {
							return fmt.Errorf("prepare fixture repository: %w: %s", err, output)
						}
						return nil
					}
					if err := runGit("remote", "set-url", "origin", url); err != nil {
						return err
					}
					if err := runGit("config", "url."+selected.Workdir+".insteadOf", url); err != nil {
						return err
					}
					workflow := "---\ntracker:\n  kind: hub_native\n  repository: acme/later\nhooks:\n  runner_setup: setup.sh\n  shell: sh\n---\nRun work.\n"
					if err := os.WriteFile(filepath.Join(target, "WORKFLOW.md"), []byte(workflow), 0600); err != nil {
						return err
					}
					script := "printf 'setup\\n' >> trace\n"
					if test.setupFailure {
						script = "exit 7\n"
					}
					if err := os.WriteFile(filepath.Join(target, "setup.sh"), []byte(script), 0600); err != nil {
						return err
					}
					if err := runGit("add", "WORKFLOW.md", "setup.sh"); err != nil {
						return err
					}
					if err := runGit("-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "setup definition"); err != nil {
						return err
					}
					return nil
				})
			}
			scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{OrganizationID: "org_example", NativeProjects: map[string]tracker.ProjectID{"orders": "prj_orders"}, Machine: hubclient.Machine{ID: "host", Hostname: "host", Capacity: 2, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if declared := p.runnerSetupDeclared(t.Context(), "prj_later"); declared != nil {
				t.Fatalf("unassigned checkout declared setup: %v", declared)
			}
			hub.projects["prj_later"] = "Later"
			hub.repositories["prj_later"] = "acme/later"
			identity := hub.identity.Load().(runnerauth.Identity)
			identity.ProjectIDs = append(identity.ProjectIDs, "prj_later")
			hub.identity.Store(identity)
			if test.unbound {
				delete(hub.repositories, "prj_later")
				delete(hub.cloneURLs, "prj_later")
				for range 2 {
					p.lastRefresh = time.Time{}
					if err := refreshRunnerProjects(t, p, scheduler); err != nil {
						t.Fatal(err)
					}
					if cloneCalls != 0 || slices.ContainsFunc(applied.Projects, func(selected globalconfig.Project) bool { return selected.ID == "prj_later" }) {
						t.Fatal("unbound project entered checkout or startup")
					}
				}
				hub.repositories["prj_later"] = "acme/later"
				hub.cloneURLs["prj_later"] = "https://github.com/acme/later.git"
				p.lastRefresh = time.Time{}
			}
			if err := refreshRunnerProjects(t, p, scheduler); err != nil {
				t.Fatal(err)
			}
			if p.checks["orders"].Setup != "passed" {
				t.Fatalf("ready project blocked: %+v", p.checks)
			}
			failed := test.cloneFailure || test.setupFailure
			if (p.checks["prj_later"].Setup == "failed") != failed {
				t.Fatalf("new project setup: %+v", p.checks)
			}
			if test.cloneFailure && p.checks["prj_later"].CheckoutFix != "gh auth login" {
				t.Fatalf("missing authentication command: %+v", p.checks["prj_later"])
			}
			if _, ok := scheduler.ConnectorForProject("prj_later"); !ok {
				t.Fatal("new project was not assigned to scheduler")
			}
			later := filepath.Join(workspaceRoot, "prj_later")
			if test.setupFailure {
				if err := os.WriteFile(filepath.Join(later, "setup.sh"), []byte("printf 'setup\\n' >> trace\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runDoctorWorkflowSourceGit(t, later, "add", "setup.sh")
				runDoctorWorkflowSourceGit(t, later, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "repair setup")
			}
			for range 2 {
				p.lastRefresh = time.Time{}
				if err := refreshRunnerProjects(t, p, scheduler); err != nil {
					t.Fatal(err)
				}
			}
			wantClones := 1
			if test.cloneFailure {
				wantClones = 2
			}
			if cloneCalls != wantClones || p.checks["prj_later"].Setup != "passed" || p.checks["prj_later"].CheckoutMessage != "" || applied.Client.NativeProjects["prj_later"] != "prj_later" {
				t.Fatalf("resume: clones %d, checks %+v, configuration %+v", cloneCalls, p.checks, applied.Client.NativeProjects)
			}
			trace, err := os.ReadFile(filepath.Join(later, "trace"))
			if err != nil || strings.Count(string(trace), "\n") != 1 {
				t.Fatalf("hook did not run exactly once: %q %v", trace, err)
			}
			if declared := p.runnerSetupDeclared(t.Context(), "prj_later"); declared == nil || !*declared {
				t.Fatalf("assigned checkout setup declaration = %v", declared)
			}
			workflowPath := filepath.Join(later, "WORKFLOW.md")
			workflow, err := os.ReadFile(workflowPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(workflowPath, []byte(strings.Replace(string(workflow), "  runner_setup: setup.sh\n", "", 1)), 0600); err != nil {
				t.Fatal(err)
			}
			runDoctorWorkflowSourceGit(t, later, "add", "WORKFLOW.md")
			runDoctorWorkflowSourceGit(t, later, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "remove setup declaration")
			if declared := p.runnerSetupDeclared(t.Context(), "prj_later"); declared == nil || *declared {
				t.Fatalf("removed setup declaration = %v", declared)
			}
			saved, err := readRunnerSetupConfig(configPath)
			if err != nil || len(saved.Projects) != 0 {
				t.Fatalf("operator config was rewritten: %+v %v", saved, err)
			}
		})
	}
}

func TestRunnerCloudProjects(t *testing.T) {
	for _, test := range []struct {
		name               string
		externalWorkspaces bool
		noInstallationAuth bool
	}{{name: "machine only"}, {name: "external retained checkpoint", externalWorkspaces: true}, {name: "unavailable installation authentication", noInstallationAuth: true}} {
		t.Run(test.name, func(t *testing.T) {
			hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_orders": "Orders", "prj_tools": "Tools", "prj_unconfigured": "Unconfigured"})
			hub.repositories["prj_unconfigured"] = ""
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			sources := filepath.Join(root, "sources")
			for _, id := range []string{"prj_orders", "prj_tools"} {
				checkout(t, filepath.Join(sources, id+".git"))
				definition := "schema: 1\ntracker:\n  kind: hub_native\n  repository: acme/" + id + "\npolling:\n  interval_ms: 3210\nworkspace:\n  root: .detent/workspaces\n  source_root: .\n"
				if test.externalWorkspaces {
					definition = strings.Split(definition, "workspace:")[0]
				}
				if err := os.WriteFile(filepath.Join(sources, id+".git", "detent.yaml"), []byte(definition), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(sources, id+".git", "WORKFLOW.md"), []byte("Work the issue.\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runDoctorWorkflowSourceGit(t, filepath.Join(sources, id+".git"), "add", "detent.yaml", "WORKFLOW.md")
				runDoctorWorkflowSourceGit(t, filepath.Join(sources, id+".git"), "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "project definition")
			}
			t.Setenv("GIT_CONFIG_COUNT", "2")
			t.Setenv("GIT_CONFIG_KEY_0", "url."+sources+string(os.PathSeparator)+".insteadOf")
			t.Setenv("GIT_CONFIG_VALUE_0", "git@github.com:acme/")
			t.Setenv("GIT_CONFIG_KEY_1", "url."+sources+string(os.PathSeparator)+".insteadOf")
			t.Setenv("GIT_CONFIG_VALUE_1", "https://github.com/acme/")
			path := filepath.Join(root, "runner", "global.yaml")
			workspace := filepath.Join(root, "checkouts")
			legacy := filepath.Join(workspace, "orders")
			if err := cloneRunnerRepository(t.Context(), "git@github.com:acme/prj_orders.git", legacy); err != nil {
				t.Fatal(err)
			}
			retainedIssue := workspacepkg.Issue{ProjectID: "orders", ID: "wi_retained", Identifier: "prj_orders#7"}
			retainedRoot := filepath.Join(legacy, ".detent/workspaces")
			if test.externalWorkspaces {
				retainedRoot = filepath.Join(root, "external-workspaces", "orders")
			}
			retained, err := workspacepkg.NewBackend(workspacepkg.KindLocalGit, workspacepkg.LocalGitOptions{Root: retainedRoot, SourceRoot: legacy, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := retained.Create(t.Context(), retainedIssue)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(checkpoint.Path, "unfinished.txt"), []byte("retained source\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := runRegisterInTestWorkspace(t, nil, nil, "--url", hub.server.URL+"/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", path, "--workspace-root", workspace); err != nil {
				t.Fatal(err)
			}
			before := mustRead(t, path)
			client, err := hubclient.New(hubclient.Config{URL: hub.server.URL + "/organizations/org_example", IdentityFile: filepath.Join(filepath.Dir(path), "identity.json")})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{HeartbeatInterval: 30 * time.Second, LeaseTTL: 90 * time.Second, OrganizationID: "org_example", Machine: hubclient.Machine{ID: hub.identity.Load().(runnerauth.Identity).MachineID, Hostname: "host", Capacity: 2, Version: "test"}})
			if err != nil {
				t.Fatal(err)
			}
			for _, ids := range [][]tracker.ProjectID{{"prj_orders"}, {"prj_orders", "prj_tools", "prj_unconfigured"}, {"prj_tools"}, {"prj_unconfigured"}, {}} {
				identity := hub.identity.Load().(runnerauth.Identity)
				identity.ProjectIDs = ids
				hub.identity.Store(identity)
				opts := defaultOptions()
				opts.read = func(path string) (globalconfig.Config, error) {
					cfg, err := globalconfig.Read(path, globalconfig.WithHome(root))
					if err != nil {
						return cfg, err
					}
					return resolveRunnerProjects(t.Context(), cfg)
				}
				ghCalls := 0
				opts.ghAuthToken = func(context.Context) (string, error) {
					ghCalls++
					if test.noInstallationAuth {
						return "", errors.New("not logged in")
					}
					return "configured-token", nil
				}
				opts.lookupEnv = func(name string) string {
					if name == "GITHUB_TOKEN" || name == "GH_TOKEN" {
						return ""
					}
					return os.Getenv(name)
				}
				boot, err := resolveBootConfigWithRuntimeDeps(t.Context(), path, "", runtimeFlags{}, opts, bootRuntimeDeps(opts), true)
				if err != nil {
					t.Fatal(err)
				}
				wantToken := "configured-token"
				if test.noInstallationAuth {
					wantToken = ""
				}
				if ghCalls != 1 || boot.Runtime.GitHubToken.Value != wantToken {
					t.Fatalf("startup authentication: resolver calls=%d, token present=%v", ghCalls, boot.Runtime.GitHubToken.Value != "")
				}
				cfg := boot.Global
				if err := scheduler.SetNativeProjects("org_example", nativeProjectsFromConfig(cfg.Client.NativeProjects), nil); err != nil {
					t.Fatal(err)
				}
				for _, id := range []tracker.ProjectID{"prj_orders", "prj_tools", "prj_unconfigured"} {
					name := string(id)
					if id == "prj_orders" {
						name = "orders"
					}
					_, ok := scheduler.ConnectorForProject(name)
					if ok != slices.Contains(ids, id) {
						t.Fatalf("scheduler connector for %s = %v, allowed %v", id, ok, ids)
					}
				}
				var got []tracker.ProjectID
				for _, selected := range project.ManagerConfigFromGlobal(cfg).Projects {
					got = append(got, tracker.ProjectID(cfg.Client.NativeProjects[selected.ID]))
					connector, ok := scheduler.ConnectorForProject(selected.ID)
					if !ok {
						t.Fatalf("missing native connector for %s", selected.ID)
					}
					candidates, err := connector.FetchCandidateIssues(t.Context())
					if err != nil || len(candidates) != 1 || candidates[0].ID != "wi_"+cfg.Client.NativeProjects[selected.ID] {
						t.Fatalf("Cloud candidates for %s = %+v, %v", selected.ID, candidates, err)
					}
					expectedWorkdir := filepath.Join(workspace, selected.ID)
					if test.externalWorkspaces && selected.ID != "orders" {
						expectedWorkdir = filepath.Join(cfg.WorkspaceRoot, selected.ID)
					}
					if selected.Workdir != expectedWorkdir || selected.WorkflowRef != "origin/HEAD" || selected.GlobalCache != cfg.Global.Cache.Normalized() {
						t.Fatalf("project = %+v, mapping = %v", selected, cfg.Client.NativeProjects)
					}
					workflow, err := project.LoadWorkflowContext(t.Context(), selected)
					if err != nil {
						t.Fatal(err)
					}
					if workflow.Config.Polling.IntervalMS != 3210 {
						t.Fatalf("repository detent.yaml was not loaded: %+v", workflow.Config.Polling)
					}
					if selected.ID == "orders" {
						resumedRoot := workflow.Config.Workspace.Root
						if !filepath.IsAbs(resumedRoot) {
							resumedRoot = filepath.Join(selected.Workdir, resumedRoot)
						}
						resumed, err := workspacepkg.NewBackend(workspacepkg.KindLocalGit, workspacepkg.LocalGitOptions{Root: resumedRoot, SourceRoot: selected.Workdir, AutoBranch: true})
						if err != nil {
							t.Fatal(err)
						}
						issue := retainedIssue
						issue.ProjectID = selected.ID
						existing, err := resumed.(*workspacepkg.LocalGit).Existing(issue)
						if err != nil || existing.Path != checkpoint.Path || existing.Branch != checkpoint.Branch || mustRead(t, filepath.Join(existing.Path, "unfinished.txt")) != "retained source\n" {
							t.Fatalf("retained checkpoint = %+v, %v", existing, err)
						}
						source := filepath.Join(sources, "prj_orders.git")
						if err := os.WriteFile(filepath.Join(source, "WORKFLOW.md"), []byte("Current repository prompt.\n"), 0o600); err != nil {
							t.Fatal(err)
						}
						runDoctorWorkflowSourceGit(t, source, "add", "WORKFLOW.md")
						runDoctorWorkflowSourceGit(t, source, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "update project prompt")
						current, err := project.LoadWorkflowContext(t.Context(), selected)
						if err != nil || strings.TrimSpace(current.Prompt) != "Current repository prompt." || current.Definition.Revision == workflow.Definition.Revision {
							t.Fatalf("current workflow prompt = %q, revision = %s, %v", current.Prompt, current.Definition.Revision, err)
						}
					}
				}
				ready := slices.DeleteFunc(slices.Clone(ids), func(id tracker.ProjectID) bool { return id == "prj_unconfigured" })
				if !slices.Equal(got, ready) {
					t.Fatalf("projects = %v, Cloud allows %v", got, ids)
				}
				if slices.Contains(ids, "prj_unconfigured") {
					checks := collectRunnerLocalChecks(t.Context(), cfg, "prj_unconfigured", nil, nil)
					if checks.Checkout != "failed" || cfg.Client.NativeProjects["prj_unconfigured"] != "prj_unconfigured" {
						t.Fatalf("unconfigured project lost failure/grant: %+v, %v", checks, cfg.Client.NativeProjects)
					}
				}
				if mustRead(t, path) != before {
					t.Fatal("discovery rewrote machine settings")
				}
				fileConfig, err := globalconfig.Read(path)
				if err != nil || len(fileConfig.Projects) != 0 || len(fileConfig.Client.NativeProjects) != 0 {
					t.Fatalf("file retained Cloud policy: %+v, %v", fileConfig, err)
				}
			}
		})
	}
}

func TestRunnerProjectsKeepAssignedProjectThroughHubOutage(t *testing.T) {
	for _, test := range []struct {
		name                string
		status              int
		kept                bool
		revoked             bool
		identityUnavailable bool
		blockedCheckout     bool
	}{
		{name: "blocked checkout keeps the running project live", kept: true, blockedCheckout: true},
		{name: "hub unavailable keeps the running project", status: http.StatusServiceUnavailable, kept: true},
		{name: "identity unavailable keeps the runtime unapplied", kept: true, identityUnavailable: true},
		{name: "project unauthorized drops the project", status: http.StatusUnauthorized},
		{name: "project forbidden drops the project", status: http.StatusForbidden},
		{name: "project not found drops the project", status: http.StatusNotFound},
		{name: "routing revocation drops the project", status: http.StatusServiceUnavailable, revoked: true},
	} {
		for _, periodic := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/periodic=%v", test.name, periodic), func(t *testing.T) {
				var identity runnerauth.Identity
				var identityReads, projectReads, legacyReads atomic.Int32
				var reloading atomic.Bool
				var heartbeats atomic.Int32
				var removeLater atomic.Bool
				var checkoutReport atomic.Value
				var checkoutProblems atomic.Value
				descriptor := policy.Descriptor{
					SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
					Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
				}.WithID()
				lease := tracker.NativeLease{ID: "lease_example", WorkItemID: "wi_example", PolicyID: descriptor.ID, FencingToken: 7, ServerTime: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch {
					case strings.HasSuffix(r.URL.Path, "/capabilities"):
						_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}, "features": []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeProjectCheckoutCapability, tracker.NativeLocalChecksCapability}})
					case strings.HasSuffix(r.URL.Path, "/runners/"+identity.RunnerID):
						if reloading.Load() && !test.blockedCheckout && (identityReads.Add(1) > 1 || test.identityUnavailable) {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						response := identity
						if test.blockedCheckout && reloading.Load() && !removeLater.Load() {
							response.ProjectIDs = append(slices.Clone(identity.ProjectIDs), "prj_later")
						}
						if test.revoked && reloading.Load() {
							response.ProjectIDs = nil
						}
						_ = json.NewEncoder(w).Encode(response)
					case strings.HasSuffix(r.URL.Path, "/projects/prj_later"):
						_ = json.NewEncoder(w).Encode(tracker.NativeProject{ID: "prj_later", OrganizationID: "org_example", Profile: "native", Repository: "example-org/later", CloneURL: "https://github.com/example-org/later.git"})
					case strings.HasSuffix(r.URL.Path, "/projects/prj_orders"):
						if !reloading.Load() || test.blockedCheckout {
							_ = json.NewEncoder(w).Encode(tracker.NativeProject{ID: "prj_orders", OrganizationID: "org_example", Profile: "native", Repository: "example-org/orders", States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
							return
						}
						projectReads.Add(1)
						w.WriteHeader(test.status)
						_ = json.NewEncoder(w).Encode(map[string]string{"code": http.StatusText(test.status)})
					case strings.HasSuffix(r.URL.Path, "/projects/prj_orders/policy"):
						_ = json.NewEncoder(w).Encode(policy.Approval{Policy: descriptor})
					case test.blockedCheckout && strings.HasSuffix(r.URL.Path, "/heartbeat"):
						var request struct {
							LocalChecks runnerauth.LocalChecks `json:"local_checks"`
							Problems    []runnerauth.Problem   `json:"problems"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						if err := request.LocalChecks.Validate(); err != nil {
							http.Error(w, err.Error(), http.StatusUnprocessableEntity)
							return
						}
						if err := runnerauth.ValidateReportedProblems(request.Problems); err != nil {
							http.Error(w, err.Error(), http.StatusUnprocessableEntity)
							return
						}
						if strings.Contains(r.URL.Path, "/projects/prj_later/") {
							checkoutReport.Store(request.LocalChecks)
							checkoutProblems.Store(request.Problems)
						}
						heartbeats.Add(1)
						_ = json.NewEncoder(w).Encode(runnerauth.RoutingSnapshot{RunnerID: identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Example runner", State: "active", CapacityLimit: 2, ProjectIDs: identity.ProjectIDs, IsolationTier: "sandbox"}})
					case strings.HasSuffix(r.URL.Path, "/policy/observed"), strings.HasSuffix(r.URL.Path, "/machines/register"), strings.HasSuffix(r.URL.Path, "/machines/"+string(identity.MachineID)+"/heartbeat"):
						heartbeats.Add(1)
						w.WriteHeader(http.StatusNoContent)
					case strings.HasSuffix(r.URL.Path, "/claims"), strings.HasSuffix(r.URL.Path, "/leases/lease_example/renew"):
						_ = json.NewEncoder(w).Encode(lease)
					case strings.HasSuffix(r.URL.Path, "/leases/lease_example/validate"):
						_ = json.NewEncoder(w).Encode(runnerauth.Runner{})
					case strings.HasSuffix(r.URL.Path, "/work-items/wi_example"):
						issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: "org_example", ProjectID: "prj_orders", WorkItemID: "wi_example"}, State: "Todo", SchedulingEligible: new(true), DependencyReady: true}
						if r.URL.Query().Get("view") == "recovery" {
							_ = json.NewEncoder(w).Encode(tracker.NativeRecovery{Issue: issue})
						} else {
							_ = json.NewEncoder(w).Encode(issue)
						}
					default:
						if strings.Contains(r.URL.Path, "/repositories/") {
							legacyReads.Add(1)
						} else {
							t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						}
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(server.Close)
				root := t.TempDir()
				if err := os.Chmod(root, 0o700); err != nil {
					t.Fatal(err)
				}
				identityPath := filepath.Join(root, "identity.json")
				file, err := runnerauth.Initialize(identityPath, server.URL)
				if err != nil {
					t.Fatal(err)
				}
				file.Identity.OrganizationID = "org_example"
				file.Identity.ProjectIDs = []tracker.ProjectID{"prj_orders"}
				file.Identity.Operations = []string{runnerauth.Read}
				file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
				identity = file.Identity
				if err := runnerauth.Save(identityPath, file); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(root, "global.yaml")
				machine := reloadTestConfig(path, 2, nil)
				machine.WorkspaceRoot = filepath.Join(root, "checkouts")
				machine.Client.URL, machine.Client.OrganizationID, machine.Client.IdentityFile = server.URL, "org_example", identityPath
				raw := "apiVersion: detent/v1\nkind: GlobalConfig\nworkspace_root: " + machine.WorkspaceRoot + "\nclient:\n  hub_url: " + server.URL + "\n  organization_id: org_example\n  identity_file: " + identityPath + "\n  capacity: 2\n"
				if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
					t.Fatal(err)
				}
				current, err := globalconfig.Read(path)
				if err != nil {
					t.Fatal(err)
				}
				if len(current.Projects) != 0 || len(current.Client.NativeProjects) != 0 {
					t.Fatal("disk config is not machine-only")
				}
				files := createBootProjectFiles(t)
				if test.blockedCheckout {
					checkout(t, files.workdirPath)
					runDoctorWorkflowSourceGit(t, files.workdirPath, "remote", "set-url", "origin", "https://github.com/example-org/orders.git")
				}
				running := globalconfig.Project{ID: "orders", Workflow: files.workflowPath, Workdir: files.workdirPath, Weight: 1}
				current.Projects = []globalconfig.Project{running}
				current.Client.NativeProjects = map[string]string{"orders": "prj_orders"}
				client, err := hubclient.New(hubclient.Config{URL: server.URL, TokenSource: func() string { return "test" }})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{
					OrganizationID: "org_example", NativeProjects: map[string]tracker.ProjectID{"orders": "prj_orders"},
					Machine: hubclient.Machine{ID: identity.MachineID, Hostname: "host", Capacity: 2, Version: "test"}, HeartbeatInterval: time.Minute, LeaseTTL: time.Hour,
					LeaseHold: func(context.Context) (func(), error) { return func() {}, nil },
				})
				if err != nil {
					t.Fatal(err)
				}
				issues, err := scheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{ProjectID: "orders", Repository: "example-org/orders", Policy: descriptor})
				if err != nil || len(issues) != 1 {
					t.Fatalf("initial native claim = %+v, %v", issues, err)
				}
				if err := os.WriteFile(path, []byte(strings.Replace(raw, "capacity: 2", "capacity: 3", 1)), 0o600); err != nil {
					t.Fatal(err)
				}
				reloading.Store(true)
				if test.blockedCheckout {
					logger := slog.New(slog.NewTextHandler(io.Discard, nil))
					client, err = hubclient.New(hubclient.Config{URL: server.URL, IdentityFile: identityPath})
					if err != nil {
						t.Fatal(err)
					}
					p := &runnerProjects{client: client, cfg: current, setup: project.NewRunnerSetup("runner", nil, logger), logger: logger, checks: map[string]runnerauth.LocalChecks{"orders": {Checkout: "passed", Setup: "passed", Doctor: "passed", Provider: "passed"}}}
					started := make(chan struct{})
					cloneCtx, cancelClone := context.WithCancel(t.Context())
					defer cancelClone()
					p.checkout = func(ctx context.Context, _ globalconfig.Project, _ string) error {
						close(started)
						select {
						case <-cloneCtx.Done():
						case <-ctx.Done():
						}
						return newRunnerCheckoutError("https://github.com/example-org/later.git", errors.New("git transfer stopped"), context.DeadlineExceeded, "transfer stalled\n"+strings.Repeat("remote: transfer stalled\n", 100))
					}
					refreshDone := make(chan error, 1)
					go func() { refreshDone <- p.refresh(t.Context(), scheduler) }()
					select {
					case <-started:
					case <-time.After(5 * time.Second):
						select {
						case err := <-refreshDone:
							t.Fatalf("checkout did not start: %v", err)
						default:
							t.Fatal("checkout did not start")
						}
					}
					ticks := make(chan struct{}, 2)
					orch, err := orchestrator.New(orchestrator.Config{
						Project: schedulerpkg.ProjectCandidate{ID: "orders"}, PollInterval: 10 * time.Millisecond,
					}, orchestrator.Dependencies{Connector: fakehub.New(fakehub.Config{}), Logger: logger,
						Scheduling: runnerProjectTickScheduling{SchedulingSource: scheduler, projects: p, ticks: ticks},
					})
					if err != nil {
						t.Fatal(err)
					}
					tickCtx, cancelTicks := context.WithCancel(t.Context())
					tickDone := make(chan error, 1)
					go func() { tickDone <- orch.Run(tickCtx) }()
					defer func() { cancelClone(); cancelTicks(); <-tickDone }()
					for range 2 {
						select {
						case <-ticks:
						case <-time.After(2 * time.Second):
							t.Fatal("blocked checkout stopped healthy project ticks")
						}
						if _, err := scheduler.RenewClaim(t.Context(), "wi_example", time.Now()); err != nil {
							t.Fatal(err)
						}
					}
					select {
					case err := <-refreshDone:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("blocked checkout stopped assignment refresh")
					}
					heartbeatScheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{
						OrganizationID: "org_example", NativeProjects: map[string]tracker.ProjectID{"orders": "prj_orders"},
						Machine:           hubclient.Machine{ID: identity.MachineID, Hostname: "host", Capacity: 2, Version: "test"},
						HeartbeatInterval: time.Nanosecond, LeaseTTL: time.Hour, RefreshProjects: p.refresh,
						Problems: p.reportedProblems,
					})
					if err != nil {
						t.Fatal(err)
					}
					for range 2 {
						before := heartbeats.Load()
						p.lastRefresh = time.Time{}
						if err := heartbeatScheduler.Heartbeat(t.Context()); err != nil {
							t.Fatal(err)
						}
						if heartbeats.Load() <= before {
							t.Fatal("machine heartbeat did not reach Hub during checkout")
						}
						if _, err := scheduler.RenewClaim(t.Context(), "wi_example", time.Now()); err != nil {
							t.Fatal(err)
						}
					}
					manager := &globalReloadManager{}
					reloader := &globalConfigReloader{current: current, manager: manager}
					if periodic {
						syncLatestGlobalConfig(t.Context(), path, reloader)
					} else {
						reloader.handle(t.Context(), configwatcher.FileUpdate[globalconfig.Config]{Path: path, Value: machine})
					}
					if len(manager.config.Projects) != 1 || manager.config.Projects[0].ID != "orders" {
						t.Fatalf("reload admitted unprepared project or lost healthy project: %+v", manager.config.Projects)
					}
					p.mu.Lock()
					run := p.preparing["prj_later"]
					p.mu.Unlock()
					if periodic {
						removeLater.Store(true)
						p.lastRefresh = time.Time{}
						if err := heartbeatScheduler.Heartbeat(t.Context()); err != nil {
							t.Fatal(err)
						}
					} else {
						cancelClone()
					}
					select {
					case <-run.done:
					case <-time.After(5 * time.Second):
						t.Fatal("checkout did not stop after timeout or revocation")
					}
					if !periodic {
						if !errors.Is(run.err, context.DeadlineExceeded) {
							t.Fatalf("checkout lost timeout cause: %v", run.err)
						}
						p.lastRefresh = time.Time{}
						if err := heartbeatScheduler.Heartbeat(t.Context()); err != nil {
							t.Fatal(err)
						}
						checks, ok := checkoutReport.Load().(runnerauth.LocalChecks)
						if !ok || checks.Checkout != "failed" || !strings.Contains(checks.CheckoutMessage, "context deadline exceeded") || strings.Contains(checks.CheckoutFix, "gh auth") {
							t.Fatalf("checkout report lost cause: %+v", checks)
						}
						problems, _ := checkoutProblems.Load().([]runnerauth.Problem)
						if !slices.ContainsFunc(problems, func(problem runnerauth.Problem) bool {
							return problem.ProjectID == "prj_later" && problem.Subject == "checkout" && strings.Contains(problem.ErrorOutput, "context deadline exceeded") && strings.Contains(problem.ErrorOutput, "transfer stalled\n")
						}) {
							t.Fatalf("checkout problem lost detailed cause: %+v", problems)
						}
						removeLater.Store(true)
						p.lastRefresh = time.Time{}
						if err := heartbeatScheduler.Heartbeat(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					if _, ok := heartbeatScheduler.ConnectorForProject("prj_later"); ok {
						t.Fatal("revoked failing project remained assigned")
					}
					if len(p.reportedProblems()) != 0 {
						t.Fatal("revoked project retained checkout problems")
					}
					if _, err := scheduler.RenewClaim(t.Context(), "wi_example", time.Now()); err != nil {
						t.Fatal(err)
					}

					return
				}
				manager := &globalReloadManager{}
				reloader := &globalConfigReloader{current: current, manager: manager, applyRuntime: func(next globalconfig.Config) error {
					if test.kept && (len(next.Projects) == 0 || next.Client.NativeProjects["orders"] != "prj_orders") {
						t.Error("reload published a partial project set before manager reconciliation")
					}
					return scheduler.SetNativeProjects(tracker.OrganizationID(next.Client.OrganizationID), nativeProjectsFromConfig(next.Client.NativeProjects), nil)
				}}
				if periodic {
					syncLatestGlobalConfig(t.Context(), path, reloader)
				} else {
					reloader.handle(t.Context(), configwatcher.FileUpdate[globalconfig.Config]{Path: path, Value: machine})
				}
				if reads := identityReads.Load(); reads != 1 {
					t.Errorf("reload identity reads = %d, want 1", reads)
				}
				expectedCalls := 1
				if test.identityUnavailable {
					expectedCalls = 0
				}
				if manager.calls != expectedCalls {
					t.Errorf("manager reconciliations = %d, want %d", manager.calls, expectedCalls)
				}
				_, mapped := scheduler.ConnectorForProject("orders")
				_, renewErr := scheduler.RenewClaim(t.Context(), "wi_example", time.Now())
				if test.kept && (renewErr != nil || legacyReads.Load() != 0) {
					t.Errorf("existing native claim renewal = %v, legacy policy reads = %d", renewErr, legacyReads.Load())
				}
				if !test.kept && renewErr == nil {
					t.Error("revoked project claim remained usable")
				}
				managed := manager.config.Projects
				if test.identityUnavailable {
					managed = reloader.current.Projects
				}
				if kept := slices.ContainsFunc(managed, func(selected globalconfig.Project) bool {
					return selected.ID == running.ID && selected.Workdir == running.Workdir && selected.Workflow == running.Workflow
				}); kept != test.kept {
					t.Errorf("manager projects = %+v, kept = %v want %v", manager.config.Projects, kept, test.kept)
				}
				if mapped != test.kept || (reloader.current.Client.NativeProjects["orders"] == "prj_orders") != test.kept {
					t.Errorf("scheduler mapped = %v, native projects = %v, want retained = %v", mapped, reloader.current.Client.NativeProjects, test.kept)
				}
				if !test.revoked && !test.identityUnavailable && projectReads.Load() == 0 {
					t.Error("reload did not read the assigned project")
				}
			})
		}
	}
}

func TestRunnerProjectsNewRepositoryBinding(t *testing.T) {
	for _, test := range []struct {
		name, repository, origin string
		wantCheckout             bool
	}{
		{name: "new private repository", repository: "acme/private", wantCheckout: true},
		{name: "matching checkout is retained", repository: "acme/private", origin: "acme/private", wantCheckout: true},
		{name: "mismatched checkout is refused", repository: "acme/private", origin: "other/repository"},
	} {
		t.Run(test.name, func(t *testing.T) {
			hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_new": "new"})
			hub.repositories["prj_new"] = ""
			hub.credential.Store("test")
			root := t.TempDir()
			gitConfig := filepath.Join(root, "gitconfig")
			t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_COUNT", "0")
			t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(root, "absent-system-config"))
			source := filepath.Join(root, "source")
			checkout(t, source)
			if test.origin == "" {
				runDoctorWorkflowSourceGit(t, source, "rm", "WORKFLOW.md")
				runDoctorWorkflowSourceGit(t, source, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "repository without policy")
			}
			runDoctorWorkflowSourceGit(t, source, "config", "--file", gitConfig, "url."+filepath.ToSlash(source)+".insteadOf", "https://github.com/acme/private.git")
			client, err := hubclient.New(hubclient.Config{URL: hub.server.URL + "/organizations/org_example", TokenSource: func() string { return "test" }})
			if err != nil {
				t.Fatal(err)
			}
			cfg := globalconfig.Config{WorkspaceRoot: filepath.Join(root, "work")}
			cfg.Client.OrganizationID = "org_example"
			identity := runnerauth.Identity{OrganizationID: "org_example", ProjectIDs: []tracker.ProjectID{"prj_new"}}
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			for _, prepare := range []bool{true, false, false} {
				cfg, err = runnerProjectsForIdentity(t.Context(), cfg, client, identity, prepare, false)
				if err != nil || len(cfg.Projects) != 0 {
					t.Fatalf("waiting project starts (prepare=%v): %+v: %v", prepare, cfg.Projects, err)
				}
			}
			if output := logs.String(); !strings.Contains(output, "level=INFO") || !strings.Contains(output, "Waiting for a repository: bind one in project setup") || strings.Contains(output, "level=ERROR") || strings.Contains(output, "error_class=") {
				t.Fatalf("unbound project logs = %s", output)
			}
			if cfg.Client.NativeProjects["prj_new"] != "prj_new" {
				t.Fatalf("unbound project = %+v: %v", cfg, err)
			}
			target := filepath.Join(cfg.WorkspaceRoot, "prj_new")
			if test.origin != "" {
				checkout(t, target)
				runDoctorWorkflowSourceGit(t, target, "remote", "set-url", "origin", "https://github.com/"+test.origin+".git")
			}
			hub.repositories["prj_new"] = test.repository
			hub.cloneURLs = map[tracker.ProjectID]string{"prj_new": "https://github.com/" + test.repository + ".git"}
			cfg, err = runnerProjectsForIdentity(t.Context(), cfg, client, identity, true, false)
			if err != nil || (len(cfg.Projects) == 1) != test.wantCheckout {
				t.Fatalf("bound project checkouts = %+v: %v", cfg.Projects, err)
			}
			p := &runnerProjects{cfg: cfg}
			if reported := p.repository(t.Context(), "prj_new"); test.wantCheckout && reported != "acme/private" || !test.wantCheckout && reported != "" {
				t.Fatalf("checkout report = %q", reported)
			}
			if test.origin != "" && runnerCheckoutRepository(t.Context(), globalconfig.Project{Workdir: target}) != test.origin {
				t.Fatal("existing checkout origin changed")
			}
		})
	}
}

func refreshRunnerProjects(t *testing.T, p *runnerProjects, scheduler *hubclient.Scheduler) error {
	t.Helper()
	if err := p.refresh(t.Context(), scheduler); err != nil {
		return err
	}
	p.mu.Lock()
	var pending []<-chan struct{}
	for _, run := range p.preparing {
		pending = append(pending, run.done)
	}
	p.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-t.Context().Done():
			return t.Context().Err()
		}
	}
	p.mu.Lock()
	p.lastRefresh = time.Time{}
	p.mu.Unlock()
	return p.refresh(t.Context(), scheduler)
}

type runnerProjectTickScheduling struct {
	orchestrator.SchedulingSource
	projects *runnerProjects
	ticks    chan<- struct{}
}

func (s runnerProjectTickScheduling) FetchCandidateIssues(ctx context.Context, request orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	if err := s.projects.prepare(ctx, request.ProjectID); err != nil {
		return nil, err
	}
	select {
	case s.ticks <- struct{}{}:
	default:
	}
	return nil, nil
}
