package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestManagedProjectConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	writeWorkflow := func(path, prompt string) {
		t.Helper()
		content := "---\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\n  api_key: fixture-token\n---\n" + prompt + "\n"
		if err := os.WriteFile(path+".tmp", []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".tmp", path); err != nil {
			t.Fatal(err)
		}
	}
	for _, scenario := range []string{"resume", "resume running", "resume drained", "resume manager drained", "resume native drained", "resume native busy drained", "resume drained restart refused", "resume native", "resume database", "resume restart refused", "resume runtime pause", "resume stale revision", "resume wrong policy", "native policy running", "native policy busy running", "native policy paused", "native policy git drained", "native policy plain drained", "native policy retry drained", "detach", "database detach", "stale revision", "wrong policy", "active attempt", "deferred completion", "missing handoff", "stopped owner", "apply policy", "apply policy hooks", "apply policy hooks active", "apply policy hooks deferred", "apply policy hooks backend failure", "unapproved policy", "wrong source", "local overlay", "local policy", "local policy overlays", "local policy native", "local policy native busy", "local policy native unapproved", "local policy native wrong source", "binding", "binding changed settings", "binding unapproved", "binding stale overlay", "binding busy", "binding foreign", "binding local source", "binding drained", "binding running", "binding native running", "binding native paused", "binding native drained", "binding native schedule change"} {
		t.Run(scenario, func(t *testing.T) {
			root := initWorkflowSourceRepo(t)
			workflowPath := filepath.Join(root, "WORKFLOW.md")
			writeWorkflow(workflowPath, "Shared workflow instructions")
			commitWorkflowSourceRepo(t, root, "initial workflow")
			hookReload := strings.HasPrefix(scenario, "apply policy hooks")
			binding := strings.HasPrefix(scenario, "binding")
			localPolicy := strings.HasPrefix(scenario, "local policy")
			policyOverlay := localPolicy || scenario == "local overlay" || hookReload
			sharedPolicy := strings.HasPrefix(scenario, "native policy")
			otherRoot := root
			if binding || policyOverlay {
				if err := os.WriteFile(workflowPath, []byte("Shared workflow instructions\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "detent.yaml"), []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\n  api_key: fixture-token\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runWorkflowSourceGit(t, root, "add", "detent.yaml")
				commitWorkflowSourceRepo(t, root, "split definition")
				if err := os.WriteFile(filepath.Join(root, "detent.local.yaml"), []byte("schema: 1\nworker:\n  ssh_hosts: [local]\n  github_token: private-worker-credential\n"), 0600); err != nil {
					t.Fatal(err)
				}
				otherRoot = initWorkflowSourceRepo(t)
				writeWorkflow(filepath.Join(otherRoot, "WORKFLOW.md"), "unrelated")
				commitWorkflowSourceRepo(t, otherRoot, "unrelated")
			}
			if sharedPolicy {
				otherRoot = initWorkflowSourceRepo(t)
				writeWorkflow(filepath.Join(otherRoot, "WORKFLOW.md"), "unrelated")
				commitWorkflowSourceRepo(t, otherRoot, "unrelated")
			}
			cfg, err := globalconfig.DefaultAt(filepath.Join(root, "global.yaml"), globalconfig.WithProjectPathLiterals())
			if err != nil {
				t.Fatal(err)
			}
			cfg.APIToken = "private-api-credential"
			cfg.Global.Memory.PressureSomeAvg60Threshold = 100
			cfg.Global.IO.PressureFullAvg10Threshold = 100
			cfg.Global.CPU.PressureSomeAvg10Threshold = 100
			cfg.Projects = []globalconfig.Project{
				{ID: "selected", Workflow: "WORKFLOW.md", WorkflowRef: "HEAD", Workdir: root, Weight: 1, Priority: 10, Paused: true},
				{ID: "unrelated", Workflow: "WORKFLOW.md", WorkflowRef: "HEAD", Workdir: otherRoot, Weight: 3, Priority: 7, Paused: true, PausedReason: "user pause", PausedUntilIssue: "unrelated#42"},
			}
			if strings.HasPrefix(scenario, "resume") && scenario != "resume runtime pause" && !strings.Contains(scenario, "drained") && scenario != "resume running" {
				cfg.Projects[0].PausedReason = "operator maintenance"
				cfg.Projects[0].PausedUntilIssue = "selected#42"
			}
			if strings.HasSuffix(scenario, "drained") || strings.HasSuffix(scenario, "running") || strings.HasPrefix(scenario, "resume") && strings.Contains(scenario, "drained") {
				cfg.Projects[0].Paused = false
			}
			if scenario == "binding local source" || localPolicy || scenario == "native policy plain drained" {
				cfg.Projects[0].WorkflowRef = ""
				cfg.Projects[0].Workflow = workflowPath
			}
			if localPolicy {
				private := "schema: 1\nworker:\n  ssh_hosts: [local]\n  github_token: private-worker-credential\nworkspace:\n  root: private-worktrees\nhooks:\n  before_run: private-isolation\n  timeout_ms: 12345\ncodex:\n  command: private-codex-command\n  env:\n    PRIVATE_ENV: private-env-value\nagent:\n  max_concurrent_agents: 1\n"
				if err := os.WriteFile(filepath.Join(root, "detent.local.yaml"), []byte(private), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "local policy overlays" || strings.HasPrefix(scenario, "local policy native") {
				if err := os.WriteFile(filepath.Join(root, "WORKFLOW.local.md"), []byte("Private local instructions"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := globalconfig.Write(cfg.Path, cfg, globalconfig.WithProjectPathLiterals()); err != nil {
				t.Fatal(err)
			}
			attempts, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(root, "runtime.db")})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := attempts.Close(); err != nil {
					t.Error(err)
				}
			})

			native := strings.HasPrefix(scenario, "resume native") || strings.HasPrefix(scenario, "binding native") || strings.HasPrefix(scenario, "local policy native") || sharedPolicy || hookReload
			if native {
				if err := os.WriteFile(workflowPath, []byte("Shared native instructions\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runWorkflowSourceGit(t, root, "add", "WORKFLOW.md")
				raw := "schema: 1\ntracker:\n  kind: hub_native\n  repository: example/project\n  api_key: fixture-token\nschedule_ownership:\n  enabled: false\n"
				if err := os.WriteFile(filepath.Join(root, "detent.yaml"), []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				runWorkflowSourceGit(t, root, "add", "detent.yaml")
				commitWorkflowSourceRepo(t, root, "native source")
			}
			if sharedPolicy {
				local := "schema: 1\nworkspace:\n  root: " + root + "/workspaces\nhooks:\n  before_run: host-bootstrap\nagent:\n  max_concurrent_agents: 3\n"
				if err := os.WriteFile(filepath.Join(root, "detent.local.yaml"), []byte(local), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "WORKFLOW.local.md"), []byte("Machine bootstrap instructions.\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if scenario == "native policy plain drained" {
					definitionRoot := t.TempDir()
					for _, name := range []string{"WORKFLOW.md", "detent.yaml", "WORKFLOW.local.md", "detent.local.yaml"} {
						raw, err := os.ReadFile(filepath.Join(root, name))
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(definitionRoot, name), raw, 0600); err != nil {
							t.Fatal(err)
						}
					}
					cfg.Projects[0].Workflow = filepath.Join(definitionRoot, "WORKFLOW.md")
					if err := globalconfig.Write(cfg.Path, cfg, globalconfig.WithProjectPathLiterals()); err != nil {
						t.Fatal(err)
					}
				}
			}
			scheduling := &managedConfigScheduling{policyTestScheduling: policyTestScheduling{approved: map[string]policy.Descriptor{}}, resolveApproved: hookReload}
			worker := &managedConfigRunner{started: make(chan orchestrator.RunRequest, 1)}
			probe := &managedConfigHookAgent{}
			writeHook := func(marker string) {
				t.Helper()
				raw := "schema: 1\nworkspace:\n  root: " + filepath.Join(root, "workspaces") + "\nhooks:\n  after_create: printf " + marker + " > hook-marker\n"
				if err := os.WriteFile(filepath.Join(root, "detent.local.yaml"), []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if hookReload {
				writeHook("A")
			}
			var selection *workflowconfig.ModelSelection
			if hookReload {
				disabled := false
				selection = &workflowconfig.ModelSelection{Enabled: &disabled}
			}
			if _, err := attempts.InitializeLocalConfiguration(t.Context(), cfg, selection); err != nil {
				t.Fatal(err)
			}
			cfg, err = attempts.LocalConfiguration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := NewManager(ManagerConfigFromGlobal(cfg), ManagerDependencies{ProjectFactory: func(selected globalconfig.Project) (*Project, error) {
				workflow, err := LoadWorkflow(selected)
				if err != nil {
					return nil, err
				}
				workflow.Config = WithMappedNativeTracker(workflow.Config, scheduling, ID(selected.ID))
				descriptor, err := ResolvePolicy(selected, workflow)
				if err != nil {
					return nil, err
				}
				scheduling.approved[selected.ID] = descriptor
				if hookReload && selected.ID == "selected" {
					factory := func(cfg workflowconfig.Config, current workspace.Backend) (workspace.Backend, error) {
						if scenario == "apply policy hooks backend failure" && strings.Contains(cfg.Hooks.AfterCreate, " B ") {
							return nil, errors.New("workspace configuration unavailable")
						}
						return workspace.ReconfigureBackend(current, workspace.KindLocalGit, workspace.LocalGitOptions{Root: cfg.Workspace.Root, SourceRoot: root, AutoBranch: true, Hooks: workspace.Hooks{AfterCreate: cfg.Hooks.AfterCreate}})
					}
					backend, err := factory(workflow.Config, nil)
					if err != nil {
						return nil, err
					}
					worker.actual, err = runnerpkg.NewRunner(runnerpkg.Dependencies{Workflow: workflow, Workspace: backend, WorkspaceFactory: factory, AgentBackend: probe})
					if err != nil {
						return nil, err
					}
				}
				return New(Config{Project: selected, Workflow: workflow}, Dependencies{Scheduling: scheduling, Runner: worker, WorkAttempts: attempts, ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) {
					return fakehub.New(fakehub.Config{}), nil
				}})
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				for _, p := range manager.Registry().List() {
					if err := p.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			verified := 0
			owner := NewConfigurationOwner(t.Context(), cfg, func() globalconfig.Config { return cfg }, manager, attempts, func(_ context.Context, observed globalconfig.Config, id, _, checkpoint string) error {
				verified++
				if id != "selected" || checkpoint != strings.Repeat("c", 64) || observed.APIToken != cfg.APIToken {
					t.Fatal("handoff selected a different authority")
				}
				return nil
			})
			selectedProject, ok := manager.Registry().Get("selected")
			if !ok {
				t.Fatal("selected project missing")
			}
			initialWorkflow := selectedProject.Workflow()
			beforeOther := owner.Read(t.Context(), "unrelated")
			before := owner.Read(t.Context(), "selected")
			if before.Constraint != "" || before.SelectedPolicy == nil || before.EffectivePolicy == nil {
				t.Fatalf("read=%+v", before)
			}
			refreshed := owner.Read(t.Context(), "selected")
			if !refreshed.ObservedAt.After(before.ObservedAt) || before.Diagnostics == nil || refreshed.Diagnostics == nil || !refreshed.Diagnostics.ObservedAt.After(before.Diagnostics.ObservedAt) {
				t.Fatal("configuration owner did not refresh observation timestamps")
			}
			refreshed.ObservedAt = before.ObservedAt
			refreshed.Diagnostics.ObservedAt = before.Diagnostics.ObservedAt
			refreshed.Diagnostics.RuntimeObservedAt = before.Diagnostics.RuntimeObservedAt
			if !reflect.DeepEqual(before, refreshed) {
				t.Fatal("unchanged owner records changed the configuration snapshot")
			}
			raw, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{root, cfg.APIToken, "private-worker-credential"} {
				if strings.Contains(string(raw), private) {
					t.Fatalf("private provenance in %s", raw)
				}
			}
			request := ManagedConfigRequest{ProjectID: "selected", ExpectedConfigRevision: before.ConfigRevision, ExpectedPolicyID: before.EffectivePolicy.ID, Checkpoint: strings.Repeat("c", 64)}
			operation := "detach_local_project"
			switch scenario {
			case "stale revision":
				if err := globalconfig.Mutate(cfg.Path, func(c *globalconfig.Config, _ string) bool { c.InstanceName = "operator edit"; return true }, globalconfig.WithProjectPathLiterals()); err != nil {
					t.Fatal(err)
				}
			case "wrong policy":
				request.ExpectedPolicyID = "different-policy"
			case "active attempt", "deferred completion":
				metadata := "{}"
				if scenario == "deferred completion" {
					metadata = `{"deferred_completion":{"version":1,"result":{"pr_url":"https://forge.test/pr/1"}}}`
				}
				if _, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "selected", IssueID: "issue", Identifier: "selected#1", WorkerType: "code", AttemptNumber: 1, Lane: "In Progress", StartedAt: time.Now(), WorkerMetadataJSON: metadata}); err != nil {
					t.Fatal(err)
				}
			case "missing handoff":
				owner.handoff = func(context.Context, globalconfig.Config, string, string, string) error {
					return errors.New("incomplete")
				}
			case "stopped owner":
				manager.running = false
			case "apply policy", "apply policy hooks", "apply policy hooks active", "apply policy hooks deferred", "apply policy hooks backend failure", "unapproved policy", "wrong source", "local overlay", "local policy", "local policy overlays", "local policy native", "local policy native busy", "local policy native unapproved", "local policy native wrong source":
				operation = "apply_local_project_policy"
				if hookReload {
					writeHook("B")
					if scenario == "apply policy hooks active" || scenario == "apply policy hooks deferred" {
						metadata := "{}"
						if scenario == "apply policy hooks deferred" {
							metadata = `{"deferred_completion":{"version":1,"result":{"pr_url":"https://forge.test/pr/1"}}}`
						}
						if _, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "selected", IssueID: "active", Identifier: "selected#2", WorkerType: "code", AttemptNumber: 1, StartedAt: time.Now(), WorkerMetadataJSON: metadata}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if policyOverlay {
					if err := os.WriteFile(workflowPath, []byte("Changed shared instructions"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					writeWorkflow(workflowPath, "Changed committed private instructions")
				}
				commitWorkflowSourceRepo(t, root, "changed workflow")
				if scenario == "local overlay" {
					if err := os.WriteFile(filepath.Join(root, "WORKFLOW.local.md"), []byte("Uncommitted notes"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				candidateView := owner.Read(t.Context(), "selected")
				request.ExpectedConfigRevision = candidateView.ConfigRevision
				candidate := candidateView.SelectedPolicy
				if candidate == nil {
					t.Fatal("candidate missing")
				}
				request.SourceRevision, request.PolicyID = candidate.SourceRevision, candidate.ID
				if scenario != "unapproved policy" && scenario != "local policy native unapproved" {
					scheduling.approved["selected"] = *candidate
				}
				if scenario == "wrong source" || scenario == "local policy native wrong source" {
					request.SourceRevision = strings.Repeat("f", 40)
				}
			}
			if scenario == "local policy native busy" {
				if _, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "selected", IssueID: "active", Identifier: "selected#2", WorkerType: "code", AttemptNumber: 1, Lane: "In Progress", StartedAt: time.Now(), WorkerMetadataJSON: `{"deferred_completion":{"version":1}}`}); err != nil {
					t.Fatal(err)
				}
			}
			privateFiles := map[string][]byte{}
			if policyOverlay {
				for _, name := range []string{"detent.local.yaml", "WORKFLOW.local.md"} {
					raw, err := os.ReadFile(filepath.Join(root, name))
					if err == nil {
						privateFiles[name] = raw
					} else if !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				}
			}
			if strings.HasPrefix(scenario, "resume") {
				if strings.Contains(scenario, "drained") {
					if scenario == "resume native busy drained" {
						scheduling.mu.Lock()
						scheduling.dispatchPolicy = before.EffectivePolicy.ID
						scheduling.mu.Unlock()
						if _, err := selectedProject.Orchestrator().RequestRefresh(t.Context()); err != nil {
							t.Fatal(err)
						}
						select {
						case <-worker.started:
						case <-time.After(30 * time.Second):
							t.Fatal("busy fixture did not start work")
						}
					}
					drained := owner.Apply(t.Context(), "drain_local_project", request)
					if !drained.Applied || drained.Paused || !drained.Draining || drained.UnsettledAttempts != 0 && scenario != "resume native busy drained" {
						t.Fatalf("drain=%+v", drained)
					}
					fresh := owner.Read(t.Context(), "selected")
					request.ExpectedConfigRevision = fresh.ConfigRevision
					if scenario == "resume manager drained" {
						if err := manager.Unpause(t.Context(), "selected"); err != nil {
							t.Fatal(err)
						}
					}
				}
				originalOrchestrator := selectedProject.Orchestrator()
				if scenario == "resume native" {
					scheduling.mu.Lock()
					scheduling.dispatchPolicy = before.EffectivePolicy.ID
					scheduling.mu.Unlock()
				}
				if scenario == "resume restart refused" || scenario == "resume drained restart refused" {
					selectedProject.orchFactory = func(orchestrator.Config, orchestrator.Dependencies) (*orchestrator.Orchestrator, error) {
						return nil, errors.New("restart refused")
					}
				}
				if scenario == "resume runtime pause" {
					if err := attempts.MutateLocalConfiguration(t.Context(), cfg, func(current *globalconfig.Config, _ string) bool {
						current.Projects[0].Paused = false
						return true
					}); err != nil {
						t.Fatal(err)
					}
					fresh := owner.Read(t.Context(), "selected")
					request.ExpectedConfigRevision = fresh.ConfigRevision
				}
				if scenario == "resume stale revision" {
					request.ExpectedConfigRevision = strings.Repeat("e", 64)
				}
				if scenario == "resume wrong policy" {
					request.ExpectedPolicyID = "policy_wrong"
				}
				result := owner.Apply(t.Context(), "resume_local_project", request)
				if scenario == "resume native busy drained" {
					fresh := owner.Read(t.Context(), "selected")
					if result.Applied || result.Saved || result.Constraint == "" || fresh.Paused || !fresh.Draining || fresh.UnsettledAttempts != 1 || selectedProject.Orchestrator() != originalOrchestrator {
						t.Fatalf("busy resume=%+v readback=%+v", result, fresh)
					}
					return
				}
				refused := scenario == "resume stale revision" || scenario == "resume wrong policy" || scenario == "resume restart refused" || scenario == "resume drained restart refused"
				if refused {
					if result.Applied || result.Constraint == "" || !selectedProject.Paused() {
						t.Fatalf("resume refusal=%+v", result)
					}
					return
				}
				if !result.Applied || !result.Saved || result.Paused || result.Draining || !selectedProject.Running() {
					t.Fatalf("resume=%+v", result)
				}
				if strings.Contains(scenario, "drained") || scenario == "resume running" {
					fresh := owner.Read(t.Context(), "selected")
					if fresh.Constraint != "" || fresh.Paused || fresh.Draining || fresh.UnsettledAttempts != 0 {
						t.Fatalf("resume readback=%+v", fresh)
					}
					request.ExpectedConfigRevision = fresh.ConfigRevision
					resumedOrchestrator := selectedProject.Orchestrator()
					repeated := owner.Apply(t.Context(), "resume_local_project", request)
					if !repeated.Applied || !repeated.Saved || repeated.Paused || repeated.Draining || selectedProject.Orchestrator() != resumedOrchestrator {
						t.Fatalf("repeated resume=%+v", repeated)
					}
				}
				persisted, err := globalconfig.Read(cfg.Path, globalconfig.WithProjectPathLiterals())
				if err == nil {
					persisted, err = attempts.LocalConfiguration(t.Context(), persisted)
				}
				if err != nil || persisted.Projects[0].Paused || persisted.Projects[0].PausedReason != "" || persisted.Projects[0].PausedUntilIssue != "" || !reflect.DeepEqual(persisted.Projects[1], cfg.Projects[1]) {
					t.Fatalf("resume persistence=%+v %v", persisted.Projects, err)
				}
				if scenario == "resume native drained" {
					scheduling.mu.Lock()
					scheduling.dispatchPolicy = request.ExpectedPolicyID
					scheduling.mu.Unlock()
					if _, err := selectedProject.Orchestrator().RequestRefresh(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				if strings.HasPrefix(scenario, "resume native") {
					select {
					case dispatched := <-worker.started:
						if dispatched.ProjectID != "selected" || dispatched.Policy.ID != request.ExpectedPolicyID {
							t.Fatalf("resume dispatch=%+v", dispatched)
						}
					case <-time.After(30 * time.Second):
						t.Fatal("no native dispatch after resume")
					}
				}
				return
			}
			if sharedPolicy {
				selected, _ := manager.Registry().Get("selected")
				previous := selected.Workflow()
				otherBefore := owner.Read(t.Context(), "unrelated")
				attemptID, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "selected", IssueID: "completed", Identifier: "selected#99", WorkerType: "code", AttemptNumber: 1, Lane: "In Progress", StartedAt: time.Now(), WorkerMetadataJSON: `{"change_id":"unpublished-change"}`})
				if err != nil {
					t.Fatal(err)
				}
				if scenario != "native policy busy running" {
					if err := attempts.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: attemptID, CompletedAt: time.Now(), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess}); err != nil {
						t.Fatal(err)
					}
				}
				completed, err := attempts.WorkAttempt(t.Context(), attemptID)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasSuffix(scenario, "drained") {
					drained := owner.Apply(t.Context(), "drain_local_project", request)
					if !drained.Applied || !drained.Draining || drained.UnsettledAttempts != 0 {
						t.Fatalf("drain=%+v", drained)
					}
				}
				approvedWorkflow, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{
					Workflow:  []byte("Approved shared instructions.\n"),
					Config:    []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example/project\ngate:\n  run: make check-land\n  validator:\n    enabled: true\nreview:\n  human: true\n"),
					HasConfig: true, ConfigPath: "detent.yaml",
				})
				if err != nil {
					t.Fatal(err)
				}
				approved, err := workflowconfig.ResolvePolicy(approvedWorkflow)
				if err != nil {
					t.Fatal(err)
				}
				scheduling.approved["selected"] = approved
				scheduling.resolveApproved = true
				fresh := owner.Read(t.Context(), "selected")
				request.ExpectedConfigRevision, request.PolicyID, request.SourceRevision = fresh.ConfigRevision, approved.ID, approved.SourceRevision
				if scenario == "native policy retry drained" {
					scheduling.failApplication = 2
					failed := owner.Apply(t.Context(), "apply_local_project_policy", request)
					if failed.Applied || failed.Constraint == "" || failed.Paused || !failed.Draining || !selected.Running() {
						t.Fatalf("failed apply lost recoverable drain: %+v", failed)
					}
					scheduling.failApplication = 0
					fresh = owner.Read(t.Context(), "selected")
					request.ExpectedConfigRevision, request.ExpectedPolicyID = fresh.ConfigRevision, fresh.EffectivePolicy.ID
				}
				result := owner.Apply(t.Context(), "apply_local_project_policy", request)
				if !result.Applied || result.Paused != cfg.Projects[0].Paused || result.Draining != strings.HasSuffix(scenario, "drained") || selected.Running() == cfg.Projects[0].Paused || result.SelectedPolicy.ID != approved.ID || result.EffectivePolicy.ID != approved.ID || result.EffectivePolicy.SourceRevision != approved.SourceRevision {
					t.Fatalf("shared apply=%+v", result)
				}
				actual := selected.Workflow()
				if actual.Config.Workspace.Root != previous.Config.Workspace.Root || actual.Config.Hooks != previous.Config.Hooks || actual.Config.Agent.MaxConcurrentAgents != previous.Config.Agent.MaxConcurrentAgents || !strings.HasSuffix(strings.TrimSpace(actual.Prompt), "Machine bootstrap instructions.") || !actual.Config.Gate.Validator.Enabled || !actual.Config.Review.Human || actual.Config.Gate.Run != "make check-land" {
					t.Fatal("application discarded host settings or approved protections")
				}
				if err := selected.reconcileWorkflow(t.Context()); err != nil {
					t.Fatal(err)
				}
				if selected.Workflow().Config.Policy.ID != approved.ID {
					t.Fatal("reload discarded approved configuration")
				}
				retained, err := attempts.WorkAttempt(t.Context(), attemptID)
				if err != nil || !reflect.DeepEqual(retained, completed) {
					t.Fatalf("policy adoption changed durable completion: %+v, %v", retained, err)
				}
				otherAfter := owner.Read(t.Context(), "unrelated")
				if otherAfter.EffectivePolicy.ID != otherBefore.EffectivePolicy.ID || otherAfter.Paused != otherBefore.Paused || otherAfter.Draining != otherBefore.Draining {
					t.Fatal("application changed unrelated project")
				}
				return
			}
			var originalLocal []byte
			if binding {
				operation = "apply_local_project_policy"
				if strings.HasSuffix(scenario, "drained") {
					if before.Paused {
						t.Fatal("drain fixture started paused")
					}
					attemptID, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "selected", IssueID: "active", Identifier: "selected#2", WorkerType: "code", AttemptNumber: 1, Lane: "In Progress", StartedAt: time.Now(), WorkerMetadataJSON: "{}"})
					if err != nil {
						t.Fatal(err)
					}
					drained := owner.Apply(t.Context(), "drain_local_project", request)
					if !drained.Applied || !drained.Draining || drained.Paused || drained.UnsettledAttempts != 1 {
						t.Fatalf("drain=%+v", drained)
					}
					enabled := true
					busy := request
					busy.AllowLocalBinding, busy.PolicyID, busy.SourceRevision = &enabled, before.LocalBindingPolicy.ID, before.LocalBindingPolicy.SourceRevision
					if result := owner.Apply(t.Context(), operation, busy); result.Applied || result.Saved || result.Constraint == "" {
						t.Fatalf("busy drain application=%+v", result)
					}
					if err := attempts.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: attemptID, CompletedAt: time.Now(), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess}); err != nil {
						t.Fatal(err)
					}
					before = owner.Read(t.Context(), "selected")
					if before.Constraint != "" || !before.Draining || before.Paused || before.UnsettledAttempts != 0 {
						t.Fatalf("settled drain=%+v", before)
					}
					request.ExpectedConfigRevision = before.ConfigRevision
				}
				enabled := true
				request.AllowLocalBinding = &enabled
				if before.LocalBindingPolicy == nil || before.AllowLocalBinding {
					t.Fatalf("binding preview=%+v", before)
				}
				request.SourceRevision, request.PolicyID = before.LocalBindingPolicy.SourceRevision, before.LocalBindingPolicy.ID
				if scenario != "binding unapproved" {
					scheduling.approved["selected"] = *before.LocalBindingPolicy
					if scenario == "binding native running" {
						scheduling.mu.Lock()
						scheduling.dispatchPolicy = request.PolicyID
						scheduling.mu.Unlock()
					}
				}
				if scenario == "binding stale overlay" {
					if err := os.WriteFile(filepath.Join(root, "detent.local.yaml"), []byte("schema: 1\nworker:\n  github_token: operator-new-credential\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "binding busy" {
					if _, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: "selected", IssueID: "active", Identifier: "selected#2", WorkerType: "code", AttemptNumber: 1, Lane: "In Progress", StartedAt: time.Now(), WorkerMetadataJSON: "{}"}); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "binding foreign" {
					request.ProjectID = "foreign"
				}
				originalLocal, err = os.ReadFile(filepath.Join(root, "detent.local.yaml"))
				if err != nil {
					t.Fatal(err)
				}
			}
			original, err := os.ReadFile(cfg.Path)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "binding changed settings" {
				scheduling.mu.Lock()
				scheduling.onCheck = func() {
					if err := attempts.MutateLocalConfiguration(t.Context(), cfg, func(current *globalconfig.Config, _ string) bool {
						current.Projects[1].PausedReason = "updated settings"
						return true
					}); err != nil {
						t.Error(err)
					}
				}
				scheduling.mu.Unlock()
			}
			result := owner.Apply(t.Context(), operation, request)
			after, err := os.ReadFile(cfg.Path)
			if err != nil {
				t.Fatal(err)
			}
			if binding {
				afterLocal, err := os.ReadFile(filepath.Join(root, "detent.local.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "binding" || scenario == "binding local source" || strings.HasSuffix(scenario, "drained") || strings.HasSuffix(scenario, "running") || scenario == "binding native paused" || scenario == "binding native schedule change" {
					if !result.Saved || !result.Applied || !result.AllowLocalBinding || result.EffectivePolicy.ID != request.PolicyID || string(after) != string(original) {
						t.Fatalf("binding application=%+v", result)
					}
					if !strings.Contains(string(afterLocal), "private-worker-credential") || !strings.Contains(string(afterLocal), "ssh_hosts") {
						t.Fatal("unrelated local settings changed")
					}
					if other := owner.Read(t.Context(), "unrelated"); other.AllowLocalBinding {
						t.Fatal("permission escaped selected project")
					}
					selected, _ := manager.Registry().Get("selected")
					wantRunning := !cfg.Projects[0].Paused
					if selected.Paused() == wantRunning || selected.Running() != wantRunning || result.Paused == wantRunning {
						t.Fatalf("application lifecycle: paused=%t running=%t receipt=%+v", selected.Paused(), selected.Running(), result)
					}
					if result.Draining != strings.HasSuffix(scenario, "drained") {
						t.Fatalf("draining=%t", result.Draining)
					}
					if scenario == "binding native running" {
						select {
						case dispatched := <-worker.started:
							if dispatched.ProjectID != "selected" || dispatched.Policy.ID != request.PolicyID || dispatched.Issue.ID != "wi_binding_dispatch" {
								t.Fatalf("dispatch=%+v", dispatched)
							}
							worker.mu.Lock()
							allowed := worker.workflow.Config.Worker.EffectiveAllowLocalBinding()
							worker.mu.Unlock()
							if !allowed {
								t.Fatal("dispatched worker retained restricted binding")
							}
						case <-time.After(30 * time.Second):
							t.Fatal("no native dispatch after application")
						}
					}
					if native {
						if selected.Workflow().Config.Tracker.Kind != workflowconfig.TrackerHubNative {
							t.Fatal("native routing changed")
						}
						if scenario == "binding native schedule change" {
							changed := selected.Workflow()
							changed.Config.ScheduleOwnership.Key = "changed/key"
							if err := selected.handleWorkflowUpdate(t.Context(), configwatcher.Update{Workflow: changed}); err == nil || !strings.Contains(err.Error(), "schedule_ownership changes") {
								t.Fatalf("schedule change refusal=%v", err)
							}
						}
					}
					loaded, err := LoadWorkflowContext(t.Context(), selected.Config())
					if err != nil {
						t.Fatal(err)
					}
					loaded.Config = WithMappedNativeTracker(loaded.Config, scheduling, selected.ID())
					savedPolicy, err := ResolvePolicy(selected.Config(), loaded)
					if err != nil || savedPolicy.ID != request.PolicyID {
						t.Fatalf("saved definition policy differs: %v", err)
					}
				} else if result.Constraint == "" || result.Applied || result.Saved || string(afterLocal) != string(originalLocal) || string(after) != string(original) {
					t.Fatalf("binding refusal=%+v", result)
				}
				return
			}
			switch scenario {
			case "detach", "database detach":
				if !result.Saved || result.Applied || result.Registered || !result.RuntimeRegistered || verified != 1 {
					t.Fatalf("detach=%+v verified=%d", result, verified)
				}
				updated, err := globalconfig.Read(cfg.Path, globalconfig.WithProjectPathLiterals())
				if err == nil {
					updated, err = attempts.LocalConfiguration(t.Context(), updated)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := cfg
				want.Projects = cfg.Projects[1:]
				if !reflect.DeepEqual(want, updated) {
					t.Fatalf("unrelated configuration changed: want=%+v got=%+v", want, updated)
				}
				if _, err := manager.Reconcile(t.Context(), ManagerConfigFromGlobal(updated)); err != nil {
					t.Fatal(err)
				}
				cfg = updated
				observed := owner.Read(t.Context(), "selected")
				if observed.Registered || observed.RuntimeRegistered || observed.Constraint != "" {
					t.Fatalf("reload receipt=%+v", observed)
				}
				owner.Apply(t.Context(), operation, request)
				replayed, err := os.ReadFile(cfg.Path)
				if err != nil || string(replayed) != string(after) {
					t.Fatal("retry rewrote configuration")
				}
			case "apply policy", "apply policy hooks", "apply policy hooks active", "apply policy hooks deferred", "local overlay", "local policy", "local policy overlays", "local policy native", "local policy native busy":
				if !result.Applied || result.EffectivePolicy.ID != request.PolicyID || !result.Paused || string(after) != string(original) {
					t.Fatalf("apply=%+v", result)
				}
				if scenario == "local policy native busy" || scenario == "apply policy hooks active" || scenario == "apply policy hooks deferred" {
					if owner.Read(t.Context(), "selected").UnsettledAttempts != 1 {
						t.Fatal("policy application settled active or deferred work")
					}
				}
				selected, _ := manager.Registry().Get("selected")
				if localPolicy {
					loaded := selected.Workflow()
					if !reflect.DeepEqual(loaded.Config.Worker, initialWorkflow.Config.Worker) || !reflect.DeepEqual(loaded.Config.Hooks, initialWorkflow.Config.Hooks) || !reflect.DeepEqual(loaded.Config.Codex, initialWorkflow.Config.Codex) || !reflect.DeepEqual(loaded.Config.Workspace, initialWorkflow.Config.Workspace) || loaded.Config.Agent.MaxConcurrentAgents != initialWorkflow.Config.Agent.MaxConcurrentAgents {
						t.Fatal("private execution settings changed")
					}
					if strings.Contains(initialWorkflow.Prompt, "Private local instructions") && !strings.Contains(loaded.Prompt, "Private local instructions") {
						t.Fatal("private instructions lost")
					}
					restarted, err := LoadWorkflowContext(t.Context(), selected.Config())
					if err != nil {
						t.Fatal(err)
					}
					restarted.Config = WithMappedNativeTracker(restarted.Config, scheduling, selected.ID())
					descriptor, err := ResolvePolicy(selected.Config(), restarted)
					if err != nil || descriptor.ID != request.PolicyID {
						t.Fatalf("restart policy=%s err=%v", descriptor.ID, err)
					}
				}
				for name, before := range privateFiles {
					after, err := os.ReadFile(filepath.Join(root, name))
					if err != nil || string(after) != string(before) {
						t.Fatalf("private file %s changed: %v", name, err)
					}
				}
				other := owner.Read(t.Context(), "unrelated")
				if other.EffectivePolicy.ID != beforeOther.EffectivePolicy.ID {
					t.Fatal("unrelated runtime policy changed")
				}
				if hookReload {
					for index, marker := range []string{"B", "C"} {
						if marker == "C" {
							writeHook(marker)
							candidate, err := LoadWorkflow(selected.Config())
							if err != nil {
								t.Fatal(err)
							}
							candidate.Config = WithMappedNativeTracker(candidate.Config, scheduling, selected.ID())
							approved, err := ResolvePolicy(selected.Config(), candidate)
							if err != nil {
								t.Fatal(err)
							}
							scheduling.approved["selected"] = approved
							if err := selected.reconcileWorkflow(t.Context()); err != nil {
								t.Fatal(err)
							}
						}
						_, err := worker.actual.Run(t.Context(), runnerpkg.RunRequest{Policy: selected.Workflow().Config.Policy, Issue: connector.Issue{ID: strconv.Itoa(index), Identifier: fmt.Sprintf("owner/repository#%d", index+1)}})
						if !errors.Is(err, errManagedConfigHookProbe) {
							t.Fatalf("fresh run after apply: %v", err)
						}
						content, err := os.ReadFile(filepath.Join(probe.path, "hook-marker"))
						if err != nil || string(content) != marker {
							t.Fatalf("fresh hook = %q, want %s, error = %v", content, marker, err)
						}
					}
				}
			default:
				if result.Constraint == "" || result.Applied || result.Saved || string(after) != string(original) {
					t.Fatalf("refusal=%+v", result)
				}
				if hookReload {
					if selectedProject.Workflow().Config.Hooks != initialWorkflow.Config.Hooks {
						t.Fatal("refused application changed effective hooks")
					}
					if scenario != "apply policy hooks backend failure" && owner.Read(t.Context(), "selected").UnsettledAttempts != 1 {
						t.Fatal("refused application settled active or deferred work")
					}
				}
			}
		})
	}
}

type managedConfigScheduling struct {
	policyTestScheduling
	mu              sync.Mutex
	dispatchPolicy  string
	dispatched      bool
	resolveApproved bool
	failApplication int
	onCheck         func()
}

func (s *managedConfigScheduling) ConnectorForProject(id string) (connector.Connector, bool) {
	return fakehub.New(fakehub.Config{}), id == "selected" || id == "unrelated"
}

func (s *managedConfigScheduling) ResolveProjectWorkflow(_ context.Context, id string, workflow workflowconfig.Workflow, _ *policy.RepositorySource) (workflowconfig.Workflow, error) {
	if !s.resolveApproved || id != "selected" {
		return workflow, nil
	}
	return workflowconfig.ApplyNativePolicy(workflow, s.approved[id])
}

func (s *managedConfigScheduling) CheckProjectPolicy(ctx context.Context, id, repository string, descriptor policy.Descriptor) error {
	s.mu.Lock()
	onCheck := s.onCheck
	s.onCheck = nil
	s.mu.Unlock()
	if onCheck != nil {
		onCheck()
	}

	if s.failApplication > 0 {
		s.failApplication--
		if s.failApplication == 0 {
			return errors.New("approved policy temporarily unavailable")
		}
	}
	return s.policyTestScheduling.CheckProjectPolicy(ctx, id, repository, descriptor)
}

func (s *managedConfigScheduling) FetchCandidateIssues(_ context.Context, request orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dispatched || s.dispatchPolicy == "" || request.Policy.ID != s.dispatchPolicy {
		return nil, nil
	}
	issue := connector.NewIssue()
	issue.ID, issue.Identifier, issue.Title, issue.State = "wi_binding_dispatch", "selected#1", "Dispatch after binding application", "Todo"
	issue.Fields["detent_hub_work_item_id"] = issue.ID
	s.dispatched = true
	return []connector.Issue{issue}, nil
}

func (s *managedConfigScheduling) AdoptClaim(_ context.Context, issue connector.Issue, now time.Time) (orchestrator.Claimed, error) {
	return orchestrator.Claimed{Issue: issue, Owner: "selected-runner", ClaimedAt: now, LeaseRenewedAt: now, LeaseExpiresAt: now.Add(time.Minute)}, nil
}

type managedConfigRunner struct {
	actual   *runnerpkg.Runner
	mu       sync.Mutex
	workflow workflowconfig.Workflow
	started  chan orchestrator.RunRequest
}

func (r *managedConfigRunner) UpdateWorkflow(workflow workflowconfig.Workflow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workflow = workflow
}

func (r *managedConfigRunner) Run(ctx context.Context, request orchestrator.RunRequest) (orchestrator.RunResult, error) {
	r.started <- request
	<-ctx.Done()
	return orchestrator.RunResult{}, ctx.Err()
}

func (r *managedConfigRunner) PrepareWorkflowUpdate(workflow workflowconfig.Workflow) (func(), error) {
	if r.actual != nil {
		return r.actual.PrepareWorkflowUpdate(workflow)
	}
	return func() { r.UpdateWorkflow(workflow) }, nil
}

var errManagedConfigHookProbe = errors.New("hook probe finished")

type managedConfigHookAgent struct {
	path string
}

func (a *managedConfigHookAgent) RunTurn(_ context.Context, req runnerpkg.AgentTurnRequest, _ runnerpkg.AgentUpdateHandler) (runnerpkg.AgentTurnResult, error) {
	if !req.TextOnly {
		a.path = req.Workspace
	}
	return runnerpkg.AgentTurnResult{}, errManagedConfigHookProbe
}
