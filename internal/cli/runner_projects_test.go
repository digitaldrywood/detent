package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	workspacepkg "github.com/digitaldrywood/detent/internal/workspace"
)

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
					workflow := "---\ntracker:\n  kind: memory\n  repository: acme/later\nhooks:\n  runner_setup: setup.sh\n  shell: sh\n---\nRun work.\n"
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
					if err := p.refresh(t.Context(), scheduler); err != nil {
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
			if err := p.refresh(t.Context(), scheduler); err != nil {
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
				if err := p.refresh(t.Context(), scheduler); err != nil {
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
		name   string
		legacy bool
	}{{name: "machine only"}, {name: "omitted root with external retained checkpoint", legacy: true}} {
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
				definition := "schema: 1\ntracker:\n  kind: memory\n  repository: acme/" + id + "\npolling:\n  interval_ms: 3210\nworkspace:\n  root: .detent/workspaces\n  source_root: .\n"
				if test.legacy {
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
			if test.legacy {
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
			if test.legacy {
				raw := mustRead(t, path)
				lines := strings.Split(raw, "\n")
				lines = slices.DeleteFunc(lines, func(line string) bool { return strings.HasPrefix(line, "workspace_root:") })
				raw = strings.Join(lines, "\n") + "\nprojects:\n  - id: orders\n    workdir: " + legacy + "\n    workflow: /ignored/private/WORKFLOW.md\n    weight: invalid\n    priority: invalid\n"
				if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
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
				opts.ghAuthToken = func(context.Context) (string, error) {
					return "", errors.New("gh auth status failed: exit status 1")
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
					if test.legacy && selected.ID != "orders" {
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
		name   string
		status int
		kept   bool
	}{
		{name: "hub unavailable keeps the running project", status: http.StatusServiceUnavailable, kept: true},
		{name: "project refusal drops the project", status: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/capabilities") {
					_ = json.NewEncoder(w).Encode(map[string]any{"protocol_majors": []int{2}, "event_schema_versions": []int{1}})
					return
				}
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": "unavailable", "message": "unavailable"})
			}))
			t.Cleanup(server.Close)
			client, err := hubclient.New(hubclient.Config{URL: server.URL + "/organizations/org_example", TokenSource: func() string { return "test" }})
			if err != nil {
				t.Fatal(err)
			}
			running := globalconfig.Project{ID: "orders", Workflow: "/work/orders/WORKFLOW.md", Workdir: "/work/orders"}
			cfg := globalconfig.Config{Projects: []globalconfig.Project{running}}
			cfg.Client.OrganizationID = "org_example"
			cfg.Client.NativeProjects = map[string]string{"orders": "prj_orders"}
			got, err := runnerProjectsForIdentity(t.Context(), cfg, client, runnerauth.Identity{OrganizationID: "org_example", ProjectIDs: []tracker.ProjectID{"prj_orders"}}, false)
			if err != nil {
				t.Fatal(err)
			}
			if kept := slices.ContainsFunc(got.Projects, func(project globalconfig.Project) bool {
				return project.ID == running.ID && project.Workdir == running.Workdir
			}); kept != test.kept {
				t.Fatalf("projects=%+v, kept=%v want %v", got.Projects, kept, test.kept)
			}
			if mapped := got.Client.NativeProjects["orders"] == "prj_orders"; mapped != test.kept {
				t.Fatalf("native projects=%v", got.Client.NativeProjects)
			}
		})
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
				cfg, err = runnerProjectsForIdentity(t.Context(), cfg, client, identity, prepare)
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
			cfg, err = runnerProjectsForIdentity(t.Context(), cfg, client, identity, true)
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
