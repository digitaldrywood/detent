package config

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/digitaldrywood/symphony/internal/connector"
)

func TestParseWorkflowFrontmatter(t *testing.T) {
	t.Parallel()

	raw := []byte(fmt.Sprintf(`---
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
  root: ~/code/symphony-workspaces
  source_root: %s
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
`, initConfigSourceRepo(t)))

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
	if cfg.Workspace.SourceRoot == "" {
		t.Fatal("Workspace.SourceRoot is blank, want configured source root")
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

	workflow, err := ParseWorkflow([]byte(fmt.Sprintf("---\ntracker:\n  kind: memory\nworkspace:\n  source_root: %s\n---\nPrompt\n", initConfigSourceRepo(t))))
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

func TestParseWorkflowMemoryTrackerIssues(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
  issues:
    - id: issue-1
      identifier: MT-1
      title: Memory adapter
      description: Load issues from config
      priority: 2
      state: Todo
      branch_name: symphony/mt-1
      url: https://example.com/issues/1
      assignee_id: worker-1
      blocked_by:
        - id: issue-0
          identifier: MT-0
          state: Done
      labels:
        - stage:s1
      assigned_to_worker: true
      model_override: gpt-5-codex-high
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}

	got := workflow.Config.Tracker.Issues
	if len(got) != 1 {
		t.Fatalf("Tracker.Issues len = %d, want 1", len(got))
	}
	priority := 2
	want := connector.Issue{
		ID:               "issue-1",
		Identifier:       "MT-1",
		Title:            "Memory adapter",
		Description:      "Load issues from config",
		Priority:         &priority,
		State:            "Todo",
		BranchName:       "symphony/mt-1",
		URL:              "https://example.com/issues/1",
		AssigneeID:       "worker-1",
		BlockedBy:        []connector.BlockedRef{{ID: "issue-0", Identifier: "MT-0", State: "Done"}},
		Labels:           []string{"stage:s1"},
		AssignedToWorker: true,
		ModelOverride:    "gpt-5-codex-high",
	}
	if got[0].ID != want.ID ||
		got[0].Identifier != want.Identifier ||
		got[0].Title != want.Title ||
		got[0].Description != want.Description ||
		got[0].Priority == nil ||
		*got[0].Priority != *want.Priority ||
		got[0].State != want.State ||
		got[0].BranchName != want.BranchName ||
		got[0].URL != want.URL ||
		got[0].AssigneeID != want.AssigneeID ||
		len(got[0].BlockedBy) != 1 ||
		got[0].BlockedBy[0] != want.BlockedBy[0] ||
		len(got[0].Labels) != 1 ||
		got[0].Labels[0] != want.Labels[0] ||
		!got[0].AssignedToWorker ||
		got[0].ModelOverride != want.ModelOverride {
		t.Fatalf("Tracker.Issues[0] = %#v, want %#v", got[0], want)
	}
}

func TestParseWorkflowMemoryTrackerIssueDefaults(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
  issues:
    - id: issue-1
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}

	got := workflow.Config.Tracker.Issues[0]
	if !got.AssignedToWorker {
		t.Fatal("AssignedToWorker = false, want true")
	}
	if len(got.BlockedBy) != 0 {
		t.Fatalf("BlockedBy len = %d, want 0", len(got.BlockedBy))
	}
	if len(got.Labels) != 0 {
		t.Fatalf("Labels len = %d, want 0", len(got.Labels))
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

	workflow, err := ParseWorkflow([]byte(fmt.Sprintf(`---
tracker:
  kind: github
  project_slug: PVT_project
  github_app_id: 12345
  github_app_private_key_path: .symphony/github-app.pem
  github_app_installation_id: 67890
workspace:
  source_root: %s
---
Prompt
`, initConfigSourceRepo(t))))
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
			name: "invalid source root",
			raw: `---
tracker:
  kind: memory
workspace:
  source_root: /path/that/does/not/exist
---
Prompt
`,
			want: []string{"workspace.source_root must be an existing git repo"},
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

func initConfigSourceRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	runConfigCommand(t, dir, "git", "init", "-b", "main")
	return dir
}

func runConfigCommand(t *testing.T, dir string, name string, args ...string) {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s failed: %v\n%s", name, strings.Join(args, " "), err, output)
	}
}
