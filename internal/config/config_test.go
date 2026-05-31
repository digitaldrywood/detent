package config

import (
	"strings"
	"testing"
)

func TestParseWorkflowFrontmatter(t *testing.T) {
	t.Parallel()

	raw := []byte(`---
tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  project_slug: "PVT_project"
  active_states:
    - Todo
    - In Progress
  state_map:
    Cancelled: Done
  priority_map:
    Urgent: 1
    No priority: null
polling:
  interval_ms: 15000
workspace:
  root: ~/code/symphony-go-workspaces
  auto_branch: false
worker:
  ssh_hosts:
    - worker-1
  max_concurrent_agents_per_host: 2
agent:
  max_concurrent_agents: 5
  max_concurrent_agents_by_state:
    Merging: 1
  dispatch_priority_by_state:
    - Merging
    - Rework
  auto_promote:
    enabled: true
    quiet_seconds: 0
    optout_label: Requires-Human-Review
    allowed_issue_labels:
      - enhancement
  lessons:
    enabled: true
    path: ".symphony/lessons.md"
    max_entries: 5
    recall_n: 2
    postmortem_max_tokens: 256
  skills:
    enabled: true
    path: ".symphony/skills"
    max_skills_in_prompt: 20
codex:
  command: codex app-server
  approval_policy: never
  thread_sandbox: danger-full-access
  turn_sandbox_policy:
    type: dangerFullAccess
    networkAccess: true
  turn_timeout_ms: 600000
  read_timeout_ms: 1000
  stall_timeout_ms: 0
server:
  host: 0.0.0.0
  port: 4001
observability:
  dashboard_enabled: false
  refresh_ms: 2000
  render_interval_ms: 32
budget:
  enabled: true
  per_day_max_usd: 25
  per_issue_max_usd: 5
  refusal_cooldown_seconds: 30
  pricing_path: priv/pricing/models.yaml
hooks:
  after_create: git clone .
  before_run: echo before
  after_run: echo after
  before_remove: echo remove
  timeout_ms: 30000
---
Ticket prompt {{ issue.title }}
`)

	workflow, err := ParseWorkflow(raw)
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}

	cfg := workflow.Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	if workflow.Prompt != "Ticket prompt {{ issue.title }}\n" {
		t.Fatalf("Prompt = %q", workflow.Prompt)
	}
	if cfg.Tracker.Kind != TrackerGitHub {
		t.Fatalf("Tracker.Kind = %q, want %q", cfg.Tracker.Kind, TrackerGitHub)
	}
	if cfg.Tracker.Endpoint != "https://api.github.com/graphql" {
		t.Fatalf("Tracker.Endpoint = %q", cfg.Tracker.Endpoint)
	}
	if got := cfg.Tracker.StateMap.Map["Cancelled"]; got != "Done" {
		t.Fatalf("Tracker.StateMap[Cancelled] = %v, want Done", got)
	}
	if got := cfg.Tracker.PriorityMap.Map["No priority"]; got != nil {
		t.Fatalf("Tracker.PriorityMap[No priority] = %v, want nil", got)
	}
	if got := cfg.Agent.MaxConcurrentAgentsByState["merging"]; got != 1 {
		t.Fatalf("Agent.MaxConcurrentAgentsByState[merging] = %d, want 1", got)
	}
	if got := cfg.Agent.DispatchPriorityByState; len(got) != 2 || got[0] != "merging" || got[1] != "rework" {
		t.Fatalf("Agent.DispatchPriorityByState = %#v", got)
	}
	if cfg.Agent.AutoPromote.OptoutLabel != "requires-human-review" {
		t.Fatalf("Agent.AutoPromote.OptoutLabel = %q", cfg.Agent.AutoPromote.OptoutLabel)
	}
	if !cfg.Codex.ApprovalPolicy.IsString || cfg.Codex.ApprovalPolicy.String != "never" {
		t.Fatalf("Codex.ApprovalPolicy = %#v, want string never", cfg.Codex.ApprovalPolicy)
	}
	if got := cfg.Codex.TurnSandboxPolicy["networkAccess"]; got != true {
		t.Fatalf("Codex.TurnSandboxPolicy[networkAccess] = %v, want true", got)
	}
	if !cfg.Budget.Enabled {
		t.Fatal("Budget.Enabled = false, want true")
	}
	if cfg.Hooks.AfterCreate != "git clone ." {
		t.Fatalf("Hooks.AfterCreate = %q", cfg.Hooks.AfterCreate)
	}
}

func TestParseWorkflowDefaults(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte("---\ntracker:\n  kind: memory\n---\nPrompt\n"))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}

	cfg := workflow.Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	if cfg.Tracker.Endpoint != "https://api.linear.app/graphql" {
		t.Fatalf("Tracker.Endpoint = %q", cfg.Tracker.Endpoint)
	}
	if cfg.Polling.IntervalMS != 30000 {
		t.Fatalf("Polling.IntervalMS = %d", cfg.Polling.IntervalMS)
	}
	if cfg.Workspace.AutoBranch != true {
		t.Fatal("Workspace.AutoBranch = false, want true")
	}
	if cfg.Agent.MaxConcurrentAgents != 10 {
		t.Fatalf("Agent.MaxConcurrentAgents = %d, want 10", cfg.Agent.MaxConcurrentAgents)
	}
	if cfg.Agent.Lessons.Path != ".symphony/lessons.md" {
		t.Fatalf("Agent.Lessons.Path = %q", cfg.Agent.Lessons.Path)
	}
	if cfg.Agent.Skills.Path != ".symphony/skills" {
		t.Fatalf("Agent.Skills.Path = %q", cfg.Agent.Skills.Path)
	}
	if !cfg.Codex.ApprovalPolicy.IsMap {
		t.Fatalf("Codex.ApprovalPolicy = %#v, want map default", cfg.Codex.ApprovalPolicy)
	}
	if cfg.Server.Host != "127.0.0.1" {
		t.Fatalf("Server.Host = %q", cfg.Server.Host)
	}
	if !cfg.Observability.DashboardEnabled {
		t.Fatal("Observability.DashboardEnabled = false, want true")
	}
	if cfg.Budget.PricingPath != "priv/pricing/models.yaml" {
		t.Fatalf("Budget.PricingPath = %q", cfg.Budget.PricingPath)
	}
}

func TestParseWorkflowNormalizesGitHubAppIDs(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: github
  api_key: token
  project_slug: PVT_project
  github_app_id: 12345
  github_app_installation_id: 67890
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}

	if workflow.Config.Tracker.GitHubAppID != "12345" {
		t.Fatalf("Tracker.GitHubAppID = %q, want 12345", workflow.Config.Tracker.GitHubAppID)
	}
	if workflow.Config.Tracker.GitHubAppInstallationID != "67890" {
		t.Fatalf("Tracker.GitHubAppInstallationID = %q, want 67890", workflow.Config.Tracker.GitHubAppInstallationID)
	}
}

func TestConfigValidateAcceptsGitHubAppCredentials(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: github
  project_slug: PVT_project
  github_app_id: 12345
  github_app_private_key_path: .symphony/github-app.pem
  github_app_installation_id: 67890
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}

	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestStringOrMapFieldsAcceptScalarOrMapping(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
  state_map: $STATE_MAP_JSON
  priority_map:
    P0: 1
    P1: 2
codex:
  approval_policy:
    allow:
      - tool: shell
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}

	if !workflow.Config.Tracker.StateMap.IsString {
		t.Fatalf("Tracker.StateMap = %#v, want string", workflow.Config.Tracker.StateMap)
	}
	if workflow.Config.Tracker.StateMap.String != "$STATE_MAP_JSON" {
		t.Fatalf("Tracker.StateMap.String = %q", workflow.Config.Tracker.StateMap.String)
	}
	if got := workflow.Config.Tracker.PriorityMap.Map["P1"]; got != 2 {
		t.Fatalf("Tracker.PriorityMap[P1] = %v, want 2", got)
	}
	if got := workflow.Config.Codex.ApprovalPolicy.Map["allow"].([]any)[0].(map[string]any)["tool"]; got != "shell" {
		t.Fatalf("Codex.ApprovalPolicy allow tool = %v, want shell", got)
	}
}

func TestConfigValidateReportsInvalidSettings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{
			name: "missing tracker kind",
			raw:  "---\ntracker: {}\n---\nPrompt\n",
			want: []string{"tracker.kind is required"},
		},
		{
			name: "unsupported tracker kind",
			raw:  "---\ntracker:\n  kind: jira\n---\nPrompt\n",
			want: []string{"tracker.kind must be one of"},
		},
		{
			name: "linear credentials",
			raw:  "---\ntracker:\n  kind: linear\n---\nPrompt\n",
			want: []string{"tracker.api_key is required for linear", "tracker.project_slug is required for linear"},
		},
		{
			name: "github credentials",
			raw:  "---\ntracker:\n  kind: github\n---\nPrompt\n",
			want: []string{"tracker.api_key or GitHub App credentials are required for github", "tracker.project_slug is required for github"},
		},
		{
			name: "partial github app credentials",
			raw: `---
tracker:
  kind: github
  project_slug: PVT_project
  github_app_id: 12345
---
Prompt
`,
			want: []string{
				"tracker.github_app_installation_id is required for github app",
				"tracker.github_app_private_key or tracker.github_app_private_key_path is required for github app",
			},
		},
		{
			name: "positive numbers and states",
			raw: `---
tracker:
  kind: memory
  active_states: ["Todo", ""]
polling:
  interval_ms: 0
worker:
  max_concurrent_agents_per_host: 0
agent:
  max_concurrent_agents: 0
  max_concurrent_agents_by_state:
    Todo: 0
  dispatch_priority_by_state: ["Todo", "Todo"]
codex:
  turn_timeout_ms: 0
hooks:
  timeout_ms: 0
observability:
  refresh_ms: 0
budget:
  per_day_max_usd: 0
server:
  port: -1
---
Prompt
`,
			want: []string{
				"tracker.active_states state names must not be blank",
				"polling.interval_ms must be greater than 0",
				"worker.max_concurrent_agents_per_host must be greater than 0",
				"agent.max_concurrent_agents must be greater than 0",
				"agent.max_concurrent_agents_by_state limits must be positive integers",
				"agent.dispatch_priority_by_state state names must be unique",
				"codex.turn_timeout_ms must be greater than 0",
				"hooks.timeout_ms must be greater than 0",
				"observability.refresh_ms must be greater than 0",
				"budget.per_day_max_usd must be greater than 0",
				"server.port must be greater than or equal to 0",
			},
		},
		{
			name: "invalid paths and priority map",
			raw: `---
tracker:
  kind: memory
  priority_map:
    "": 1
    Bad: 5
agent:
  lessons:
    path: ../lessons.md
  skills:
    path: /tmp/skills
---
Prompt
`,
			want: []string{
				"tracker.priority_map option names must not be blank",
				"tracker.priority_map ranks must be integers 1 through 4 or null",
				"agent.lessons.path must be a relative path inside the workspace",
				"agent.skills.path must be a relative path inside the workspace",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			workflow, err := ParseWorkflow([]byte(tt.raw))
			if err != nil {
				t.Fatalf("ParseWorkflow() error = %v", err)
			}

			err = workflow.Config.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Validate() error = %q, want substring %q", err, want)
				}
			}
		})
	}
}

func TestParseWorkflowReportsInvalidFrontmatter(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "missing frontmatter", raw: "Prompt only\n", want: "missing YAML frontmatter"},
		{name: "unterminated frontmatter", raw: "---\ntracker:\n  kind: memory\n", want: "unterminated YAML frontmatter"},
		{name: "invalid yaml", raw: "---\ntracker: [\n---\nPrompt\n", want: "parse YAML frontmatter"},
		{name: "not a map", raw: "---\n- tracker\n---\nPrompt\n", want: "workflow frontmatter must be a mapping"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseWorkflow([]byte(tt.raw))
			if err == nil {
				t.Fatal("ParseWorkflow() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseWorkflow() error = %q, want substring %q", err, tt.want)
			}
		})
	}
}
