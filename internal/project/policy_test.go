package project

import (
	"context"
	"errors"
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
)

type policyTestScheduling struct {
	testSchedulingSource
	approved map[string]policy.Descriptor
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
			for _, field := range []string{"active hours", "rate pacing"} {
				t.Run(field, func(t *testing.T) {
					changed := cfg
					if field == "active hours" {
						changed.ActiveHours = &activehours.Config{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}}
					} else {
						changed.GlobalRateWindowPacing = workflowconfig.RateWindowPacing{Mode: workflowconfig.RateWindowPacingOff}
					}
					if err := p.updateLiveConfig(t.Context(), changed); err == nil || !strings.Contains(err.Error(), "policy_mismatch") {
						t.Fatalf("host policy change = %v", err)
					}
					if p.Workflow().Config.Policy.ID != descriptor.ID {
						t.Fatal("host reload changed approved policy")
					}
				})
			}
			for _, change := range []string{"invalid", "review relaxation", "privileged runner", "newly approved revision"} {
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
