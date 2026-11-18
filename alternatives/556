//go:build !windows

package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerProjectsAssignmentRefresh(t *testing.T) {

	t.Parallel()
	for _, test := range []struct {
		name         string
		cloneFailure bool
		setupFailure bool
		unbound      bool
		missing      bool
		generate     bool
		incremental  bool
	}{
		{name: "new assignment", incremental: true}, {name: "one clone fails", cloneFailure: true}, {name: "one setup hook fails", setupFailure: true}, {name: "wait for repository binding", unbound: true}, {name: "missing configuration appears next cycle", missing: true}, {name: "generate starter files", missing: true, generate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			hub := newRegisterHub(t, map[tracker.ProjectID]string{"prj_orders": "orders"})
			hub.features = []string{"native_issues", "scoped_collaboration", "scoped_runner_identity", "repository_policy", tracker.NativeProjectCheckoutCapability, tracker.NativeLocalChecksCapability}
			hub.cloneURLs = map[tracker.ProjectID]string{"prj_later": "https://github.com/example-org/later.git"}
			hub.repositories["prj_orders"] = "example-org/prj_orders"
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "config", "global.yaml")
			workspaceRoot := filepath.Join(root, "checkouts")
			checkout(t, filepath.Join(workspaceRoot, "orders"))
			runDoctorWorkflowSourceGit(t, filepath.Join(workspaceRoot, "orders"), "remote", "set-url", "origin", "https://github.com/example-org/prj_orders.git")
			runDoctorWorkflowSourceGit(t, filepath.Join(workspaceRoot, "orders"), "config", "url."+filepath.Join(workspaceRoot, "orders")+".insteadOf", "https://github.com/example-org/prj_orders.git")
			_, err = runRegisterInTestWorkspace(t, nil, func(*cobra.Command, string) error { return nil }, "--url", hub.server.URL+"/organizations/org_example", "--token", "det_enroll_example", "--name", "Build host", "--capacity", "2", "--config", configPath, "--workspace-root", workspaceRoot)
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
			setupState := &runnerSetupFixture{}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			t.Cleanup(func() {
				if t.Failed() {
					t.Log(logs.String())
				}
			})
			applied := cfg
			p := &runnerProjects{client: client, cfg: cfg, setup: project.NewRunnerSetup("test-runner", setupState, logger), logger: logger, checks: make(map[string]runnerauth.LocalChecks), apply: func(_ context.Context, next globalconfig.Config) error { applied = next; return nil }}
			doctorStarted, releaseDoctor := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				select {
				case <-releaseDoctor:
				default:
					close(releaseDoctor)
				}
			})
			p.inspect = func(ctx context.Context, cfg globalconfig.Config, name string, emit ...func(runnerauth.LocalChecks)) runnerauth.LocalChecks {
				return collectRunnerLocalChecks(ctx, cfg, name, func(context.Context, doctorConfig) doctorReport {
					if test.incremental && name == "prj_later" {
						close(doctorStarted)
						select {
						case <-releaseDoctor:
						case <-ctx.Done():
						}
					}
					return doctorReport{}
				}, func(context.Context, workflowconfig.AgentBackend) bool { return true }, emit...)
			}
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
					if err := runGit("config", "uploadpack.hideRefs", "refs/remotes"); err != nil {
						return err
					}
					workflow := "---\ntracker:\n  kind: hub_native\n  repository: example-org/later\nhooks:\n  runner_setup: setup.sh\n  shell: sh\n---\nRun work.\n"
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
					if test.missing {
						if err := runGit("rm", "WORKFLOW.md"); err != nil {
							return err
						}
						if err := runGit("-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "missing definition"); err != nil {
							return err
						}
					}
					return runGit("update-ref", "refs/remotes/origin/HEAD", "HEAD")
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
			hub.repositories["prj_later"] = "example-org/later"
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
				hub.repositories["prj_later"] = "example-org/later"
				hub.cloneURLs["prj_later"] = "https://github.com/example-org/later.git"
				p.lastRefresh = time.Time{}
			}
			if test.incremental {
				if err := p.refresh(t.Context(), scheduler); err != nil {
					t.Fatal(err)
				}
				select {
				case <-doctorStarted:
				case <-t.Context().Done():
					t.Fatal(t.Context().Err())
				}
				p.lastRefresh = time.Time{}
				if err := p.refresh(t.Context(), scheduler); err != nil {
					t.Fatal(err)
				}
				checks := p.checks["prj_later"]
				if checks.Checkout != "passed" || checks.Configuration != "passed" || checks.Doctor != "running" || checks.Provider != "pending" {
					t.Fatalf("incremental checks unavailable: %+v", checks)
				}
				if err := scheduler.Heartbeat(t.Context()); err != nil {
					t.Fatal(err)
				}
				report, ok := hub.localChecks.Load(tracker.ProjectID("prj_later"))
				if !ok || report.(runnerauth.LocalChecks).Doctor != "running" {
					t.Fatalf("incremental heartbeat missing: %+v", report)
				}
				close(releaseDoctor)
			}
			if err := refreshRunnerProjects(t, p, scheduler); err != nil {
				t.Fatal(err)
			}
			if p.checks["orders"].Setup != "passed" {
				t.Fatalf("ready project blocked: %+v", p.checks)
			}
			if test.missing {
				checks := p.checks["prj_later"]
				if checks.Checkout != "passed" || checks.Configuration != "missing" || checks.Doctor != "pending" || checks.Provider != "pending" {
					t.Fatalf("missing configuration: %+v", checks)
				}
				if problems := p.reportedProblems(); checks.Setup != "failed" || len(problems) != 1 || problems[0].ErrorOutput == "" {
					t.Fatalf("initial workflow load error was lost: checks=%+v problems=%+v", checks, problems)
				}
				if test.generate {
					hub.setupProgress.Store(onboarding.Progress{Revision: 1, Repository: "generate"})
				} else {
					for file, content := range map[string]string{"detent.yaml": "schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/later\n", "WORKFLOW.md": "Work the assigned issue.\n"} {
						if err := os.WriteFile(filepath.Join(workspaceRoot, "prj_later", file), []byte(content), 0600); err != nil {
							t.Fatal(err)
						}
					}
					later := filepath.Join(workspaceRoot, "prj_later")
					runDoctorWorkflowSourceGit(t, later, "add", "detent.yaml", "WORKFLOW.md")
					runDoctorWorkflowSourceGit(t, later, "-c", "user.name=Detent Test", "-c", "user.email=detent@example.com", "-c", "commit.gpgsign=false", "commit", "-m", "add definition")
					runDoctorWorkflowSourceGit(t, later, "update-ref", "refs/remotes/origin/HEAD", "HEAD")
				}
				p.lastRefresh = time.Time{}
				if err := refreshRunnerProjects(t, p, scheduler); err != nil {
					t.Fatal(err)
				}
				if !p.checks["prj_later"].Passed() {
					t.Fatalf("configuration did not recover: %+v", p.checks["prj_later"])
				}
				if cloneCalls != 1 {
					t.Fatalf("existing checkout was cloned %d times", cloneCalls)
				}
				raw, err := os.ReadFile(filepath.Join(workspaceRoot, "prj_later", "WORKFLOW.md"))
				if err != nil || len(raw) == 0 {
					t.Fatalf("workflow missing: %s %v", raw, err)
				}
				return
			}
			failed := test.cloneFailure || test.setupFailure
			if (p.checks["prj_later"].Setup == "failed") != failed {
				t.Fatalf("new project setup: %+v", p.checks)
			}
			if test.cloneFailure && p.checks["prj_later"].CheckoutFix != "gh auth login" {
				t.Fatalf("missing authentication command: %+v", p.checks["prj_later"])
			}
			if !failed && !p.checks["prj_later"].Passed() {
				t.Fatalf("newly routed project checks did not pass: %+v", p.checks["prj_later"])
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
