package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/hostpressure"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

func TestManagedProjectConfiguration(t *testing.T) {
	for _, scenario := range []string{"resume", "resume running", "resume drained", "drain", "detach", "detach running", "detach deferred", "resume deferred", "binding running", "binding paused", "binding drained", "binding deferred", "policy running", "policy paused", "policy drained", "backend failure", "backend retry", "backend failure drained", "backend retry drained", "private instructions", "schedule change", "invalid policy", "missing owner", "missing policy", "wrong authority"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			files := map[string][]byte{"global.yaml": []byte("instance_name: example\nprojects: []\n"), "WORKFLOW.md": []byte("Example source instructions.\n"), "detent.local.yaml": []byte("worker:\n  github_token: fixture-private-value\nhooks:\n  before_run: fixture-host-hook\n")}
			for name, raw := range files {
				if err := os.WriteFile(filepath.Join(root, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := workflowconfig.Default()
			cfg.Tracker.Kind = workflowconfig.TrackerHubNative
			cfg.Tracker.Repository = "example-org/example-project"
			cfg.Tracker.APIKey = "fixture-token"
			cfg.ScheduleOwnership.Enabled = false
			cfg.Workspace.Root = filepath.Join(root, "workspaces")
			cfg.Hooks.BeforeRun = "fixture-host-hook"
			cfg.Agent.MaxConcurrentAgents = 2
			workflow := workflowconfig.Workflow{Config: cfg, Prompt: "Initial shared instructions.", SharedPrompt: "Initial shared instructions."}
			var err error
			if scenario == "private instructions" {
				workflow, err = workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{ConfigPath: "detent.yaml", WorkflowPath: "WORKFLOW.md", HasConfig: true, Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\nschedule_ownership:\n  enabled: false\n"), Workflow: []byte("Initial shared instructions.\n"), HasLocalConfig: true, LocalConfig: []byte(fmt.Sprintf("schema: 1\nworkspace:\n  root: %q\nhooks:\n  before_run: fixture-host-hook\nworker:\n  github_token: fixture-private-value\nagent:\n  max_concurrent_agents: 2\n", cfg.Workspace.Root)), HasLocalWorkflow: true, LocalWorkflow: []byte("Fixture machine-only instructions.\n")})
				if err != nil {
					t.Fatal(err)
				}
			}
			descriptor, err := workflowconfig.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			workflow.Config.Policy = descriptor
			paused := scenario == "resume" || scenario == "resume deferred" || strings.HasSuffix(scenario, "paused") || strings.HasPrefix(scenario, "detach")
			selected := globalconfig.Project{ID: "selected", Workflow: filepath.Join(root, "WORKFLOW.md"), Workdir: root, Weight: 1, Paused: paused}
			unrelated := globalconfig.Project{ID: "unrelated", Workflow: filepath.Join(root, "WORKFLOW.md"), Workdir: root, Weight: 3, Priority: 7, Paused: true, PausedReason: "user pause", PausedUntilIssue: "unrelated#42"}
			worker := &managedRuntimeRunner{}
			ctx, cancel := context.WithCancel(t.Context())
			manager, err := NewManager(ManagerConfig{Projects: []globalconfig.Project{selected, unrelated}}, ManagerDependencies{ProjectFactory: func(project globalconfig.Project) (*Project, error) {
				return NewContext(ctx, Config{Project: project, Workflow: workflow}, Dependencies{Runner: worker, Scheduling: &managedRuntimeScheduling{}, Logger: slog.New(slog.DiscardHandler), ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) { return fakehub.New(fakehub.Config{}), nil }, WorkflowWatcherFactory: func(string) (WorkflowWatcher, error) { return managedRuntimeWatcher{}, nil }, OrchestratorFactory: func(config orchestrator.Config, deps orchestrator.Dependencies) (*orchestrator.Orchestrator, error) {
					sample := func(context.Context) (hostpressure.Sample, error) { return hostpressure.Sample{}, nil }
					deps.ReadMemoryPressure = sample
					deps.ReadIOPressure = sample
					deps.ReadCPUPressure = sample
					return orchestrator.New(config, deps)
				}})
			}})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel()
				manager.Wait()
				for _, p := range manager.Registry().List() {
					if err := p.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			if err := manager.Start(ctx); err != nil {
				t.Fatal(err)
			}
			p, ok := manager.Registry().Get("selected")
			if !ok {
				t.Fatal("selected runtime absent")
			}
			other, _ := manager.Registry().Get("unrelated")
			previousOther := other.Config()
			previousOtherPolicy := other.Workflow().Config.Policy
			previous := p.Workflow()
			desired := runnerauth.ProjectConfiguration{ProjectID: "native-selected", Authority: "hub_configuration", ConfigRevision: strings.Repeat("b", 64), Registered: true, Paused: paused, Source: "cloud_workflow", EffectivePolicy: &descriptor, SelectedPolicy: &descriptor, ObservedAt: time.Now().UTC()}
			if strings.HasSuffix(scenario, "drained") || scenario == "resume drained" {
				drain := desired
				drain.Paused = false
				drain.Draining = true
				result := manager.ApplyHubConfiguration(ctx, "selected", drain)
				if !result.Applied || !result.Draining || !p.Running() {
					t.Fatalf("initial drain=%+v", result)
				}
				desired.Paused = false
				desired.Draining = true
			}
			switch scenario {
			case "resume", "resume running", "resume drained":
				desired.Paused = false
				desired.Draining = false
			case "drain":
				desired.Paused = false
				desired.Draining = true
			case "detach", "detach running", "detach deferred":
				desired.Registered = false
				if scenario == "detach running" {
					if err := p.unpause(ctx, false); err != nil {
						t.Fatal(err)
					}
				}
				if scenario == "detach deferred" {
					desired.UnsettledAttempts = 1
				}
			case "resume deferred":
				desired.Paused = false
				desired.UnsettledAttempts = 1
			case "binding running", "binding paused", "binding drained", "binding deferred":
				desired.AllowLocalBinding = true
				candidate := previous
				candidate.Config.Worker.AllowLocalBinding = new(true)
				approved, err := workflowconfig.ResolvePolicy(candidate)
				if err != nil {
					t.Fatal(err)
				}
				desired.EffectivePolicy = &approved
				desired.SelectedPolicy = &approved
				if scenario == "binding deferred" {
					desired.UnsettledAttempts = 1
				}
			case "policy running", "policy paused", "policy drained", "backend failure", "backend retry", "backend failure drained", "backend retry drained", "schedule change":
				candidate := previous
				candidate.Prompt = "Approved shared instructions."
				candidate.SharedPrompt = candidate.Prompt
				candidate.Config.Review.Human = true
				candidate.Config.Gate.Validator.Enabled = true
				candidate.Config.Gate.Run = "make check-land"
				if scenario == "schedule change" {
					candidate.Config.ScheduleOwnership.Enabled = true
					candidate.Config.ScheduleOwnership.Key = "example/production"
					candidate.Config.ScheduleOwnership.Repository = "example-org/coordination"
				}
				approved, err := workflowconfig.ResolvePolicy(candidate)
				if err != nil {
					t.Fatal(err)
				}
				desired.EffectivePolicy = &approved
				desired.SelectedPolicy = &approved
				if strings.HasPrefix(scenario, "backend") {
					worker.setFailure(true)
				}
			case "private instructions":
				sources := *previous.DefinitionSources
				sources.Workflow = []byte("Approved shared instructions.\n")
				candidate, err := workflowconfig.ParseProjectDefinition(sources)
				if err != nil {
					t.Fatal(err)
				}
				approved, err := workflowconfig.ResolvePolicy(candidate)
				if err != nil {
					t.Fatal(err)
				}
				desired.EffectivePolicy = &approved
				desired.SelectedPolicy = &approved
			case "invalid policy":
				invalid := descriptor
				invalid.ID = "invalid"
				desired.EffectivePolicy = &invalid
				desired.AllowLocalBinding = true
			case "missing policy":
				desired.EffectivePolicy = nil
			case "wrong authority":
				desired.Authority = "local_global_configuration"
			}
			id := "selected"
			if scenario == "missing owner" {
				id = "absent"
			}
			result := manager.ApplyHubConfiguration(ctx, id, desired)
			refused := scenario == "detach running" || strings.HasSuffix(scenario, "deferred") || strings.HasPrefix(scenario, "backend") || scenario == "invalid policy" || scenario == "missing owner" || scenario == "missing policy" || scenario == "wrong authority"
			if refused {
				if result.Applied || result.Saved || result.Constraint == "" {
					t.Fatalf("refused runtime application=%+v", result)
				}
				if p.Workflow().Config.Policy.ID != previous.Config.Policy.ID || p.Workflow().Config.Hooks != previous.Config.Hooks || p.Workflow().Config.Worker.EffectiveAllowLocalBinding() != previous.Config.Worker.EffectiveAllowLocalBinding() {
					t.Fatal("refused application changed effective policy or host behavior")
				}
				if strings.HasPrefix(scenario, "backend") && strings.HasSuffix(scenario, "drained") && (result.Paused || !result.Draining || !p.Running()) {
					t.Fatalf("failed apply lost recoverable drain: %+v", result)
				}
				if strings.HasPrefix(scenario, "backend retry") {
					worker.setFailure(false)
					result = manager.ApplyHubConfiguration(ctx, id, desired)
					if !result.Applied || result.Constraint != "" || result.Draining != desired.Draining || result.Paused != desired.Paused || !p.Running() {
						t.Fatalf("runtime retry=%+v", result)
					}
				}
			} else {
				if !result.Applied || !result.Saved || result.Constraint != "" {
					t.Fatalf("runtime application=%+v", result)
				}
				if desired.Registered {
					if result.EffectivePolicy == nil || result.EffectivePolicy.ID != desired.EffectivePolicy.ID || p.Workflow().Config.Policy.ID != desired.EffectivePolicy.ID || result.Paused != desired.Paused || p.Paused() != desired.Paused || p.Running() == desired.Paused || result.Draining != desired.Draining || result.AllowLocalBinding != desired.AllowLocalBinding {
						t.Fatalf("effective runtime state=%+v paused=%v running=%v", result, p.Paused(), p.Running())
					}
					actual := p.Workflow()
					if actual.Config.Workspace.Root != previous.Config.Workspace.Root || actual.Config.Hooks != previous.Config.Hooks || actual.Config.Agent.MaxConcurrentAgents != previous.Config.Agent.MaxConcurrentAgents || actual.Config.ScheduleOwnership != previous.Config.ScheduleOwnership {
						t.Fatal("application discarded host settings")
					}
					if strings.HasPrefix(scenario, "policy") && (!actual.Config.Review.Human || !actual.Config.Gate.Validator.Enabled || actual.Config.Gate.Run != "make check-land" || !strings.Contains(actual.Prompt, "Approved shared instructions.")) {
						t.Fatal("application lost approved protections or instructions")
					}
				} else if _, ok := manager.Registry().Get("selected"); ok || result.RuntimeRegistered {
					t.Fatal("detached runtime remains registered")
				}
			}
			if scenario == "private instructions" {
				raw, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				for _, private := range []string{"fixture-private-value", "Fixture machine-only instructions", root, "detent.local.yaml"} {
					if strings.Contains(string(raw), private) {
						t.Fatalf("private provenance in configuration report: %s", raw)
					}
				}
				if !strings.Contains(p.Workflow().Prompt, "Fixture machine-only instructions.") || !strings.Contains(p.Workflow().Prompt, "Approved shared instructions.") {
					t.Fatal("private or shared instructions lost")
				}
			}
			if !reflect.DeepEqual(other.Config(), previousOther) || other.Workflow().Config.Policy.ID != previousOtherPolicy.ID {
				t.Fatal("application changed unrelated runtime")
			}
			for name, original := range files {
				actual, err := os.ReadFile(filepath.Join(root, name))
				if err != nil || string(actual) != string(original) {
					t.Fatalf("local file %s changed: %v", name, err)
				}
			}
		})
	}
}

type managedRuntimeWatcher struct{}

func (managedRuntimeWatcher) Watch(context.Context) (<-chan configwatcher.Update, error) {
	return nil, nil
}

type managedRuntimeRunner struct {
	mu     sync.Mutex
	reject bool
}

func (r *managedRuntimeRunner) Run(ctx context.Context, _ orchestrator.RunRequest) (orchestrator.RunResult, error) {
	<-ctx.Done()
	return orchestrator.RunResult{}, ctx.Err()
}
func (r *managedRuntimeRunner) setFailure(value bool) { r.mu.Lock(); r.reject = value; r.mu.Unlock() }
func (r *managedRuntimeRunner) PrepareWorkflowUpdate(workflowconfig.Workflow) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reject {
		return nil, errors.New("runtime backend unavailable")
	}
	return func() {}, nil
}
func TestHubConfigurationAppliesRuntimeWithoutLocalState(t *testing.T) {
	for _, id := range []string{"project_first", "project_second"} {
		t.Run(id, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			original := []byte("projects: []\n")
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			descriptor := policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")), Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"}}.WithID()
			desired := runnerauth.ProjectConfiguration{ProjectID: id, Authority: "hub_configuration", ConfigRevision: strings.Repeat("b", 64), Registered: true, Paused: true, Source: "cloud_workflow", EffectivePolicy: &descriptor, SelectedPolicy: &descriptor, ObservedAt: time.Now().UTC()}
			audit := policy.AuditRoute{Backend: id + "-audit", Model: id + "-model"}
			for range 2 {
				events := hub.New[Event]()
				cfg := workflowconfig.Config{Policy: descriptor}
				cfg.Agents.Routes = []workflowconfig.AgentRoute{{Role: "security_audit", Backend: audit.Backend, Model: audit.Model, Default: true}}
				p := &Project{id: ID(id), cfg: globalconfig.Project{ID: id}, workflow: workflowconfig.Workflow{Config: cfg}, events: events}
				registry := NewRegistry()
				if err := registry.Set(p); err != nil {
					t.Fatal(err)
				}
				manager := &Manager{registry: registry}
				result := manager.ApplyHubConfiguration(t.Context(), id, desired)
				events.Close()
				if !result.Applied || !result.Saved || !p.Paused() || result.Constraint != "" || result.ConfigRevision != desired.ConfigRevision {
					t.Fatalf("Hub runtime application=%+v paused=%v", result, p.Paused())
				}
				if result.LandingAuditRoute == nil || *result.LandingAuditRoute != audit {
					t.Fatalf("runtime configuration lost the existing audit route: %+v", result.LandingAuditRoute)
				}
				stored, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if string(stored) != string(original) {
					t.Fatal("Hub runtime application rewrote local configuration")
				}
			}
		})
	}
}

type managedRuntimeScheduling struct{ testSchedulingSource }

func (*managedRuntimeScheduling) ConnectorForProject(string) (connector.Connector, bool) {
	return fakehub.New(fakehub.Config{}), true
}

type hubPolicyScheduling struct {
	mu sync.Mutex
	testSchedulingSource
	checked      []string
	project      *Project
	unserialized []string
}

func (s *hubPolicyScheduling) ConnectorForProject(string) (connector.Connector, bool) {
	return fakehub.New(fakehub.Config{}), true
}

func (s *hubPolicyScheduling) CheckProjectPolicy(_ context.Context, _, _ string, descriptor policy.Descriptor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checked = append(s.checked, descriptor.ID)
	if s.project != nil && s.project.configMu.TryLock() {
		s.project.configMu.Unlock()
		s.unserialized = append(s.unserialized, descriptor.ID)
	}
	return nil
}

func (s *hubPolicyScheduling) serialize(p *Project) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.project = p
}

func (s *hubPolicyScheduling) unserializedChecks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.unserialized)
}

func (s *hubPolicyScheduling) checkedSince(index int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.checked[index:])
}

func TestHubConfigurationOwnsRuntimePolicy(t *testing.T) {
	for _, path := range []string{"reconcile", "watcher update"} {
		t.Run(path, func(t *testing.T) {
			root := t.TempDir()
			repository := "---\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\n  api_key: fixture-token\nschedule_ownership:\n  enabled: false\nworkspace:\n  root: " + filepath.Join(root, "workspaces") + "\n---\nRepository instructions changed after approval.\n"
			if err := os.WriteFile(filepath.Join(root, "WORKFLOW.md"), []byte(repository), 0o600); err != nil {
				t.Fatal(err)
			}
			selected := globalconfig.Project{ID: "selected", Workflow: filepath.Join(root, "WORKFLOW.md"), Workdir: root, Weight: 1, Paused: true}
			selected.GlobalWorker.AllowLocalBinding = new(false)
			scheduling := &hubPolicyScheduling{}
			repositoryWorkflow, err := loadWorkflowForScheduling(t.Context(), selected, scheduling)
			if err != nil {
				t.Fatal(err)
			}
			mapped := repositoryWorkflow
			mapped.Config = WithMappedNativeTracker(mapped.Config, scheduling, "selected")
			observed, err := ResolvePolicy(selected, mapped)
			if err != nil {
				t.Fatal(err)
			}
			approvedRoot := filepath.Join(root, "approved")
			if err := os.MkdirAll(approvedRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(approvedRoot, "WORKFLOW.md"), []byte(strings.Replace(repository, "Repository instructions changed after approval.", "Approved instructions.", 1)), 0o600); err != nil {
				t.Fatal(err)
			}
			candidate, err := loadWorkflowForScheduling(t.Context(), globalconfig.Project{ID: "selected", Workflow: filepath.Join(approvedRoot, "WORKFLOW.md"), Workdir: approvedRoot}, scheduling)
			if err != nil {
				t.Fatal(err)
			}
			candidate.Config = WithMappedNativeTracker(candidate.Config, scheduling, "selected")
			approved, err := ResolvePolicy(selected, candidate)
			if err != nil {
				t.Fatal(err)
			}
			if approved.ID == observed.ID {
				t.Fatal("fixture policies must differ")
			}
			ctx, cancel := context.WithCancel(t.Context())
			global := globalconfig.Config{Projects: []globalconfig.Project{selected}}
			manager, err := NewManager(ManagerConfigFromGlobal(global), ManagerDependencies{ProjectFactory: func(project globalconfig.Project) (*Project, error) {
				workflow, err := loadWorkflowForScheduling(ctx, project, scheduling)
				if err != nil {
					return nil, err
				}
				return NewContext(ctx, Config{Project: project, Workflow: workflow}, Dependencies{Runner: &managedRuntimeRunner{}, Scheduling: scheduling, Logger: slog.New(slog.DiscardHandler), WorkflowReconcileInterval: time.Hour, ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) { return fakehub.New(fakehub.Config{}), nil }, WorkflowWatcherFactory: func(string) (WorkflowWatcher, error) { return managedRuntimeWatcher{}, nil }, OrchestratorFactory: func(config orchestrator.Config, deps orchestrator.Dependencies) (*orchestrator.Orchestrator, error) {
					sample := func(context.Context) (hostpressure.Sample, error) { return hostpressure.Sample{}, nil }
					deps.ReadMemoryPressure = sample
					deps.ReadIOPressure = sample
					deps.ReadCPUPressure = sample
					return orchestrator.New(config, deps)
				}})
			}})
			if err != nil {
				cancel()
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cancel()
				manager.Wait()
				for _, p := range manager.Registry().List() {
					if err := p.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			if err := manager.Start(ctx); err != nil {
				t.Fatal(err)
			}
			p, ok := manager.Registry().Get("selected")
			if !ok {
				t.Fatal("selected runtime absent")
			}
			scheduling.serialize(p)
			desired := runnerauth.ProjectConfiguration{ProjectID: "native-selected", Authority: "hub_configuration", ConfigRevision: strings.Repeat("b", 64), Registered: true, Source: "cloud_workflow", EffectivePolicy: &approved, SelectedPolicy: &approved, ObservedAt: time.Now().UTC()}
			result := manager.ApplyHubConfiguration(ctx, "selected", desired)
			if !result.Applied || result.Constraint != "" || !p.Running() {
				t.Fatalf("Hub application=%+v running=%v", result, p.Running())
			}
			if p.Workflow().Config.Policy.ID != approved.ID {
				t.Errorf("resuming the Hub-configured project replaced its policy with %s", p.Workflow().Config.Policy.ID)
			}
			orch := p.Orchestrator()
			checked := len(scheduling.checkedSince(0))
			reload := func() {
				t.Helper()
				switch path {
				case "reconcile":
					if err := p.reconcileWorkflow(ctx); err != nil {
						t.Fatal(err)
					}
				case "watcher update":
					if err := p.handleWorkflowUpdate(ctx, configwatcher.Update{Path: selected.Workflow, Workflow: repositoryWorkflow, At: time.Now().UTC()}); err != nil {
						t.Fatal(err)
					}
				}
			}
			reload()
			if p.Workflow().Config.Policy.ID != approved.ID {
				t.Errorf("repository %s replaced the Hub-applied policy with %s", path, p.Workflow().Config.Policy.ID)
			}
			if pinned := scheduling.checkedSince(checked); slices.Contains(pinned, observed.ID) {
				t.Errorf("repository %s pinned claims to the unapproved policy: %v", path, pinned)
			}
			selected.Paused = false
			global.Projects = []globalconfig.Project{selected}
			reconciled, err := manager.Reconcile(ctx, ManagerConfigFromGlobal(global))
			if err != nil {
				t.Fatal(err)
			}
			if current, _ := manager.Registry().Get("selected"); len(reconciled.Changed) != 0 || current != p || p.Orchestrator() != orch {
				t.Errorf("runner project refresh restarted the Hub-configured project: %+v", reconciled)
			}
			result = manager.ApplyHubConfiguration(ctx, "selected", desired)
			if !result.Applied || result.EffectivePolicy == nil || result.EffectivePolicy.ID != approved.ID || p.Workflow().Config.Policy.ID != approved.ID {
				t.Errorf("next Hub application=%+v policy=%s", result, p.Workflow().Config.Policy.ID)
			}
			if result.SelectedPolicy == nil || result.SelectedPolicy.ID != observed.ID || result.Source != "configured_local_workflow_read_only" {
				t.Errorf("repository policy was not reported as observed: selected=%v source=%s", result.SelectedPolicy, result.Source)
			}
			if p.Orchestrator() != orch || !p.Running() {
				t.Error("Hub application restarted the project")
			}
			repositoryApproved := desired
			repositoryApproved.ConfigRevision = strings.Repeat("c", 64)
			repositoryApproved.EffectivePolicy, repositoryApproved.SelectedPolicy = &observed, &observed
			if result := manager.ApplyHubConfiguration(ctx, "selected", repositoryApproved); !result.Applied || p.Workflow().Config.Policy.ID != observed.ID {
				t.Fatalf("approved repository policy application=%+v policy=%s", result, p.Workflow().Config.Policy.ID)
			}
			reload()
			newer := desired
			newer.ConfigRevision = strings.Repeat("d", 64)
			if result := manager.ApplyHubConfiguration(ctx, "selected", newer); !result.Applied || p.Workflow().Config.Policy.ID != approved.ID {
				t.Fatalf("newer approved policy application=%+v policy=%s", result, p.Workflow().Config.Policy.ID)
			}
			if checked := scheduling.checkedSince(0); checked[len(checked)-1] != approved.ID {
				t.Errorf("claims are pinned to %s after the Hub applied %s", checked[len(checked)-1], approved.ID)
			}
			if unserialized := scheduling.unserializedChecks(); len(unserialized) != 0 {
				t.Errorf("policy checks ran outside the configuration owner and can repin claims after a Hub application: %v", unserialized)
			}
		})
	}
}
