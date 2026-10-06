package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	workspacepkg "github.com/digitaldrywood/detent/internal/workspace"
)

func TestRunnerCloudProjects(t *testing.T) {
	hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_orders": "Orders", "prj_tools": "Tools", "prj_unconfigured": "Unconfigured"})
	hub.repositories["prj_unconfigured"] = ""
	root := t.TempDir()
	sources := filepath.Join(root, "sources")
	for _, id := range []string{"prj_orders", "prj_tools"} {
		checkout(t, filepath.Join(sources, id+".git"))
		definition := "schema: 1\ntracker:\n  kind: memory\n  repository: acme/" + id + "\npolling:\n  interval_ms: 3210\nworkspace:\n  root: .detent/workspaces\n  source_root: .\n"
		if err := os.WriteFile(filepath.Join(sources, id+".git", "detent.yaml"), []byte(definition), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sources, id+".git", "WORKFLOW.md"), []byte("Work the issue.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		runDoctorWorkflowSourceGit(t, filepath.Join(sources, id+".git"), "add", "detent.yaml", "WORKFLOW.md")
		runDoctorWorkflowSourceGit(t, filepath.Join(sources, id+".git"), "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "project definition")
	}
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+sources+string(os.PathSeparator)+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "git@github.com:acme/")
	path := filepath.Join(root, "runner", "global.yaml")
	workspace := filepath.Join(root, "checkouts")
	legacy := filepath.Join(workspace, "orders")
	if err := cloneRunnerProject(t.Context(), "acme/prj_orders", legacy); err != nil {
		t.Fatal(err)
	}
	retainedIssue := workspacepkg.Issue{ProjectID: "orders", ID: "wi_retained", Identifier: "prj_orders#7"}
	retained, err := workspacepkg.NewBackend(workspacepkg.KindLocalGit, workspacepkg.LocalGitOptions{Root: filepath.Join(legacy, ".detent/workspaces"), SourceRoot: legacy, AutoBranch: true})
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
		boot, err := resolveBootConfig(t.Context(), path, "", runtimeFlags{}, defaultOptions())
		if err != nil {
			t.Fatal(err)
		}
		cfg := boot.Global
		if err := scheduler.SetNativeProjects(cfg.Client.NativeProjects); err != nil {
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
			if selected.Workdir != filepath.Join(workspace, selected.ID) || selected.WorkflowRef != "origin/HEAD" || selected.GlobalCache != cfg.Global.Cache.Normalized() {
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
				resumed, err := workspacepkg.NewBackend(workspacepkg.KindLocalGit, workspacepkg.LocalGitOptions{Root: filepath.Join(selected.Workdir, workflow.Config.Workspace.Root), SourceRoot: selected.Workdir, AutoBranch: true})
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
}
