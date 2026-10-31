package project

import (
	"context"

	"errors"
	"fmt"

	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/testutil/fakehub"
)

type policyTestScheduling struct {
	testSchedulingSource
	approved map[string]policy.Descriptor
}

type mappedPolicyScheduling struct {
	mu sync.Mutex
	testSchedulingSource
	approved       policy.Descriptor
	observed       policy.Descriptor
	modelSelection workflowconfig.ModelSelection
	checkError     error
}

type cloudPolicyScheduling struct {
	mappedPolicyScheduling
	markdown string
}

func (s *cloudPolicyScheduling) ProjectWorkflowMarkdown(context.Context, string) (string, error) {
	return s.markdown, nil
}

func TestNativeCloudWorkflowFallback(t *testing.T) {
	t.Parallel()
	markdown := "---\ntracker:\n  kind: hub_native\n  lanes:\n    - name: Backlog\n      role: holding\n    - name: Blocked\n      role: holding\n    - name: Human Review\n      role: holding\n    - name: Plan Review\n      role: holding\n    - name: Todo\n      role: active\n    - name: Repair\n      role: active\n    - name: Merging\n      role: active\n    - name: Done\n      role: terminal\n    - name: Cancelled\n      role: terminal\nplan:\n  enabled: true\n---\nCloud instructions.\n"
	for _, test := range []struct {
		name, instructions, config string
		wantCloud                  bool
		wantError                  string
	}{
		{name: "no files", wantCloud: true},
		{name: "instructions only", instructions: "# Agent instructions\n- Todo\n- A prose lane is not configuration\n", wantCloud: true},
		{name: "split takes precedence", instructions: "Repository instructions.\n", config: "schema: 1\ntracker:\n  kind: hub_native\n  lanes:\n    - name: Backlog\n      role: holding\n    - name: Human Review\n      role: holding\n    - name: Blocked\n      role: holding\n    - name: Todo\n      role: active\n    - name: Rework\n      role: active\n    - name: Closed\n      role: terminal\n    - name: Cancelled\n      role: terminal\n    - name: Canceled\n      role: terminal\n    - name: Duplicate\n      role: terminal\n    - name: Done\n      role: terminal\n"},
		{name: "legacy takes precedence", instructions: "---\ntracker:\n  kind: hub_native\n---\nRepository instructions.\n"},
		{name: "invalid supplied definition", instructions: "Repository instructions.\n", config: "schema: 999\ntracker:\n  kind: hub_native\n", wantError: "schema"},
		{name: "mixed supplied definition", instructions: "---\ntracker:\n  kind: hub_native\n---\nInstructions.\n", config: "schema: 1\ntracker:\n  kind: hub_native\n", wantError: "mixed"},
		{name: "unavailable definition directory", wantError: "read definition directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := globalconfig.Project{ID: "cloud-project", Workdir: dir, Workflow: filepath.Join(dir, "WORKFLOW.md")}
			if test.name == "unavailable definition directory" {
				cfg.Workflow = filepath.Join(dir, "missing", "WORKFLOW.md")
			}
			if test.instructions != "" {
				if err := os.WriteFile(cfg.Workflow, []byte(test.instructions), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.config != "" {
				if err := os.WriteFile(filepath.Join(dir, "detent.yaml"), []byte(test.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			scheduling := &cloudPolicyScheduling{markdown: markdown}
			workflow, err := loadWorkflowForScheduling(t.Context(), cfg, scheduling)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("load error = %v, want %s", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (workflow.Definition.Layout == workflowconfig.ProjectDefinitionCloud) != test.wantCloud {
				t.Fatalf("definition authority = %s", workflow.Definition.Layout)
			}
			workflow.Config = MapNativeTracker(workflow.Config, true)
			descriptor, err := ResolvePolicy(cfg, workflow)
			if err != nil {
				t.Fatal(err)
			}
			if (descriptor.Workflow == nil) != test.wantCloud {
				t.Fatalf("repository workflow projection = %#v", descriptor.Workflow)
			}
			scheduling.approved = descriptor
			loaded, err := Load(cfg, Dependencies{Scheduling: scheduling, Runner: orchestrator.FakeRunner{}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := loaded.Close(); err != nil {
					t.Error(err)
				}
			})
			if test.wantCloud && !loaded.Workflow().Config.KanbanTransitionAllowed("Todo", "Repair") {
				t.Fatal("Cloud workflow did not reach runner transition policy")
			}
			if test.wantCloud {
				if _, err := loaded.loadManagedWorkflow(t.Context(), descriptor.SourceRevision); err != nil {
					t.Fatalf("Cloud policy revision cannot be applied: %v", err)
				}
				scheduling.markdown = strings.ReplaceAll(markdown, "Repair", "Updated Repair")
				if _, err := loaded.loadManagedWorkflow(t.Context(), descriptor.SourceRevision); err == nil {
					t.Fatal("stale Cloud policy revision accepted a changed definition")
				}
			}
		})
	}
}

func (s *mappedPolicyScheduling) ConnectorForProject(string) (connector.Connector, bool) {
	return fakehub.New(fakehub.Config{}), true
}

func (s *mappedPolicyScheduling) CheckProjectPolicy(_ context.Context, _, _ string, descriptor policy.Descriptor) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = descriptor
	if s.checkError != nil {
		return s.checkError
	}
	return descriptor.Match(s.approved)
}

func (s *mappedPolicyScheduling) ResolveProjectWorkflow(_ context.Context, _ string, workflow workflowconfig.Workflow, _ *policy.RepositorySource) (workflowconfig.Workflow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelSelection.Configured() {
		workflow.Config.Agents.ModelSelection = s.modelSelection
	}
	if s.approved.Configuration == nil {
		return workflow, nil
	}
	return workflowconfig.ApplyNativePolicy(workflow, s.approved)
}

func TestMappedNativeStartupUsesInspectedPolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, feature, wantError string
	}{
		{name: "supported workflow"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := globalconfig.Project{ID: "orders", Workdir: t.TempDir()}
			workflow, err := workflowconfig.ParseWorkflow([]byte("---\ntracker:\n  kind: hub_native\n  repository: acme/orders\n  api_key: test-token\n" + test.feature + "---\nPrompt\n"))
			if err != nil {
				t.Fatal(err)
			}
			workflow.Definition.Revision = strings.Repeat("a", 40)
			workflow.SourceHash = policy.Digest([]byte("source"))
			inspected := workflow
			inspected.Config = MapNativeTracker(inspected.Config, true)
			descriptor, inspectErr := ResolvePolicy(cfg, inspected)
			if test.wantError != "" {
				if inspectErr == nil || !strings.Contains(inspectErr.Error(), test.wantError) || !strings.Contains(inspectErr.Error(), "migrate") {
					t.Fatalf("inspection error = %v, want %s migration", inspectErr, test.wantError)
				}
			} else if inspectErr != nil {
				t.Fatal(inspectErr)
			}
			scheduling := &mappedPolicyScheduling{approved: descriptor}
			loaded, startupErr := New(Config{Project: cfg, Workflow: workflow}, Dependencies{Scheduling: scheduling, Runner: orchestrator.FakeRunner{}})
			if test.wantError != "" {
				if startupErr == nil || !strings.Contains(startupErr.Error(), test.wantError) || !strings.Contains(startupErr.Error(), "migrate") {
					t.Fatalf("startup error = %v, want %s migration", startupErr, test.wantError)
				}
				return
			}
			if startupErr != nil {
				t.Fatal(startupErr)
			}
			t.Cleanup(func() {
				if err := loaded.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := scheduling.observed.Match(descriptor); err != nil {
				t.Fatalf("startup policy differs from inspected policy: %v", err)
			}
			if loaded.Workflow().Config.Tracker.Kind != workflowconfig.TrackerHubNative {
				t.Fatal("startup did not select native tracker")
			}
		})
	}
}

func (s *policyTestScheduling) CheckProjectPolicy(_ context.Context, project, _ string, descriptor policy.Descriptor) error {
	return descriptor.Match(s.approved[project])
}

func TestHostOverridesApplyWithoutPolicyConfiguration(t *testing.T) {
	t.Parallel()
	for _, override := range []string{"active hours", "model selection", "pricing path"} {
		t.Run(override, func(t *testing.T) {
			t.Parallel()
			cfg := globalconfig.Project{ID: "example-project", Workdir: t.TempDir()}
			workflow := workflowconfig.Workflow{Config: workflowconfig.Default()}
			workflow.Config.Tracker.Kind = workflowconfig.TrackerHubNative
			descriptor, err := ResolvePolicy(cfg, workflow)
			if err != nil {
				t.Fatal(err)
			}
			descriptor.Configuration = nil
			descriptor = descriptor.WithID()
			workflow.Config.Policy = descriptor
			p := &Project{cfg: cfg, workflow: workflow, workflowActiveHours: workflow.Config.ActiveHours}
			changed := cfg
			switch override {
			case "active hours":
				changed.ActiveHours = &activehours.Config{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}}
			case "model selection":
				changed.ModelSelection = &workflowconfig.ModelSelection{NormalModel: new("gpt-6.1-sol")}
			case "pricing path":
				changed.GlobalBudget.PricingPath = new(filepath.Join(cfg.Workdir, "pricing.json"))
			}
			if err := p.updateLiveConfig(t.Context(), changed); err != nil {
				t.Fatalf("host override did not apply live: %v", err)
			}
			current := p.Workflow()
			if current.Config.Policy.ID != descriptor.ID || current.Config.Policy.Configuration != nil {
				t.Fatalf("host override changed policy: %+v", current.Config.Policy)
			}
			switch override {
			case "active hours":
				if !reflect.DeepEqual(current.Config.ActiveHours, changed.ActiveHours.Normalize()) || !reflect.DeepEqual(p.DispatchCandidate().ActiveHours, changed.ActiveHours.Normalize()) {
					t.Fatalf("active hours not applied: %+v", current.Config.ActiveHours)
				}
			case "model selection":
				if current.Config.EffectiveModelSelection().Model("normal") != "gpt-6.1-sol" {
					t.Fatalf("model selection not applied: %+v", current.Config.Agents.ModelSelection)
				}
			case "pricing path":
				if current.Config.Budget.PricingPath != *changed.GlobalBudget.PricingPath {
					t.Fatalf("pricing path not applied: %s", current.Config.Budget.PricingPath)
				}
			}
		})
	}
}

func TestTrustedRefIgnoresWorkingBranchPolicyEdits(t *testing.T) {
	if testing.Short() {
		t.Skip("real Git integration")
	}

	t.Parallel()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Policy Test"}} {
		if _, err := runWorkflowGit(t.Context(), root, args...); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "WORKFLOW.md")
	trusted := "---\ntracker:\n  kind: hub_native\ngate:\n  kind: human_review\nagent:\n  auto_promote:\n    enabled: false\n---\nTrusted instructions.\n"
	if err := os.WriteFile(path, []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "WORKFLOW.md"}, {"commit", "-m", "test: add approved workflow"}, {"checkout", "-b", "untrusted"}} {
		if _, err := runWorkflowGit(t.Context(), root, args...); err != nil {
			t.Fatal(err)
		}
	}
	cfg := globalconfig.Project{ID: "trusted", Workflow: "WORKFLOW.md", WorkflowRef: "refs/heads/main", Workdir: root}
	workflow, err := LoadWorkflow(cfg)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := ResolvePolicy(cfg, workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(trusted, "human_review", "command")), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadWorkflow(cfg)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := ResolvePolicy(cfg, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if err := actual.Match(approved); err != nil {
		t.Fatalf("untrusted shared file changed the trusted ref: %v", err)
	}
	local := "---\ngate:\n  kind: command\nagent:\n  auto_promote:\n    enabled: true\nrunners:\n  profile: privileged\n  profiles:\n    privileged:\n      required_tags: [production]\n---\nRelax review.\n"
	if err := os.WriteFile(workflowconfig.LocalWorkflowPath(path), []byte(local), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadWorkflow(cfg)
	if err != nil {
		t.Fatal(err)
	}
	actual, err = ResolvePolicy(cfg, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Match(approved) == nil {
		t.Fatal("untrusted local overlay relaxed active policy")
	}
}

func TestNativeSharedPolicyReloadsRunningProject(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "WORKFLOW.md")
	raw := []byte("---\ntracker:\n  kind: hub_native\nworker:\n  ssh_hosts: [local]\ngate:\n  run: true\n---\nKeep human review holds.\n")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := globalconfig.Project{ID: "shared", Workdir: root, Workflow: path}
	workflow, err := LoadWorkflow(cfg)
	if err != nil {
		t.Fatal(err)
	}
	workflow.Authored.Version = 1
	approved, err := ResolvePolicy(cfg, workflow)
	if err != nil {
		t.Fatal(err)
	}
	scheduling := &mappedPolicyScheduling{approved: approved}
	attempts, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: filepath.Join(root, "runtime.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := attempts.Close(); err != nil {
			t.Error(err)
		}
	})
	p, err := New(Config{Project: cfg, Workflow: workflow}, Dependencies{Scheduling: scheduling, Runner: orchestrator.FakeRunner{}, WorkAttempts: attempts, WorkflowReconcileInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := p.start(t.Context(), startOptions{}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	attemptID, err := attempts.StartWorkAttempt(t.Context(), store.WorkAttemptStart{ProjectID: cfg.ID, IssueID: "canonical-upgrade", WorkerType: "agent", StartedAt: started, LeaseExpiresAt: started.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	upgraded := workflow
	authored := *workflow.Authored
	authored.Version = workflowconfig.PolicyCanonicalizationVersion
	upgraded.Authored = &authored
	upgradedPolicy, err := ResolvePolicy(cfg, upgraded)
	if err != nil {
		t.Fatal(err)
	}
	scheduling.mu.Lock()
	scheduling.approved = upgradedPolicy
	scheduling.mu.Unlock()
	for _, tick := range []struct {
		name             string
		pending, catalog bool
	}{
		{name: "provider catalog unavailable", catalog: true},
		{name: "first rejection", pending: true},
		{name: "still pending", pending: true},
		{name: "approved"},
	} {
		t.Run(tick.name, func(t *testing.T) {
			scheduling.mu.Lock()
			scheduling.checkError = nil
			if tick.pending {
				scheduling.checkError = &hubclient.APIError{Status: http.StatusConflict, Code: "policy_mismatch", Message: "repository policy approval pending"}
			}
			if tick.catalog {
				scheduling.checkError = errors.Join(orchestrator.ErrSchedulingUnavailable, fmt.Errorf("%w: provider models for backend codex: wait for initialize response: context deadline exceeded", providercapacity.ErrCatalogUnavailable))
			}
			scheduling.mu.Unlock()
			err := p.reconcileWorkflow(t.Context())
			reloadError := p.WorkflowSourceStatus().LastReloadError
			if tick.catalog {
				if !errors.Is(err, providercapacity.ErrCatalogUnavailable) || !strings.HasPrefix(reloadError, ProviderStartupFailed+": ") || strings.Contains(reloadError, "repository policy reload rejected") || p.Workflow().Config.Policy.ID != approved.ID {
					t.Fatalf("provider startup failure was reported as a policy rejection: %q, %v", reloadError, err)
				}
			} else if tick.pending {
				var failure *hubclient.APIError
				if !errors.As(err, &failure) || failure.Status != http.StatusConflict || failure.Code != "policy_mismatch" || p.Workflow().Config.Policy.ID != approved.ID || !strings.HasPrefix(reloadError, "repository policy reload rejected: ") {
					t.Fatalf("pending approval changed effective policy: %s, %q, %v", p.Workflow().Config.Policy.ID, reloadError, err)
				}
			} else if err != nil || p.Workflow().Config.Policy.ID != upgradedPolicy.ID || p.WorkflowSourceStatus().LastReloadError != "" {
				t.Fatalf("approved unchanged selection was not applied on the next tick: %s, %v", p.Workflow().Config.Policy.ID, err)
			}
		})
	}
	active, err := attempts.ListActiveWorkAttempts(t.Context(), store.WorkAttemptQuery{ProjectID: cfg.ID})
	if err != nil || len(active) != 1 || active[0].ID != attemptID || p.Workflow().Config.Policy.ID != upgradedPolicy.ID || !p.Running() {
		t.Fatalf("canonical upgrade changed active work: %+v %v", active, err)
	}
	changed, err := workflowconfig.ParseWorkflowOverlay(raw, []byte("---\nplan:\n  enabled: true\ngate:\n  validator:\n    enabled: true\nagent:\n  auto_promote:\n    enabled: true\n---\n"), "operator-approved.local.md")
	if err != nil {
		t.Fatal(err)
	}
	changed.Definition = workflow.Definition
	replacement, err := ResolvePolicy(cfg, changed)
	if err != nil {
		t.Fatal(err)
	}
	scheduling.mu.Lock()
	scheduling.approved = replacement
	scheduling.mu.Unlock()
	if err := p.reconcileWorkflow(t.Context()); err != nil {
		t.Fatal(err)
	}
	current := p.Workflow()
	if current.SourceHash != workflow.SourceHash {
		t.Fatal("cloud update changed the local source identity")
	}
	if current.Config.Policy.ID != replacement.ID || !current.Config.Plan.Enabled || !current.Config.Gate.Validator.Enabled || !current.Config.Agent.AutoPromote.Enabled || !p.Running() {
		t.Fatalf("shared policy was not reloaded in the running project: %+v", current.Config.Policy)
	}
	active, err = attempts.ListActiveWorkAttempts(t.Context(), store.WorkAttemptQuery{ProjectID: cfg.ID})
	if err != nil || len(active) != 1 || active[0].ID != attemptID {
		t.Fatalf("changed authored policy interrupted active work: %+v %v", active, err)
	}
	if err := attempts.CompleteWorkAttempt(t.Context(), store.WorkAttemptCompletion{AttemptID: attemptID, CompletedAt: started.Add(time.Second), Status: store.WorkAttemptStatusTerminal, TerminalState: store.WorkAttemptTerminalSuccess}); err != nil {
		t.Fatal(err)
	}
	scheduling.mu.Lock()
	scheduling.modelSelection = workflowconfig.ResolveCloudModelSelection(workflowconfig.ModelSelection{Preset: new("sol_first"), NormalModel: new("gpt-6.1-sol")}, workflowconfig.ModelSelection{})
	scheduling.mu.Unlock()
	if err := p.reconcileWorkflow(t.Context()); err != nil {
		t.Fatal(err)
	}
	current = p.Workflow()
	if current.SourceHash != workflow.SourceHash || current.Config.Policy.ID != replacement.ID || current.Config.EffectiveModelSelection().Model("normal") != "gpt-6.1-sol" {
		t.Fatalf("Cloud selection did not reload without a source or policy change: %+v", current.Config.Agents.ModelSelection)
	}

	host := cfg
	host.GlobalWorker.AllowLocalBinding = new(true)
	host.ActiveHours = &activehours.Config{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}}
	if err := p.updateLiveConfig(t.Context(), host); err != nil {
		t.Fatal(err)
	}
	if p.Workflow().Config.Policy.ID != replacement.ID || p.Workflow().Config.Worker.EffectiveAllowLocalBinding() || p.Workflow().Config.EffectiveModelSelection().Model("normal") != "gpt-6.1-sol" {
		t.Fatal("host reload changed shared permissions or policy identity")
	}
}

func (s *policyTestScheduling) ConnectorForProject(string) (connector.Connector, bool) {
	return fakehub.New(fakehub.Config{}), true
}
