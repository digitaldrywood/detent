package cli

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
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
	}{
		{name: "new assignment"}, {name: "one clone fails", cloneFailure: true}, {name: "one setup hook fails", setupFailure: true},
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
			_, err := runRegisterInTestWorkspace(t, nil, func(*cobra.Command, string) error { return nil }, "--url", hub.server.URL+"/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", configPath, "--workspace-root", workspaceRoot)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := readRunnerSetupConfig(configPath)
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
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			applied := cfg
			p := &runnerProjects{client: client, cfg: cfg, setup: project.NewRunnerSetup("test-runner", localStore, logger), logger: logger, checks: make(map[string]runnerauth.LocalChecks), apply: func(_ context.Context, next globalconfig.Config) error { applied = next; return nil }}
			source := filepath.Join(root, "source")
			checkout(t, source)
			cloneCalls := 0
			p.checkout = func(ctx context.Context, selected globalconfig.Project, url string) error {
				return prepareRunnerCheckoutWithClone(ctx, selected, url, func(_ context.Context, _ string, target string) error {
					cloneCalls++
					if test.cloneFailure && cloneCalls == 1 {
						return &runnerCheckoutError{message: "Cannot clone " + url, fix: "gh auth login"}
					}
					if err := os.CopyFS(target, os.DirFS(source)); err != nil {
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
					return os.WriteFile(filepath.Join(target, "setup.sh"), []byte(script), 0600)
				})
			}
			scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{OrganizationID: "org_example", NativeProjects: map[string]tracker.ProjectID{"orders": "prj_orders"}, Machine: hubclient.Machine{ID: "host", Hostname: "host", Capacity: 2, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if declared := p.runnerSetupDeclared(t.Context(), "later"); declared != nil {
				t.Fatalf("unassigned checkout declared setup: %v", declared)
			}
			hub.projects["prj_later"] = "Later"
			identity := hub.identity.Load().(runnerauth.Identity)
			identity.ProjectIDs = append(identity.ProjectIDs, "prj_later")
			hub.identity.Store(identity)
			if err := p.refresh(t.Context(), scheduler); err != nil {
				t.Fatal(err)
			}
			if p.checks["orders"].Setup != "passed" {
				t.Fatalf("ready project blocked: %+v", p.checks)
			}
			failed := test.cloneFailure || test.setupFailure
			if (p.checks["later"].Setup == "failed") != failed {
				t.Fatalf("new project setup: %+v", p.checks)
			}
			if test.cloneFailure && p.checks["later"].CheckoutFix != "gh auth login" {
				t.Fatalf("missing authentication command: %+v", p.checks["later"])
			}
			if _, ok := scheduler.ConnectorForProject("later"); !ok {
				t.Fatal("new project was not assigned to scheduler")
			}
			later := filepath.Join(workspaceRoot, "later")
			if test.setupFailure {
				if err := os.WriteFile(filepath.Join(later, "setup.sh"), []byte("printf 'setup\\n' >> trace\n"), 0600); err != nil {
					t.Fatal(err)
				}
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
			if cloneCalls != wantClones || p.checks["later"].Setup != "passed" || p.checks["later"].CheckoutMessage != "" || applied.Client.NativeProjects["later"] != "prj_later" {
				t.Fatalf("resume: clones %d, checks %+v, configuration %+v", cloneCalls, p.checks, applied.Client.NativeProjects)
			}
			trace, err := os.ReadFile(filepath.Join(later, "trace"))
			if err != nil || strings.Count(string(trace), "\n") != 1 {
				t.Fatalf("hook did not run exactly once: %q %v", trace, err)
			}
			if declared := p.runnerSetupDeclared(t.Context(), "later"); declared == nil || !*declared {
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
			if declared := p.runnerSetupDeclared(t.Context(), "later"); declared == nil || *declared {
				t.Fatalf("removed setup declaration = %v", declared)
			}
			saved, err := readRunnerSetupConfig(configPath)
			if err != nil || len(saved.Projects) != 1 {
				t.Fatalf("operator config was rewritten: %+v %v", saved, err)
			}
		})
	}
}
