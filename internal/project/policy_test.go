package project

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/activehours"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type policyTestScheduling struct {
	testSchedulingSource
	approved map[string]policy.Descriptor
}

type mappedPolicyScheduling struct {
	testSchedulingSource
	approved policy.Descriptor
	observed policy.Descriptor
}

func (s *mappedPolicyScheduling) ConnectorForProject(string) (connector.Connector, bool) {
	return memory.New(memory.Config{}), true
}

func (s *mappedPolicyScheduling) CheckProjectPolicy(_ context.Context, _, _ string, descriptor policy.Descriptor) error {
	s.observed = descriptor
	return descriptor.Match(s.approved)
}

func TestMappedNativeStartupUsesInspectedPolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, feature, wantError string
	}{
		{name: "supported workflow"},
		{name: "intake needs migration", feature: "intake:\n  sources:\n    - name: errors\n      kind: webhook\n      secret: test-secret\n      creates:\n        status: Backlog\n", wantError: "intake.sources"},
		{name: "routines need migration", feature: "schedule_ownership:\n  enabled: true\n  key: acme/orders\n  repository: acme/orders\nroutines:\n  - name: audit\n    schedule: '0 * * * *'\n    prompt: Inspect.\n", wantError: "routines"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := globalconfig.Project{ID: "orders", Workdir: t.TempDir()}
			workflow, err := workflowconfig.ParseWorkflow([]byte("---\ntracker:\n  kind: github\n  project_slug: PVT_test\n  repository: acme/orders\n  api_key: test-token\n" + test.feature + "---\nPrompt\n"))
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

func TestMappedNativeReloadRetainsPolicySchedulingSource(t *testing.T) {
	t.Parallel()
	updated, err := workflowconfig.ParseWorkflow([]byte("---\ntracker:\n  kind: github\n  project_slug: PVT_test\n  repository: acme/orders\n  api_key: test-token\nintake:\n  sources:\n    - name: errors\n      kind: webhook\n      secret: test-secret\n      creates:\n        status: Backlog\n---\nPrompt\n"))
	if err != nil {
		t.Fatal(err)
	}
	// A local tracker does not use Hub scheduling for dispatch, but the
	// unfiltered source still determines how a changed workflow is mapped.
	scheduling := &mappedPolicyScheduling{}
	p := &Project{
		id:               "orders",
		cfg:              globalconfig.Project{ID: "orders", Workdir: t.TempDir()},
		workflow:         workflowconfig.Workflow{Config: workflowconfig.Config{Policy: policy.Descriptor{ID: "approved-local"}}},
		policyScheduling: scheduling,
		logger:           slog.Default(),
	}
	if p.orchDeps.Scheduling != nil {
		t.Fatal("local tracker unexpectedly has Hub dispatch scheduling")
	}
	err = p.handleWorkflowUpdate(t.Context(), configwatcher.Update{Path: "WORKFLOW.md", Workflow: updated})
	if err == nil || !strings.Contains(err.Error(), "intake.sources") || !strings.Contains(err.Error(), "migrate") {
		t.Fatalf("mapped reload error = %v, want native intake migration", err)
	}
}

func (s *policyTestScheduling) CheckProjectPolicy(_ context.Context, project, _ string, descriptor policy.Descriptor) error {
	return descriptor.Match(s.approved[project])
}

func TestProjectPolicyReloadAndGateIsolation(t *testing.T) {
	t.Parallel()
	scheduling := &policyTestScheduling{approved: make(map[string]policy.Descriptor)}
	for _, test := range []struct {
		name, kind string
		automatic  bool
		want       gate.Action
	}{
		{"human", "human_review", false, gate.ActionWait}, {"automatic", "command", true, gate.ActionPass},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := globalconfig.Project{ID: test.name, Workflow: filepath.Join(root, "WORKFLOW.md"), Workdir: root}
			raw := "---\ntracker:\n  kind: github\n  github_status_source: label\n  repository: acme/" + test.name + "\n  api_key: test-token\n---\nPrivate instructions.\n"
			if err := os.WriteFile(cfg.Workflow, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			workflow, err := LoadWorkflow(cfg)
			if err != nil {
				t.Fatal(err)
			}
			workflow.Config.Gate.Kind = test.kind
			workflow.Config.Gate.AutomatedReview = gate.AutomatedReviewOff
			workflow.Config.Agent.AutoPromote.Enabled = test.automatic
			descriptor, err := ResolvePolicy(cfg, workflow)
			if err != nil {
				t.Fatal(err)
			}
			scheduling.approved[cfg.ID] = descriptor
			p, err := New(Config{Project: cfg, Workflow: workflow}, Dependencies{Scheduling: scheduling, Runner: orchestrator.FakeRunner{}, ConnectorFactory: func(workflowconfig.Config) (connector.Connector, error) { return memory.New(memory.Config{}), nil }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := p.Close(); err != nil {
					t.Error(err)
				}
			})
			before := p.Workflow()
			decision := gate.Evaluate(before.Config.Gate, nil, gate.Summary{PullRequestPresent: true, CIStatus: "green"}, time.Now(), gate.EvaluationOptions{})
			if decision.Action != test.want || before.Config.Agent.AutoPromote.Enabled != test.automatic {
				t.Fatalf("repository gate leaked: %#v", decision)
			}
			if err := p.handleWorkflowUpdate(t.Context(), configwatcher.Update{Path: cfg.Workflow, Workflow: workflow}); err != nil {
				t.Fatalf("unchanged approved reload: %v", err)
			}
			if err := p.updateLiveConfig(t.Context(), cfg); err != nil {
				t.Fatalf("unchanged host settings: %v", err)
			}
			changed := cfg
			changed.ActiveHours = &activehours.Config{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}}
			if err := p.updateLiveConfig(t.Context(), changed); err == nil || !strings.Contains(err.Error(), "policy_mismatch") {
				t.Fatalf("active hours policy change = %v", err)
			}
			version := tracker.ChangeVersion{
				ChangeVersionInput: tracker.ChangeVersionInput{PolicyID: descriptor.ID, HeadSHA: strings.Repeat("c", 40)},
				ID:                 "version_pending", Policy: descriptor,
				ReviewPolicy: tracker.ChangeReviewPolicy{PolicyID: descriptor.ID},
			}
			versionBefore, err := json.Marshal(version)
			if err != nil {
				t.Fatal(err)
			}
			for _, pacing := range []workflowconfig.RateWindowPacing{
				{Mode: workflowconfig.RateWindowPacingOff},
				{Mode: workflowconfig.RateWindowPacingFloor, FloorPercent: 35, StaleAfterSeconds: 600},
				workflowconfig.DefaultRateWindowPacing(),
			} {
				changed := cfg
				changed.GlobalRateWindowPacing = pacing.Normalized()
				if err := p.updateLiveConfig(t.Context(), changed); err != nil {
					t.Fatalf("approved pacing reload = %v", err)
				}
				current := p.Workflow()
				if current.Config.Agent.RateWindowPacing != changed.GlobalRateWindowPacing || current.Config.Policy.ID != descriptor.ID {
					t.Fatalf("runtime/policy reload = %#v", current.Config.Agent)
				}
				resolved, err := ResolvePolicy(changed, current)
				if err != nil {
					t.Fatal(err)
				}
				if err := version.Policy.Match(resolved); err != nil || version.PolicyID != resolved.ID || version.ReviewPolicy.PolicyID != resolved.ID {
					t.Fatalf("pending version policy changed: %v", err)
				}
				versionAfter, err := json.Marshal(version)
				if err != nil || string(versionAfter) != string(versionBefore) {
					t.Fatalf("immutable pending version changed: %v", err)
				}
			}
			for _, change := range []string{"invalid", "review relaxation", "privileged runner", "model selection", "source instructions", "newly approved revision"} {
				t.Run(change, func(t *testing.T) {
					proposal := workflow
					update := configwatcher.Update{Path: cfg.Workflow, Workflow: proposal}
					switch change {
					case "invalid":
						update.Err = errors.New("invalid YAML")
					case "review relaxation":
						update.Workflow.Config.Gate.Kind = gate.KindArtifact
					case "privileged runner":
						update.Workflow.Config.Runners = workflowconfig.Runners{Profile: "privileged", Profiles: map[string]policy.Requirements{"privileged": {RequiredTags: []string{"production"}}}}
					case "model selection":
						update.Workflow.Config.Agents.ModelSelection.NormalModel = new("gpt-6-sol")
					case "source instructions":
						update.Workflow.SourceHash = policy.Digest([]byte("changed instructions"))
					case "newly approved revision":
						update.Workflow.Definition.Revision = strings.Repeat("b", 40)
						approved, err := ResolvePolicy(cfg, update.Workflow)
						if err != nil {
							t.Fatal(err)
						}
						scheduling.approved[cfg.ID] = approved
					}
					if err := p.handleWorkflowUpdate(t.Context(), update); err == nil {
						t.Fatal("unsafe reload was accepted")
					}
					if got := p.Workflow().Config.Policy.ID; got != descriptor.ID {
						t.Fatalf("last-good policy changed: %s", got)
					}
					if p.WorkflowSourceStatus().LastReloadError == "" {
						t.Fatal("reload mismatch has no actionable diagnostic")
					}
				})
			}
		})
	}
}

func TestTrustedRefIgnoresWorkingBranchPolicyEdits(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Policy Test"}} {
		if _, err := runWorkflowGit(t.Context(), root, args...); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "WORKFLOW.md")
	trusted := "---\ntracker:\n  kind: memory\ngate:\n  kind: human_review\nagent:\n  auto_promote:\n    enabled: false\n---\nTrusted instructions.\n"
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
