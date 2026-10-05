package project

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestManagedProjectConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}

	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, scenario := range []string{"detach", "stale revision", "wrong policy", "active attempt", "deferred completion", "missing handoff", "stopped owner", "apply policy", "unapproved policy", "wrong source", "local overlay", "binding", "binding unapproved", "binding stale overlay", "binding busy", "binding foreign", "binding local source", "binding drained", "binding running", "binding native running", "binding native paused", "binding native drained", "binding native schedule change"} {
		t.Run(scenario, func(t *testing.T) {
			root := initWorkflowSourceRepo(t)
			workflowPath := filepath.Join(root, "WORKFLOW.md")
			writeWorkflowSourceFile(t, workflowPath, "Private workflow instructions")
			commitWorkflowSourceRepo(t, root, "initial workflow")
			binding := strings.HasPrefix(scenario, "binding")
			otherRoot := root
			if binding {
				if err := os.WriteFile(workflowPath, []byte("Private workflow instructions\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "detent.yaml"), []byte("schema: 1\ntracker:\n  kind: memory\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runWorkflowSourceGit(t, root, "add", "detent.yaml")
				commitWorkflowSourceRepo(t, root, "split definition")
				if err := os.WriteFile(filepath.Join(root, "detent.local.yaml"), []byte("schema: 1\nworker:\n  ssh_hosts: [local]\n  github_token: private-worker-credential\n"), 0600); err != nil {
					t.Fatal(err)
				}
				otherRoot = initWorkflowSourceRepo(t)
				writeWorkflowSourceFile(t, filepath.Join(otherRoot, "WORKFLOW.md"), "unrelated")
				commitWorkflowSourceRepo(t, otherRoot, "unrelated")
			}
			cfg, err := globalconfig.DefaultAt(filepath.Join(root, "global.yaml"), globalconfig.WithProjectPathLiterals())
			if err != nil {
				t.Fatal(err)
			}
			cfg.APIToken = "private-api-credential"
			cfg.Projects = []globalconfig.Project{
				{ID: "selected", Workflow: "WORKFLOW.md", WorkflowRef: "HEAD", Workdir: root, Weight: 1, Paused: true},
				{ID: "unrelated", Workflow: "WORKFLOW.md", WorkflowRef: "HEAD", Workdir: otherRoot, Weight: 3, Priority: 7, Paused: true, PausedReason: "user pause", PausedUntilIssue: "unrelated#42"},
			}
			if strings.HasSuffix(scenario, "drained") || strings.HasSuffix(scenario, "running") {
				cfg.Projects[0].Paused = false
			}
			if scenario == "binding local source" {
				cfg.Projects[0].WorkflowRef = ""
				cfg.Projects[0].Workflow = workflowPath
			}
			if err := globalconfig.Write(cfg.Path, cfg, globalconfig.WithProjectPathLiterals()); err != nil {
				t.Fatal(err)
			}
			cfg, err = globalconfig.Read(cfg.Path, globalconfig.WithProjectPathLiterals())
			if err != nil {
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
			native := strings.HasPrefix(scenario, "binding native")
			if native {
				if err := os.WriteFile(workflowPath, []byte("Shared native instructions\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runWorkflowSourceGit(t, root, "add", "WORKFLOW.md")
				raw := "schema: 1\ntracker:\n  kind: github\n  repository: example/project\n  github_status_source: label\n  api_key: fixture-token\nschedule_ownership:\n  enabled: false\n"
				if err := os.WriteFile(filepath.Join(root, "detent.yaml"), []byte(raw), 0600); err != nil {
					t.Fatal(err)
				}
				runWorkflowSourceGit(t, root, "add", "detent.yaml")
				commitWorkflowSourceRepo(t, root, "native source")
			}
			scheduling := &managedConfigScheduling{policyTestScheduling: policyTestScheduling{approved: map[string]policy.Descriptor{}}, native: native}
			worker := &managedConfigRunner{started: make(chan orchestrator.RunRequest, 1)}
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
				return New(Config{Project: selected, Workflow: workflow}, Dependencies{Scheduling: scheduling, Runner: worker, WorkAttempts: attempts})
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
			owner := NewConfigurationOwner(cfg, func() globalconfig.Config { return cfg }, manager, attempts, func(_ context.Context, observed globalconfig.Config, id, _, checkpoint string) error {
				verified++
				if id != "selected" || checkpoint != strings.Repeat("c", 64) || observed.APIToken != cfg.APIToken {
					t.Fatal("handoff selected a different authority")
				}
				return nil
			})
			before := owner.Read(t.Context(), "selected")
			if before.Constraint != "" || before.SelectedPolicy == nil || before.EffectivePolicy == nil {
				t.Fatalf("read=%+v", before)
			}
			raw, err := json.Marshal(before)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{root, cfg.APIToken, "private-worker-credential", "Private workflow instructions", "WORKFLOW.md"} {
				if native && private == "WORKFLOW.md" {
					continue
				}
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
			case "apply policy", "unapproved policy", "wrong source", "local overlay":
				operation = "apply_local_project_policy"
				writeWorkflowSourceFile(t, workflowPath, "Changed committed private instructions")
				commitWorkflowSourceRepo(t, root, "changed workflow")
				candidateView := owner.Read(t.Context(), "selected")
				request.ExpectedConfigRevision = candidateView.ConfigRevision
				candidate := candidateView.SelectedPolicy
				if candidate == nil {
					t.Fatal("candidate missing")
				}
				request.SourceRevision, request.PolicyID = candidate.SourceRevision, candidate.ID
				if scenario != "unapproved policy" {
					scheduling.approved["selected"] = *candidate
				}
				if scenario == "wrong source" {
					request.SourceRevision = strings.Repeat("f", 40)
				}
				if scenario == "local overlay" {
					if err := os.WriteFile(filepath.Join(root, "WORKFLOW.local.md"), []byte("Uncommitted notes"), 0600); err != nil {
						t.Fatal(err)
					}
				}
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
						case <-time.After(5 * time.Second):
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
			case "detach":
				if !result.Saved || result.Applied || result.Registered || !result.RuntimeRegistered || verified != 1 {
					t.Fatalf("detach=%+v verified=%d", result, verified)
				}
				updated, err := globalconfig.Read(cfg.Path, globalconfig.WithProjectPathLiterals())
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
			case "apply policy":
				if !result.Applied || result.EffectivePolicy.ID != request.PolicyID || !result.Paused || string(after) != string(original) {
					t.Fatalf("apply=%+v", result)
				}
				other := owner.Read(t.Context(), "unrelated")
				if other.EffectivePolicy.ID != before.EffectivePolicy.ID {
					t.Fatal("unrelated runtime policy changed")
				}
			default:
				if result.Constraint == "" || result.Applied || result.Saved || string(after) != string(original) {
					t.Fatalf("refusal=%+v", result)
				}
			}
		})
	}
}

type managedConfigScheduling struct {
	policyTestScheduling
	mu             sync.Mutex
	native         bool
	dispatchPolicy string
	dispatched     bool
}

func (s *managedConfigScheduling) ConnectorForProject(id string) (connector.Connector, bool) {
	return memory.New(memory.Config{}), s.native && id == "selected"
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
