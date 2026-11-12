package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestDoctorReviewFlowPhraseMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    string
		phrases []string
		want    int
	}{
		{
			name:    "affirmative enter review instruction",
			text:    "When done, move the issue to Human Review.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
		{
			name:    "affirmative enter review instruction wrapped across lines",
			text:    "When done, move the issue to\nHuman Review.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
		{
			name:    "conditional enter review instruction",
			text:    "Do not move the issue to Human Review until validation passes.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
		{
			name:    "do not enter review instruction",
			text:    "Do NOT move the issue to Human Review.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
		},
		{
			name:    "never enter review instruction",
			text:    "Never move the issue to Human Review.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
		},
		{
			name:    "negated skip review instruction",
			text:    "Do not skip Human Review.",
			phrases: doctorReviewFlowSkipReviewPhrases("Human Review"),
		},
		{
			name:    "negated direct promotion instruction",
			text:    "Do not promote the issue directly to Merging.",
			phrases: doctorReviewFlowDirectPromotePhrases("Merging"),
		},
		{
			name: "mixed document",
			text: "Do not move the issue to Human Review.\n" +
				"When explicitly requested, move the issue to Human Review.",
			phrases: doctorReviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := doctorReviewFlowPhraseMatches(tt.text, tt.phrases); got != tt.want {
				t.Fatalf("doctorReviewFlowPhraseMatches() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestDoctorWorkflowWorkerModelChoiceMode(t *testing.T) {
	t.Parallel()

	pinned := workflowconfig.Config{}
	pinned.Agents.Routes = []workflowconfig.AgentRoute{{
		Name:    "default",
		Backend: workflowconfig.DefaultAgentBackendID,
		Model:   "gpt-5.6-sol",
		Default: true,
	}}
	for _, tc := range []struct {
		name string
		cfg  workflowconfig.Config
		want string
	}{
		{name: "pinned", cfg: pinned, want: "pinned"},
		{name: "unpinned", cfg: workflowconfig.Config{}, want: "provider_default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := doctorWorkflowWorkerModelChoice(tc.cfg).Mode; got != tc.want {
				t.Fatalf("model choice = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDoctorWorkflowOptimizationJSONReportSchema(t *testing.T) {
	t.Parallel()

	report := doctorReport{
		Checks: []doctorCheck{{
			Name:   "Project detent repository merge policy",
			Status: doctorWarn,
			Detail: "1 finding",
		}},
		WorkflowOptimization: doctorWorkflowOptimizationReport{
			Findings: []doctorWorkflowOptimizationFinding{{
				RuleID:               doctorWorkflowRuleRunawaySessionTokens,
				ProjectID:            "detent",
				Severity:             "warning",
				Title:                "Runaway session tail",
				Detail:               "max session tokens are high",
				EstimatedTokenImpact: 290000,
				Evidence: map[string]any{
					"max_session_tokens": int64(300000),
				},
			}},
		},
	}

	var output bytes.Buffer
	if err := writeDoctorReport(&output, report, OutputFormatJSON); err != nil {
		t.Fatalf("writeDoctorReport() error = %v", err)
	}

	var got struct {
		Result               string `json:"result"`
		WorkflowOptimization struct {
			Findings []struct {
				RuleID string         `json:"rule_id"`
				Detail string         `json:"detail"`
				Values map[string]any `json:"evidence"`
			} `json:"findings"`
		} `json:"workflow_optimization"`
	}
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal() error = %v\n%s", err, output.String())
	}
	if got.Result != "PASS" {
		t.Fatalf("Result = %q, want PASS for advisory findings", got.Result)
	}
	if len(got.WorkflowOptimization.Findings) != 1 || got.WorkflowOptimization.Findings[0].RuleID != doctorWorkflowRuleRunawaySessionTokens {
		t.Fatalf("workflow optimization findings = %#v", got.WorkflowOptimization.Findings)
	}
}

func TestDoctorWorkflowOptimizationStrictFailsOnAdvisoryFindings(t *testing.T) {
	t.Parallel()

	paths := writeDoctorWorkflowOptimizationFixture(t)
	configPath := filepath.Join(paths.dir, "global.yaml")
	global := doctorWorkflowOptimizationGlobal(paths)
	global.Path = configPath

	deps := successfulDoctorDeps()
	deps.loadWorkflow = workflowconfig.LoadWorkflow

	configFlag := configPath
	envFlag := ""
	logLevelFlag := ""
	hostFlag := "127.0.0.1"
	portFlag := 0
	cmd := newDoctorCommandWithDeps(&configFlag, &envFlag, &logLevelFlag, &hostFlag, &portFlag, successfulDoctorOptionsWithConfig(configPath, global), deps)
	cmd.SetArgs([]string{"--project", "detent", "--strict"})
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	if !errors.Is(err, ErrDoctorFailed) {
		t.Fatalf("Execute() error = %v, want ErrDoctorFailed for strict advisory findings\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	for _, want := range []string{"Workflow Optimization", "Summary:", "0 FAIL", "Result: FAIL"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDoctorWorkflowDefaultRouteModelConfigSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantOK     bool
		wantModel  string
		wantSource string
	}{
		{
			name: "route model",
			raw: `---
tracker:
  kind: hub_native
  api_key: fixture-token
  repository: example-org/example-project
agents:
  backends:
    - id: codex-main
      kind: codex
      command: codex app-server
  routes:
    - name: default
      backend: codex-main
      model: gpt-5-route
      default: true
---
Prompt
`,
			wantOK:     true,
			wantModel:  "gpt-5-route",
			wantSource: "agents.routes.model",
		},
		{
			name: "backend command config model",
			raw: `---
tracker:
  kind: hub_native
  api_key: fixture-token
  repository: example-org/example-project
agents:
  backends:
    - id: codex-main
      kind: codex
      command: codex --config=model=\"gpt-5.5\" app-server
  routes:
    - name: default
      backend: codex-main
      default: true
---
Prompt
`,
			wantOK:     true,
			wantModel:  "gpt-5.5",
			wantSource: "agents.backends.command",
		},
		{
			name: "backend command config model with issue field overrides",
			raw: `---
tracker:
  kind: hub_native
  api_key: fixture-token
  repository: example-org/example-project
agents:
  backends:
    - id: codex-main
      kind: codex
      command: codex --config=model="gpt-5.5" app-server
  routes:
    - name: default
      backend: codex-main
      model_field: Model
      default: true
---
Prompt
`,
			wantOK:     true,
			wantModel:  "gpt-5.5",
			wantSource: "agents.backends.command",
		},
		{
			name: "route model field",
			raw: `---
tracker:
  kind: hub_native
  api_key: fixture-token
  repository: example-org/example-project
agents:
  backends:
    - id: codex-main
      kind: codex
      command: codex app-server
  routes:
    - name: default
      backend: codex-main
      model_field: Model
      default: true
---
Prompt
`,
			wantOK:     true,
			wantSource: "agents.routes.model_field",
		},
		{
			name: "no model source",
			raw: `---
tracker:
  kind: hub_native
  api_key: fixture-token
  repository: example-org/example-project
codex:
  command: codex app-server
---
Prompt
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflow, err := workflowconfig.ParseWorkflow([]byte(tt.raw))
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}
			got, ok := doctorWorkflowDefaultRouteModelConfig(workflow.Config)
			if ok != tt.wantOK {
				t.Fatalf("doctorWorkflowDefaultRouteModelConfig() ok = %v, want %v; config = %#v", ok, tt.wantOK, got)
			}
			if got.Model != tt.wantModel {
				t.Fatalf("doctorWorkflowDefaultRouteModelConfig() model = %q, want %q", got.Model, tt.wantModel)
			}
			if got.Source != tt.wantSource {
				t.Fatalf("doctorWorkflowDefaultRouteModelConfig() source = %q, want %q", got.Source, tt.wantSource)
			}
		})
	}
}

type doctorWorkflowOptimizationFixturePaths struct {
	dir      string
	db       string
	workflow string
}

func writeDoctorWorkflowOptimizationFixture(t *testing.T) doctorWorkflowOptimizationFixturePaths {
	t.Helper()

	dir := t.TempDir()
	workflowPath := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(`---
tracker:
  kind: hub_native
  lanes:
    - name: Backlog
      role: holding
    - name: Human Review
      role: holding
    - name: Blocked
      role: holding
    - name: Todo
      role: active
    - name: In Progress
      role: active
    - name: Rework
      role: active
    - name: Merging
      role: active
    - name: Closed
      role: terminal
    - name: Cancelled
      role: terminal
    - name: Canceled
      role: terminal
    - name: Duplicate
      role: terminal
    - name: Done
      role: terminal
polling:
  interval_ms: 60000
budget:
  enabled: true
agent:
  auto_promote:
    enabled: true
gate:
  kind: command
  validator:
    enabled: true
---
Prompt

Never move the issue to Human Review.
`), 0o600); err != nil {
		t.Fatalf("WriteFile(WORKFLOW.md) error = %v", err)
	}

	return doctorWorkflowOptimizationFixturePaths{dir: dir, db: filepath.Join(dir, "detent.db"), workflow: workflowPath}
}

func doctorWorkflowOptimizationGlobal(paths doctorWorkflowOptimizationFixturePaths) globalconfig.Config {
	return globalconfig.Config{
		Path:       filepath.Join(paths.dir, "global.yaml"),
		APIVersion: globalconfig.APIVersion,
		Kind:       globalconfig.Kind,
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 1,
			Scheduling:          globalconfig.SchedulingWeighted,
		},
		Projects: []globalconfig.Project{{
			ID:       "detent",
			Workflow: paths.workflow,
			Workdir:  paths.dir,
			Weight:   1,
		}},
	}
}

func doctorWorkflowFindingByRule(t *testing.T, findings []doctorWorkflowOptimizationFinding, ruleID string) doctorWorkflowOptimizationFinding {
	t.Helper()

	for _, finding := range findings {
		if finding.RuleID == ruleID {
			return finding
		}
	}
	t.Fatalf("missing finding %q in %#v", ruleID, findings)
	return doctorWorkflowOptimizationFinding{}
}

func doctorWorkflowProposalBySignal(t *testing.T, proposals []doctorWorkflowImprovementProposal, signalKind string, pattern string) doctorWorkflowImprovementProposal {
	t.Helper()

	for _, proposal := range proposals {
		if proposal.SignalKind == signalKind && proposal.Pattern == pattern {
			return proposal
		}
	}
	t.Fatalf("missing proposal signal=%q pattern=%q in %#v", signalKind, pattern, proposals)
	return doctorWorkflowImprovementProposal{}
}
