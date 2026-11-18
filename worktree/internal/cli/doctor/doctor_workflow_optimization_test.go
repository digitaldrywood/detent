package doctor

import (
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
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
			phrases: reviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
		{
			name:    "affirmative enter review instruction wrapped across lines",
			text:    "When done, move the issue to\nHuman Review.",
			phrases: reviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
		{
			name:    "conditional enter review instruction",
			text:    "Do not move the issue to Human Review until validation passes.",
			phrases: reviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
		{
			name:    "do not enter review instruction",
			text:    "Do NOT move the issue to Human Review.",
			phrases: reviewFlowEnterReviewPhrases("Human Review"),
		},
		{
			name:    "never enter review instruction",
			text:    "Never move the issue to Human Review.",
			phrases: reviewFlowEnterReviewPhrases("Human Review"),
		},
		{
			name:    "negated skip review instruction",
			text:    "Do not skip Human Review.",
			phrases: reviewFlowSkipReviewPhrases("Human Review"),
		},
		{
			name:    "negated direct promotion instruction",
			text:    "Do not promote the issue directly to Merging.",
			phrases: reviewFlowDirectPromotePhrases("Merging"),
		},
		{
			name: "mixed document",
			text: "Do not move the issue to Human Review.\n" +
				"When explicitly requested, move the issue to Human Review.",
			phrases: reviewFlowEnterReviewPhrases("Human Review"),
			want:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := reviewFlowPhraseMatches(tt.text, tt.phrases); got != tt.want {
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

			if got := WorkflowWorkerModelChoice(tc.cfg).Mode; got != tc.want {
				t.Fatalf("model choice = %q, want %q", got, tc.want)
			}
		})
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
			got, ok := workflowDefaultRouteModelConfig(workflow.Config)
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
