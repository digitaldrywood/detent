package config

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/selector"
)

func TestParseWorkflowFrontmatter(t *testing.T) {
	t.Parallel()

	raw := []byte(`---
identity:
  name: release-captain
  github_login: detent-bot
  ownership_mode: field
  owner_field: Owner
tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  project_slug: "PVT_project"
  write_probe_issue: " digitaldrywood/detent#1 "
  http_max_idle_conns: 120
  http_max_idle_conns_per_host: 40
  http_idle_conn_timeout_ms: 120000
  github_graphql_warn_remaining: 750
  claims:
    enabled: true
    lease_field: Detent Lease
    ttl_seconds: 300
    heartbeat_seconds: 45
  authorization:
    assignee_in:
      - "@me"
    labels:
      include:
        - release
    fields:
      - name: Track
        value: multi-instance
  active_states:
    - Todo
    - In Progress
  state_map:
    Cancelled: Done
  priority_map:
    Urgent: 1
    No priority: null
  dependency_auto_unblock:
    enabled: true
    source_states:
      - Blocked
      - Waiting
    target_state: Todo
    readiness: terminal_or_merged
polling:
  interval_ms: 60000
workspace:
  root: ~/code/detent-workspaces
  auto_branch: false
  cleanup_idle_ttl_ms: 7200000
  cleanup_sweep_interval_ms: 120000
worker:
  ssh_hosts:
    - worker-1
  max_concurrent_agents_per_host: 2
agent:
  max_concurrent_agents: 5
  shutdown:
    drain_timeout_ms: 300000
  max_concurrent_agents_by_state:
    Merging: 1
  dispatch_priority_by_state:
    - Merging
    - Rework
  dispatch_priority_by_label:
    - Bug
    - regression
    - enhancement
  auto_promote:
    enabled: true
    quiet_seconds: 0
    optout_label: Requires-Human-Review
    allowed_issue_labels:
      - enhancement
  lessons:
    enabled: true
    path: ".detent/lessons.md"
    max_entries: 5
    recall_n: 2
    postmortem_max_tokens: 256
  skills:
    enabled: true
    path: ".detent/skills"
    max_skills_in_prompt: 20
codex:
  command: codex app-server
  shell: bash
  approval_policy: never
  thread_sandbox: danger-full-access
  turn_sandbox_policy:
    type: dangerFullAccess
    networkAccess: true
  turn_timeout_ms: 600000
  read_timeout_ms: 1000
  stall_timeout_ms: 0
gate:
  kind: human_review
  approval_label: Approved-By-Human
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
  shell: bash
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
	if cfg.Identity.Name != "release-captain" {
		t.Fatalf("Identity.Name = %q, want release-captain", cfg.Identity.Name)
	}
	if cfg.Identity.GitHubLogin != "detent-bot" {
		t.Fatalf("Identity.GitHubLogin = %q, want detent-bot", cfg.Identity.GitHubLogin)
	}
	if cfg.Identity.OwnershipMode != IdentityOwnershipField {
		t.Fatalf("Identity.OwnershipMode = %q, want %q", cfg.Identity.OwnershipMode, IdentityOwnershipField)
	}
	if cfg.Identity.OwnerField != "Owner" {
		t.Fatalf("Identity.OwnerField = %q, want Owner", cfg.Identity.OwnerField)
	}
	if cfg.Tracker.Endpoint != "https://api.github.com/graphql" {
		t.Fatalf("Tracker.Endpoint = %q", cfg.Tracker.Endpoint)
	}
	if cfg.Tracker.WriteProbeIssue != "digitaldrywood/detent#1" {
		t.Fatalf("Tracker.WriteProbeIssue = %q, want digitaldrywood/detent#1", cfg.Tracker.WriteProbeIssue)
	}
	if cfg.Tracker.HTTPMaxIdleConns != 120 {
		t.Fatalf("Tracker.HTTPMaxIdleConns = %d, want 120", cfg.Tracker.HTTPMaxIdleConns)
	}
	if cfg.Tracker.HTTPMaxIdleConnsPerHost != 40 {
		t.Fatalf("Tracker.HTTPMaxIdleConnsPerHost = %d, want 40", cfg.Tracker.HTTPMaxIdleConnsPerHost)
	}
	if cfg.Tracker.HTTPIdleConnTimeoutMS != 120000 {
		t.Fatalf("Tracker.HTTPIdleConnTimeoutMS = %d, want 120000", cfg.Tracker.HTTPIdleConnTimeoutMS)
	}
	if cfg.Workspace.CleanupIdleTTLMS != 7200000 {
		t.Fatalf("Workspace.CleanupIdleTTLMS = %d, want 7200000", cfg.Workspace.CleanupIdleTTLMS)
	}
	if cfg.Workspace.CleanupSweepIntervalMS != 120000 {
		t.Fatalf("Workspace.CleanupSweepIntervalMS = %d, want 120000", cfg.Workspace.CleanupSweepIntervalMS)
	}
	if cfg.Tracker.GitHubGraphQLWarnRemaining != 750 {
		t.Fatalf("Tracker.GitHubGraphQLWarnRemaining = %d, want 750", cfg.Tracker.GitHubGraphQLWarnRemaining)
	}
	if !cfg.Tracker.Claims.Enabled {
		t.Fatal("Tracker.Claims.Enabled = false, want true")
	}
	if cfg.Tracker.Claims.LeaseField != "Detent Lease" {
		t.Fatalf("Tracker.Claims.LeaseField = %q, want Detent Lease", cfg.Tracker.Claims.LeaseField)
	}
	if cfg.Tracker.Claims.TTLSeconds != 300 {
		t.Fatalf("Tracker.Claims.TTLSeconds = %d, want 300", cfg.Tracker.Claims.TTLSeconds)
	}
	if cfg.Tracker.Claims.HeartbeatSeconds != 45 {
		t.Fatalf("Tracker.Claims.HeartbeatSeconds = %d, want 45", cfg.Tracker.Claims.HeartbeatSeconds)
	}
	wantAuthorization := selector.Selector{
		AssigneeIn: []string{"@me"},
		Labels:     selector.Labels{Include: []string{"release"}},
		Fields:     []selector.FieldEquals{{Name: "Track", Value: "multi-instance"}},
	}
	if got := cfg.Tracker.Authorization; !reflect.DeepEqual(got, wantAuthorization) {
		t.Fatalf("Tracker.Authorization = %#v, want %#v", got, wantAuthorization)
	}
	if got := cfg.Tracker.StateMap.Map["Cancelled"]; got != "Done" {
		t.Fatalf("Tracker.StateMap[Cancelled] = %v, want Done", got)
	}
	if got := cfg.Tracker.PriorityMap.Map["No priority"]; got != nil {
		t.Fatalf("Tracker.PriorityMap[No priority] = %v, want nil", got)
	}
	if !cfg.Tracker.DependencyAutoUnblock.Enabled {
		t.Fatal("Tracker.DependencyAutoUnblock.Enabled = false, want true")
	}
	if got := cfg.Tracker.DependencyAutoUnblock.SourceStates; !reflect.DeepEqual(got, []string{"blocked", "waiting"}) {
		t.Fatalf("Tracker.DependencyAutoUnblock.SourceStates = %#v, want blocked/waiting", got)
	}
	if cfg.Tracker.DependencyAutoUnblock.TargetState != "Todo" {
		t.Fatalf("Tracker.DependencyAutoUnblock.TargetState = %q, want Todo", cfg.Tracker.DependencyAutoUnblock.TargetState)
	}
	if cfg.Tracker.DependencyAutoUnblock.Readiness != DependencyReadinessTerminalOrMerged {
		t.Fatalf("Tracker.DependencyAutoUnblock.Readiness = %q, want %q", cfg.Tracker.DependencyAutoUnblock.Readiness, DependencyReadinessTerminalOrMerged)
	}
	if got := cfg.Agent.MaxConcurrentAgentsByState["merging"]; got != 1 {
		t.Fatalf("Agent.MaxConcurrentAgentsByState[merging] = %d, want 1", got)
	}
	if cfg.Agent.Shutdown.DrainTimeoutMS != 300000 {
		t.Fatalf("Agent.Shutdown.DrainTimeoutMS = %d, want 300000", cfg.Agent.Shutdown.DrainTimeoutMS)
	}
	if got := cfg.Agent.DispatchPriorityByState; len(got) != 2 || got[0] != "merging" || got[1] != "rework" {
		t.Fatalf("Agent.DispatchPriorityByState = %#v", got)
	}
	if got := cfg.Agent.DispatchPriorityByLabel; !reflect.DeepEqual(got, []string{"bug", "regression", "enhancement"}) {
		t.Fatalf("Agent.DispatchPriorityByLabel = %#v, want bug/regression/enhancement", got)
	}
	if cfg.Agent.AutoPromote.OptoutLabel != "requires-human-review" {
		t.Fatalf("Agent.AutoPromote.OptoutLabel = %q", cfg.Agent.AutoPromote.OptoutLabel)
	}
	if !cfg.Codex.ApprovalPolicy.IsString || cfg.Codex.ApprovalPolicy.String != "never" {
		t.Fatalf("Codex.ApprovalPolicy = %#v, want string never", cfg.Codex.ApprovalPolicy)
	}
	if cfg.Gate.Kind != gate.KindHumanReview || cfg.Gate.ApprovalLabel != "approved-by-human" || cfg.Gate.Run != "" {
		t.Fatalf("Gate = %#v, want human_review with approved-by-human label", cfg.Gate)
	}
	if cfg.Codex.Shell != "bash" {
		t.Fatalf("Codex.Shell = %q, want bash", cfg.Codex.Shell)
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
	if cfg.Hooks.Shell != "bash" {
		t.Fatalf("Hooks.Shell = %q, want bash", cfg.Hooks.Shell)
	}
}

func TestParseWorkflowGitHubIssueFieldTracker(t *testing.T) {
	t.Parallel()

	raw := []byte(`---
tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  github_status_source: issue_field
  repository: digitaldrywood/detent
  active_states:
    - Todo
---
Prompt
`)

	workflow, err := ParseWorkflow(raw)
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	cfg := workflow.Config
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if cfg.Tracker.GitHubStatusSource != GitHubStatusSourceIssueField {
		t.Fatalf("GitHubStatusSource = %q, want %q", cfg.Tracker.GitHubStatusSource, GitHubStatusSourceIssueField)
	}
	if cfg.Tracker.Repository != "digitaldrywood/detent" {
		t.Fatalf("Repository = %q, want digitaldrywood/detent", cfg.Tracker.Repository)
	}
	if cfg.Tracker.StatusField != "Status" {
		t.Fatalf("StatusField = %q, want Status", cfg.Tracker.StatusField)
	}
	if cfg.Tracker.ProjectSlug != "" {
		t.Fatalf("ProjectSlug = %q, want empty for issue_field source", cfg.Tracker.ProjectSlug)
	}
}

func TestValidateGitHubProjectV2StillRequiresProjectSlug(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Tracker.Kind = TrackerGitHub
	cfg.Tracker.APIKey = "token"
	cfg.Tracker.ProjectSlug = ""

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tracker.project_slug") {
		t.Fatalf("Validate() error = %v, want project_slug requirement", err)
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
	if cfg.Identity.Configured() {
		t.Fatalf("Identity = %#v, want omitted default", cfg.Identity)
	}
	if cfg.Tracker.Authorization.Configured() {
		t.Fatalf("Tracker.Authorization = %#v, want authorize all default", cfg.Tracker.Authorization)
	}
	if cfg.Tracker.DependencyAutoUnblock.Enabled {
		t.Fatal("Tracker.DependencyAutoUnblock.Enabled = true, want disabled by default")
	}
	if got := cfg.Tracker.DependencyAutoUnblock.SourceStates; !reflect.DeepEqual(got, []string{"blocked"}) {
		t.Fatalf("Tracker.DependencyAutoUnblock.SourceStates = %#v, want blocked", got)
	}
	if cfg.Tracker.DependencyAutoUnblock.TargetState != "Todo" {
		t.Fatalf("Tracker.DependencyAutoUnblock.TargetState = %q, want Todo", cfg.Tracker.DependencyAutoUnblock.TargetState)
	}
	if cfg.Tracker.DependencyAutoUnblock.Readiness != DependencyReadinessTerminalOrMerged {
		t.Fatalf("Tracker.DependencyAutoUnblock.Readiness = %q, want %q", cfg.Tracker.DependencyAutoUnblock.Readiness, DependencyReadinessTerminalOrMerged)
	}
	if cfg.Polling.IntervalMS != 120000 {
		t.Fatalf("Polling.IntervalMS = %d", cfg.Polling.IntervalMS)
	}
	if cfg.Tracker.HTTPMaxIdleConns != 100 {
		t.Fatalf("Tracker.HTTPMaxIdleConns = %d, want 100", cfg.Tracker.HTTPMaxIdleConns)
	}
	if cfg.Tracker.HTTPMaxIdleConnsPerHost != 32 {
		t.Fatalf("Tracker.HTTPMaxIdleConnsPerHost = %d, want 32", cfg.Tracker.HTTPMaxIdleConnsPerHost)
	}
	if cfg.Tracker.HTTPIdleConnTimeoutMS != 90000 {
		t.Fatalf("Tracker.HTTPIdleConnTimeoutMS = %d, want 90000", cfg.Tracker.HTTPIdleConnTimeoutMS)
	}
	if cfg.Workspace.AutoBranch != true {
		t.Fatal("Workspace.AutoBranch = false, want true")
	}
	if cfg.Workspace.CleanupIdleTTLMS != 86400000 {
		t.Fatalf("Workspace.CleanupIdleTTLMS = %d, want 86400000", cfg.Workspace.CleanupIdleTTLMS)
	}
	if cfg.Workspace.CleanupSweepIntervalMS != 600000 {
		t.Fatalf("Workspace.CleanupSweepIntervalMS = %d, want 600000", cfg.Workspace.CleanupSweepIntervalMS)
	}
	if cfg.Agent.MaxConcurrentAgents != 10 {
		t.Fatalf("Agent.MaxConcurrentAgents = %d, want 10", cfg.Agent.MaxConcurrentAgents)
	}
	if cfg.Agent.Shutdown.DrainTimeoutMS != DefaultShutdownDrainTimeoutMS {
		t.Fatalf("Agent.Shutdown.DrainTimeoutMS = %d, want %d", cfg.Agent.Shutdown.DrainTimeoutMS, DefaultShutdownDrainTimeoutMS)
	}
	if cfg.Agent.Lessons.Path != ".detent/lessons.md" {
		t.Fatalf("Agent.Lessons.Path = %q", cfg.Agent.Lessons.Path)
	}
	if cfg.Agent.Skills.Path != ".detent/skills" {
		t.Fatalf("Agent.Skills.Path = %q", cfg.Agent.Skills.Path)
	}
	if !cfg.Codex.ApprovalPolicy.IsMap {
		t.Fatalf("Codex.ApprovalPolicy = %#v, want map default", cfg.Codex.ApprovalPolicy)
	}
	if strings.TrimSpace(cfg.Codex.Shell) == "" {
		t.Fatal("Codex.Shell is blank, want per-OS default")
	}
	if strings.TrimSpace(cfg.Hooks.Shell) == "" {
		t.Fatal("Hooks.Shell is blank, want per-OS default")
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
	if len(cfg.Agents.Backends) != 0 {
		t.Fatalf("Agents.Backends len = %d, want legacy empty config", len(cfg.Agents.Backends))
	}
	if len(cfg.Agents.Routes) != 0 {
		t.Fatalf("Agents.Routes len = %d, want legacy empty config", len(cfg.Agents.Routes))
	}
	if len(cfg.Agent.DispatchPriorityByLabel) != 0 {
		t.Fatalf("Agent.DispatchPriorityByLabel = %#v, want empty default", cfg.Agent.DispatchPriorityByLabel)
	}
	if cfg.Gate.Kind != gate.KindCommand || cfg.Gate.Run != gate.DefaultCommand {
		t.Fatalf("Gate = %#v, want default command gate", cfg.Gate)
	}
}

func TestAgentDispatchPriorityByLabelYAMLRoundTrip(t *testing.T) {
	t.Parallel()

	want := []string{"bug", "regression", "enhancement"}
	raw, err := yaml.Marshal(Agent{DispatchPriorityByLabel: want})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got Agent
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if !reflect.DeepEqual(got.DispatchPriorityByLabel, want) {
		t.Fatalf("DispatchPriorityByLabel = %#v, want %#v", got.DispatchPriorityByLabel, want)
	}
}

func TestParseWorkflowAgentsConfig(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
agents:
  backends:
    - id: codex-high
      kind: codex
      protocol: app-server
      command: codex app-server --profile high
      options:
        shell: bash
        approval_policy: never
        thread_sandbox: danger-full-access
        turn_sandbox_policy:
          type: dangerFullAccess
        turn_timeout_ms: 600000
        read_timeout_ms: 1000
        stall_timeout_ms: 0
  routes:
    - name: high-label
      backend: codex-high
      model: gpt-5-codex-high
      selector:
        labels:
          include:
            - tier:high
    - name: project-model
      backend: codex-high
      model_field: Model
    - name: urgent
      backend: codex-high
      model: gpt-5-codex
      selector:
        priority_in:
          - 1
    - name: default
      backend: codex-high
      default: true
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	agents := workflow.Config.Agents
	if len(agents.Backends) != 1 {
		t.Fatalf("Agents.Backends len = %d, want 1", len(agents.Backends))
	}
	backend := agents.Backends[0]
	if backend.ID != "codex-high" || backend.Kind != "codex" || backend.Protocol != "app-server" {
		t.Fatalf("backend identity = %#v, want codex-high codex app-server", backend)
	}
	if backend.Command != "codex app-server --profile high" {
		t.Fatalf("backend Command = %q, want configured command", backend.Command)
	}
	if backend.Options.Shell != "bash" {
		t.Fatalf("backend shell = %q, want bash", backend.Options.Shell)
	}
	if !backend.Options.ApprovalPolicy.IsString || backend.Options.ApprovalPolicy.String != "never" {
		t.Fatalf("backend approval policy = %#v, want never", backend.Options.ApprovalPolicy)
	}
	if backend.Options.TurnSandboxPolicy["type"] != "dangerFullAccess" {
		t.Fatalf("backend turn sandbox policy = %#v, want dangerFullAccess", backend.Options.TurnSandboxPolicy)
	}
	if len(agents.Routes) != 4 {
		t.Fatalf("Agents.Routes len = %d, want 4", len(agents.Routes))
	}
	if got := agents.Routes[0].Selector.Labels.Include; len(got) != 1 || got[0] != "tier:high" {
		t.Fatalf("route label selector = %#v, want tier:high", got)
	}
	if agents.Routes[1].ModelField != "Model" {
		t.Fatalf("route ModelField = %q, want Model", agents.Routes[1].ModelField)
	}
	if got := agents.Routes[2].Selector.PriorityIn; len(got) != 1 || got[0] != 1 {
		t.Fatalf("route priority selector = %#v, want priority 1", got)
	}
	if !agents.Routes[3].Default {
		t.Fatal("default route Default = false, want true")
	}
}

func TestParseWorkflowCommandGateDisablesAutomatedReviewRequirement(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
gate:
  kind: command
  run: make check
  require_automated_review: false
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	cfg := workflow.Config.Gate
	if cfg.Kind != gate.KindCommand || cfg.Run != gate.DefaultCommand {
		t.Fatalf("Gate = %#v, want command make check", cfg)
	}
	if cfg.RequireAutomatedReview == nil {
		t.Fatal("Gate.RequireAutomatedReview = nil, want false")
	}
	if *cfg.RequireAutomatedReview {
		t.Fatal("Gate.RequireAutomatedReview = true, want false")
	}
}

func TestParseWorkflowAgentRoutesCanUseLegacyCodexBackend(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
codex:
  command: codex app-server
agents:
  routes:
    - name: project-model
      backend: codex
      model_field: Model
    - name: default
      backend: codex
      default: true
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
      branch_name: detent/mt-1
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
		BranchName:       "detent/mt-1",
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

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: github
  project_slug: PVT_project
  github_app_id: 12345
  github_app_private_key_path: .detent/github-app.pem
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
  http_max_idle_conns: 0
  http_max_idle_conns_per_host: 0
  http_idle_conn_timeout_ms: 0
  active_states: ["Todo", ""]
polling:
  interval_ms: 0
worker:
  max_concurrent_agents_per_host: 0
workspace:
  cleanup_idle_ttl_ms: 0
  cleanup_sweep_interval_ms: 0
agent:
  max_concurrent_agents: 0
  max_concurrent_agents_by_state:
    Todo: 0
  dispatch_priority_by_state: ["Todo", "Todo"]
  dispatch_priority_by_label: [""]
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
				"tracker.http_max_idle_conns must be greater than 0",
				"tracker.http_max_idle_conns_per_host must be greater than 0",
				"tracker.http_idle_conn_timeout_ms must be greater than 0",
				"polling.interval_ms must be greater than 0",
				"worker.max_concurrent_agents_per_host must be greater than 0",
				"workspace.cleanup_idle_ttl_ms must be greater than 0",
				"workspace.cleanup_sweep_interval_ms must be greater than 0",
				"agent.max_concurrent_agents must be greater than 0",
				"agent.max_concurrent_agents_by_state limits must be positive integers",
				"agent.dispatch_priority_by_state state names must be unique",
				"agent.dispatch_priority_by_label labels must not be blank",
				"codex.turn_timeout_ms must be greater than 0",
				"hooks.timeout_ms must be greater than 0",
				"observability.refresh_ms must be greater than 0",
				"budget.per_day_max_usd must be greater than 0",
				"server.port must be greater than or equal to 0",
			},
		},
		{
			name: "polling interval floor",
			raw: `---
tracker:
  kind: memory
polling:
  interval_ms: 59999
---
Prompt
`,
			want: []string{"polling.interval_ms must be at least 60000"},
		},
		{
			name: "invalid dependency auto unblock config",
			raw: `---
tracker:
  kind: memory
  dependency_auto_unblock:
    enabled: true
    source_states: [""]
    target_state: ""
    readiness: sometimes
---
Prompt
`,
			want: []string{
				"tracker.dependency_auto_unblock.source_states state names must not be blank",
				"tracker.dependency_auto_unblock.target_state is required when tracker.dependency_auto_unblock.enabled is true",
				"tracker.dependency_auto_unblock.readiness must be one of terminal, terminal_or_merged",
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
		{
			name: "invalid agents config",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: codex
      kind: claude
      protocol: stream
      command: ""
  routes:
    - backend: missing
      default: true
    - backend: codex
      default: true
      selector:
        priority_in: [0]
---
Prompt
`,
			want: []string{
				"agents.backends.kind must be codex",
				"agents.backends.command is required",
				"agents.routes.backend must reference a configured backend",
				"agents.routes.selector.priority_in values must be integers 1 through 4",
				"agents.routes must not define multiple default routes",
			},
		},
		{
			name: "invalid identity and authorization",
			raw: `---
identity:
  github_login: detent-bot
  ownership_mode: field
tracker:
  kind: memory
  authorization:
    priority_in: [0]
    fields:
      - value: multi-instance
---
Prompt
`,
			want: []string{
				"identity.name must not be blank",
				"identity.owner_field is required when identity.ownership_mode is field",
				"tracker.authorization.priority_in values must be integers 1 through 4",
				"tracker.authorization.fields[0].name must not be blank",
			},
		},
		{
			name: "invalid claim lease config",
			raw: `---
identity:
  name: release-captain
  github_login: detent-bot
tracker:
  kind: memory
  claims:
    enabled: true
    lease_field: ""
    ttl_seconds: 30
    heartbeat_seconds: 60
---
Prompt
`,
			want: []string{
				"tracker.claims.lease_field must not be blank when tracker.claims.enabled is true",
				"tracker.claims.heartbeat_seconds must be less than or equal to tracker.claims.ttl_seconds",
			},
		},
		{
			name: "invalid gate config",
			raw: `---
tracker:
  kind: memory
gate:
  kind: checklist
---
Prompt
`,
			want: []string{
				"gate.kind must be one of command, human_review",
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
