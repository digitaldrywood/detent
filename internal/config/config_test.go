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
  github_graphql_min_remaining_reserve: 1750
  github_rest_min_remaining_reserve: 1500
  github_rest_fanout_max_requests: 42
  github_rest_debug_logging: true
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
    - Rework
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
  blocker_auto_promote:
    enabled: true
    source_states:
      - Blocked
      - Rework
    blocker_states:
      - Backlog
      - Human Review
    target_state: Todo
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
  max_session_tokens: 10000000
  max_session_context_multiplier: 3.5
  max_session_token_override_label: Allow-Large-Session
  max_session_token_override_field: Token Override
  experimental_thread_resume: true
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
    gate_wait_state: review
    gate_wait_timeout_seconds: 900
    rework_limit: 3
    allowed_issue_labels:
      - enhancement
  lessons:
    enabled: true
    path: ".detent/lessons.md"
    max_entries: 5
    recall_n: 2
    postmortem_max_tokens: 256
  knowledge:
    enabled: true
    max_bytes: 2048
    sources:
      - name: Team standards
        path: ../knowledge/team.md
  skills:
    enabled: true
    path: ".detent/skills"
    max_skills_in_prompt: 20
    creation:
      enabled: true
      max_drafts_per_run: 3
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
plan:
  enabled: true
  review: both
  approval_label: Plan-Approved
  stop: " Plan Review "
server:
  host: 0.0.0.0
  port: 4001
  kanban:
    mode: integration
    show_blocked_alerts: true
    allowed_transitions:
      In Progress:
        - Blocked
        - Cancelled
      QA:
        - Done
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
	if cfg.Tracker.GitHubGraphQLMinReserve != 1750 {
		t.Fatalf("Tracker.GitHubGraphQLMinReserve = %d, want 1750", cfg.Tracker.GitHubGraphQLMinReserve)
	}
	if cfg.Tracker.GitHubRESTMinReserve != 1500 {
		t.Fatalf("Tracker.GitHubRESTMinReserve = %d, want 1500", cfg.Tracker.GitHubRESTMinReserve)
	}
	if cfg.Tracker.GitHubRESTFanoutMaxRequests != 42 {
		t.Fatalf("Tracker.GitHubRESTFanoutMaxRequests = %d, want 42", cfg.Tracker.GitHubRESTFanoutMaxRequests)
	}
	if !cfg.Tracker.GitHubRESTDebugLogging {
		t.Fatal("Tracker.GitHubRESTDebugLogging = false, want true")
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
	if !cfg.Tracker.BlockerAutoPromote.Enabled {
		t.Fatal("Tracker.BlockerAutoPromote.Enabled = false, want true")
	}
	if got := cfg.Tracker.BlockerAutoPromote.SourceStates; !reflect.DeepEqual(got, []string{"blocked", "rework"}) {
		t.Fatalf("Tracker.BlockerAutoPromote.SourceStates = %#v, want blocked/rework", got)
	}
	if got := cfg.Tracker.BlockerAutoPromote.BlockerStates; !reflect.DeepEqual(got, []string{"backlog", "human review"}) {
		t.Fatalf("Tracker.BlockerAutoPromote.BlockerStates = %#v, want backlog/human review", got)
	}
	if cfg.Tracker.BlockerAutoPromote.TargetState != "Todo" {
		t.Fatalf("Tracker.BlockerAutoPromote.TargetState = %q, want Todo", cfg.Tracker.BlockerAutoPromote.TargetState)
	}
	if got := cfg.Agent.MaxConcurrentAgentsByState["merging"]; got != 1 {
		t.Fatalf("Agent.MaxConcurrentAgentsByState[merging] = %d, want 1", got)
	}
	if cfg.Agent.MaxSessionTokens != 10000000 {
		t.Fatalf("Agent.MaxSessionTokens = %d, want 10000000", cfg.Agent.MaxSessionTokens)
	}
	if cfg.Agent.MaxSessionContextMultiplier != 3.5 {
		t.Fatalf("Agent.MaxSessionContextMultiplier = %v, want 3.5", cfg.Agent.MaxSessionContextMultiplier)
	}
	if cfg.Agent.MaxSessionTokenOverrideLabel != "allow-large-session" {
		t.Fatalf("Agent.MaxSessionTokenOverrideLabel = %q, want allow-large-session", cfg.Agent.MaxSessionTokenOverrideLabel)
	}
	if cfg.Agent.MaxSessionTokenOverrideField != "Token Override" {
		t.Fatalf("Agent.MaxSessionTokenOverrideField = %q, want Token Override", cfg.Agent.MaxSessionTokenOverrideField)
	}
	if !cfg.Agent.ExperimentalThreadResume {
		t.Fatal("Agent.ExperimentalThreadResume = false, want true")
	}
	if !cfg.Agent.Knowledge.Enabled {
		t.Fatal("Agent.Knowledge.Enabled = false, want true")
	}
	if cfg.Agent.Knowledge.MaxBytes != 2048 {
		t.Fatalf("Agent.Knowledge.MaxBytes = %d, want 2048", cfg.Agent.Knowledge.MaxBytes)
	}
	if len(cfg.Agent.Knowledge.Sources) != 1 {
		t.Fatalf("Agent.Knowledge.Sources len = %d, want 1", len(cfg.Agent.Knowledge.Sources))
	}
	if source := cfg.Agent.Knowledge.Sources[0]; source.Name != "Team standards" || source.Path != "../knowledge/team.md" {
		t.Fatalf("Agent.Knowledge.Sources[0] = %#v, want team standards source", source)
	}
	if !cfg.Agent.Skills.Creation.Enabled {
		t.Fatal("Agent.Skills.Creation.Enabled = false, want true")
	}
	if cfg.Agent.Skills.Creation.MaxDraftsPerRun != 3 {
		t.Fatalf("Agent.Skills.Creation.MaxDraftsPerRun = %d, want 3", cfg.Agent.Skills.Creation.MaxDraftsPerRun)
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
	if cfg.Agent.AutoPromote.ReworkLimit != 3 {
		t.Fatalf("Agent.AutoPromote.ReworkLimit = %d, want 3", cfg.Agent.AutoPromote.ReworkLimit)
	}
	if cfg.Agent.AutoPromote.GateWaitState != AutoPromoteGateWaitStateReview {
		t.Fatalf("Agent.AutoPromote.GateWaitState = %q, want review", cfg.Agent.AutoPromote.GateWaitState)
	}
	if cfg.Agent.AutoPromote.GateWaitTimeoutSeconds != 900 {
		t.Fatalf("Agent.AutoPromote.GateWaitTimeoutSeconds = %d, want 900", cfg.Agent.AutoPromote.GateWaitTimeoutSeconds)
	}
	if !cfg.Codex.ApprovalPolicy.IsString || cfg.Codex.ApprovalPolicy.String != "never" {
		t.Fatalf("Codex.ApprovalPolicy = %#v, want string never", cfg.Codex.ApprovalPolicy)
	}
	if cfg.Gate.Kind != gate.KindHumanReview || cfg.Gate.ApprovalLabel != "approved-by-human" || cfg.Gate.Run != "" {
		t.Fatalf("Gate = %#v, want human_review with approved-by-human label", cfg.Gate)
	}
	if !cfg.Plan.Enabled || cfg.Plan.Review != gate.PlanReviewBoth || cfg.Plan.ApprovalLabel != gate.DefaultPlanApprovalLabel || cfg.Plan.Stop != gate.DefaultPlanStop {
		t.Fatalf("Plan = %#v, want enabled both review at Plan Review with plan-approved label", cfg.Plan)
	}
	if !stateListContains(cfg.Tracker.ObservedStates, gate.DefaultPlanStop) {
		t.Fatalf("Tracker.ObservedStates = %#v, want plan stop", cfg.Tracker.ObservedStates)
	}
	if cfg.Server.Kanban.Mode != KanbanModeIntegration {
		t.Fatalf("Server.Kanban.Mode = %q, want %q", cfg.Server.Kanban.Mode, KanbanModeIntegration)
	}
	if !cfg.Server.Kanban.ShowBlockedAlerts {
		t.Fatal("Server.Kanban.ShowBlockedAlerts = false, want true")
	}
	wantTransitions := map[string][]string{
		"In Progress": {"Blocked", "Cancelled"},
		"QA":          {"Done"},
	}
	if !reflect.DeepEqual(cfg.Server.Kanban.AllowedTransitions, wantTransitions) {
		t.Fatalf("Server.Kanban.AllowedTransitions = %#v, want %#v", cfg.Server.Kanban.AllowedTransitions, wantTransitions)
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

func TestParseWorkflowGitHubLabelTracker(t *testing.T) {
	t.Parallel()

	raw := []byte(`---
tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  github_status_source: label
  repository: digitaldrywood/detent
  status_label_prefix: "detent:"
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
	if cfg.Tracker.GitHubStatusSource != GitHubStatusSourceLabel {
		t.Fatalf("GitHubStatusSource = %q, want %q", cfg.Tracker.GitHubStatusSource, GitHubStatusSourceLabel)
	}
	if cfg.Tracker.Repository != "digitaldrywood/detent" {
		t.Fatalf("Repository = %q, want digitaldrywood/detent", cfg.Tracker.Repository)
	}
	if cfg.Tracker.StatusLabelPrefix != "detent:" {
		t.Fatalf("StatusLabelPrefix = %q, want detent:", cfg.Tracker.StatusLabelPrefix)
	}
	if cfg.Tracker.ProjectSlug != "" {
		t.Fatalf("ProjectSlug = %q, want empty for label source", cfg.Tracker.ProjectSlug)
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

func TestValidateGitHubIssueFieldRequiresRepository(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Tracker.Kind = TrackerGitHub
	cfg.Tracker.APIKey = "token"
	cfg.Tracker.GitHubStatusSource = GitHubStatusSourceIssueField
	cfg.Tracker.ProjectSlug = ""
	cfg.Tracker.Repository = ""

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tracker.repository") {
		t.Fatalf("Validate() error = %v, want repository requirement", err)
	}
	if err != nil && strings.Contains(err.Error(), "tracker.project_slug") {
		t.Fatalf("Validate() error = %v, want no project_slug requirement in issue_field mode", err)
	}
}

func TestValidateGitHubLabelRequiresRepository(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Tracker.Kind = TrackerGitHub
	cfg.Tracker.APIKey = "token"
	cfg.Tracker.GitHubStatusSource = GitHubStatusSourceLabel
	cfg.Tracker.ProjectSlug = ""
	cfg.Tracker.Repository = ""

	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "tracker.repository") {
		t.Fatalf("Validate() error = %v, want repository requirement", err)
	}
	if err != nil && strings.Contains(err.Error(), "tracker.project_slug") {
		t.Fatalf("Validate() error = %v, want no project_slug requirement in label mode", err)
	}
}

func TestNormalizeGitHubLabelStatusSourceAliases(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"label", "labels", "issue_label", "issue_labels"} {
		if got := normalizeGitHubStatusSource(value); got != GitHubStatusSourceLabel {
			t.Fatalf("normalizeGitHubStatusSource(%q) = %q, want %q", value, got, GitHubStatusSourceLabel)
		}
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
	if cfg.Tracker.BlockerAutoPromote.Enabled {
		t.Fatal("Tracker.BlockerAutoPromote.Enabled = true, want disabled by default")
	}
	if got := cfg.Tracker.BlockerAutoPromote.BlockerStates; !reflect.DeepEqual(got, []string{"backlog", "blocked", "human review"}) {
		t.Fatalf("Tracker.BlockerAutoPromote.BlockerStates = %#v, want backlog/blocked/human review", got)
	}
	if cfg.Tracker.BlockerAutoPromote.TargetState != "Todo" {
		t.Fatalf("Tracker.BlockerAutoPromote.TargetState = %q, want Todo", cfg.Tracker.BlockerAutoPromote.TargetState)
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
	if cfg.Tracker.GitHubGraphQLMinReserve != 1000 {
		t.Fatalf("Tracker.GitHubGraphQLMinReserve = %d, want 1000", cfg.Tracker.GitHubGraphQLMinReserve)
	}
	if cfg.Tracker.GitHubRESTMinReserve != 1000 {
		t.Fatalf("Tracker.GitHubRESTMinReserve = %d, want 1000", cfg.Tracker.GitHubRESTMinReserve)
	}
	if cfg.Tracker.GitHubRESTFanoutMaxRequests != 80 {
		t.Fatalf("Tracker.GitHubRESTFanoutMaxRequests = %d, want 80", cfg.Tracker.GitHubRESTFanoutMaxRequests)
	}
	if cfg.Tracker.GitHubRESTDebugLogging {
		t.Fatal("Tracker.GitHubRESTDebugLogging = true, want false by default")
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
	if cfg.Agent.MaxSessionTokens != 0 {
		t.Fatalf("Agent.MaxSessionTokens = %d, want disabled default", cfg.Agent.MaxSessionTokens)
	}
	if cfg.Agent.MaxSessionContextMultiplier != 0 {
		t.Fatalf("Agent.MaxSessionContextMultiplier = %v, want disabled default", cfg.Agent.MaxSessionContextMultiplier)
	}
	if cfg.Agent.MergeFastPath.Enabled {
		t.Fatal("Agent.MergeFastPath.Enabled = true, want false default")
	}
	if cfg.Agent.ExperimentalThreadResume {
		t.Fatal("Agent.ExperimentalThreadResume = true, want disabled default")
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
	if !cfg.Agent.Knowledge.Enabled {
		t.Fatal("Agent.Knowledge.Enabled = false, want true default")
	}
	if cfg.Agent.Knowledge.MaxBytes != DefaultKnowledgeMaxBytes {
		t.Fatalf("Agent.Knowledge.MaxBytes = %d, want %d", cfg.Agent.Knowledge.MaxBytes, DefaultKnowledgeMaxBytes)
	}
	if len(cfg.Agent.Knowledge.Sources) != 0 {
		t.Fatalf("Agent.Knowledge.Sources = %#v, want empty default", cfg.Agent.Knowledge.Sources)
	}
	if !cfg.Agent.Skills.Creation.Enabled {
		t.Fatal("Agent.Skills.Creation.Enabled = false, want true default")
	}
	if cfg.Agent.Skills.Creation.MaxDraftsPerRun != 1 {
		t.Fatalf("Agent.Skills.Creation.MaxDraftsPerRun = %d, want 1", cfg.Agent.Skills.Creation.MaxDraftsPerRun)
	}
	if cfg.Agent.AutoPromote.ReworkLimit != DefaultReworkLimit {
		t.Fatalf("Agent.AutoPromote.ReworkLimit = %d, want %d", cfg.Agent.AutoPromote.ReworkLimit, DefaultReworkLimit)
	}
	if cfg.Agent.AutoPromote.GateWaitState != AutoPromoteGateWaitStateSource {
		t.Fatalf("Agent.AutoPromote.GateWaitState = %q, want source", cfg.Agent.AutoPromote.GateWaitState)
	}
	if cfg.Agent.AutoPromote.GateWaitTimeoutSeconds != DefaultAutoPromoteGateWaitTimeoutSeconds {
		t.Fatalf("Agent.AutoPromote.GateWaitTimeoutSeconds = %d, want %d", cfg.Agent.AutoPromote.GateWaitTimeoutSeconds, DefaultAutoPromoteGateWaitTimeoutSeconds)
	}
	if cfg.Agent.OutputTruncation.MaxBytes != 0 {
		t.Fatalf("Agent.OutputTruncation.MaxBytes = %d, want disabled default", cfg.Agent.OutputTruncation.MaxBytes)
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
	if cfg.Plan.Enabled || cfg.Plan.Review != gate.PlanReviewHuman || cfg.Plan.ApprovalLabel != gate.DefaultPlanApprovalLabel || cfg.Plan.Stop != gate.DefaultPlanStop {
		t.Fatalf("Plan = %#v, want disabled human review plan default", cfg.Plan)
	}
}

func TestParseWorkflowMarksAgentKnowledgeConfigured(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
agent:
  knowledge:
    max_bytes: 2048
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}

	if !workflow.Config.Agent.Knowledge.Configured {
		t.Fatal("Agent.Knowledge.Configured = false, want true")
	}
	if workflow.Config.Agent.Knowledge.MaxBytes != 2048 {
		t.Fatalf("Agent.Knowledge.MaxBytes = %d, want 2048", workflow.Config.Agent.Knowledge.MaxBytes)
	}
}

func TestKnowledgeWithSourcesAllowsSourceLessMaxBytesOverride(t *testing.T) {
	t.Parallel()

	workflowDefault := defaultKnowledge()
	got := KnowledgeWithSources(
		Knowledge{
			Enabled:  true,
			MaxBytes: 4096,
			Sources: []KnowledgeSource{{
				Name: "Global",
				Path: "global.md",
			}},
		},
		Knowledge{
			Enabled:    true,
			MaxBytes:   1024,
			Configured: true,
		},
		workflowDefault,
	)

	if got.MaxBytes != 1024 {
		t.Fatalf("MaxBytes = %d, want 1024", got.MaxBytes)
	}
	if len(got.Sources) != 1 || got.Sources[0].Path != "global.md" {
		t.Fatalf("Sources = %#v, want inherited global source", got.Sources)
	}
}

func TestValidatePlanRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Plan.Enabled = true
	cfg.Plan.Review = "committee"
	cfg.Plan.Stop = " "

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want plan validation errors")
	}
	for _, want := range []string{
		"plan.review must be one of human, automated, both",
		"plan.stop must not be blank when plan.enabled is true",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Validate() error = %v, want %q", err, want)
		}
	}
}

func TestParseWorkflowAgentMergeFastPath(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
agent:
  merge_fast_path:
    enabled: true
---
Body
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if !workflow.Config.Agent.MergeFastPath.Enabled {
		t.Fatal("Agent.MergeFastPath.Enabled = false, want true")
	}
}

func TestParseWorkflowAgentOutputTruncation(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
agent:
  output_truncation:
    max_bytes: 4096
---
Body
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if got := workflow.Config.Agent.OutputTruncation.MaxBytes; got != 4096 {
		t.Fatalf("Agent.OutputTruncation.MaxBytes = %d, want 4096", got)
	}
}

func TestKanbanTransitionPolicyRestrictsActiveExecutionDefaults(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress", "Rework", "Merging"}
	cfg.Tracker.ObservedStates = []string{"Backlog", "Blocked", "Human Review"}
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled"}

	tests := []struct {
		name   string
		source string
		target string
		want   bool
	}{
		{
			name:   "todo can move into execution",
			source: "Todo",
			target: "In Progress",
			want:   true,
		},
		{
			name:   "in progress can move to blocked",
			source: "In Progress",
			target: "Blocked",
			want:   true,
		},
		{
			name:   "in progress cannot bypass review",
			source: "In Progress",
			target: "Human Review",
			want:   false,
		},
		{
			name:   "rework cannot move straight to done",
			source: "Rework",
			target: "Done",
			want:   false,
		},
		{
			name:   "merging can move to cancelled",
			source: "Merging",
			target: "Cancelled",
			want:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := cfg.KanbanTransitionAllowed(tt.source, tt.target); got != tt.want {
				t.Fatalf("KanbanTransitionAllowed(%q, %q) = %v, want %v", tt.source, tt.target, got, tt.want)
			}
		})
	}
}

func TestKanbanTransitionPolicyAllowsConfiguredOverrides(t *testing.T) {
	t.Parallel()

	cfg := Default()
	if cfg.Server.Kanban.ShowBlockedAlerts {
		t.Fatal("Default Server.Kanban.ShowBlockedAlerts = true, want false")
	}
	cfg.Tracker.ActiveStates = []string{"Todo", "In Progress", "Rework", "Merging"}
	cfg.Tracker.ObservedStates = []string{"Backlog", "Blocked", "Human Review"}
	cfg.Tracker.TerminalStates = []string{"Done", "Cancelled"}
	cfg.Server.Kanban.AllowedTransitions = map[string][]string{
		"In Progress": {"Blocked", "Human Review"},
	}
	cfg.Server.Kanban.Normalize()

	if !cfg.KanbanTransitionAllowed("In Progress", "Human Review") {
		t.Fatal("KanbanTransitionAllowed(In Progress, Human Review) = false, want configured override")
	}
	if cfg.KanbanTransitionAllowed("In Progress", "Done") {
		t.Fatal("KanbanTransitionAllowed(In Progress, Done) = true, want configured source allowlist")
	}
}

func TestKanbanTransitionPolicyAllowsConfiguredOverridesWithoutStateLists(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Tracker.ActiveStates = nil
	cfg.Tracker.ObservedStates = nil
	cfg.Tracker.TerminalStates = nil
	cfg.Server.Kanban.AllowedTransitions = map[string][]string{
		"In Progress": {"Blocked"},
	}
	cfg.Server.Kanban.Normalize()

	if got, want := cfg.KanbanAllowedTransitionTargets("In Progress"), []string{"Blocked"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("KanbanAllowedTransitionTargets(In Progress) = %#v, want %#v", got, want)
	}
	if !cfg.KanbanTransitionAllowed("In Progress", "Blocked") {
		t.Fatal("KanbanTransitionAllowed(In Progress, Blocked) = false, want explicit override")
	}
	if cfg.KanbanTransitionAllowed("In Progress", "Human Review") {
		t.Fatal("KanbanTransitionAllowed(In Progress, Human Review) = true, want configured source allowlist")
	}
}

func TestValidateKanbanTransitionsRejectsBlankNames(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Server.Kanban.AllowedTransitions = map[string][]string{
		"":            {"Blocked"},
		"In Progress": {""},
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want transition validation errors")
	}
	for _, want := range []string{
		"server.kanban.allowed_transitions source states must not be blank",
		"server.kanban.allowed_transitions target states must not be blank",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Validate() error = %v, want %q", err, want)
		}
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
	options := backend.CodexOptions()
	if options.Shell != "bash" {
		t.Fatalf("backend shell = %q, want bash", options.Shell)
	}
	if !options.ApprovalPolicy.IsString || options.ApprovalPolicy.String != "never" {
		t.Fatalf("backend approval policy = %#v, want never", options.ApprovalPolicy)
	}
	if options.TurnSandboxPolicy["type"] != "dangerFullAccess" {
		t.Fatalf("backend turn sandbox policy = %#v, want dangerFullAccess", options.TurnSandboxPolicy)
	}
	var decoded CodexOptions
	if err := backend.Options.Decode(&decoded); err != nil {
		t.Fatalf("backend options Decode() error = %v", err)
	}
	if decoded.ReadTimeoutMS != 1000 {
		t.Fatalf("decoded read timeout = %d, want 1000", decoded.ReadTimeoutMS)
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

func TestParseWorkflowClaudeCodeAgentBackendConfig(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
agents:
  backends:
    - id: claude-worker
      kind: claude_code
      options:
        allowed_tools:
          - Bash
          - Edit
        disallowed_tools:
          - WebFetch
        include_partial_messages: true
        turn_timeout_ms: 600000
        stall_timeout_ms: 0
        shell: bash
        extra_args:
          - --model
          - claude-sonnet-4
  routes:
    - backend: claude-worker
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

	backends := workflow.Config.AgentBackendConfigs()
	if len(backends) != 1 {
		t.Fatalf("AgentBackendConfigs() len = %d, want 1", len(backends))
	}
	backend := backends[0]
	if backend.ID != "claude-worker" || backend.Kind != AgentBackendClaudeCode || backend.Protocol != "headless" {
		t.Fatalf("backend identity = %#v, want claude-worker claude_code headless", backend)
	}
	if backend.Command != "claude" {
		t.Fatalf("backend Command = %q, want default claude", backend.Command)
	}

	options := backend.ClaudeCodeOptions()
	if options.PermissionMode != "bypassPermissions" {
		t.Fatalf("permission mode = %q, want bypassPermissions", options.PermissionMode)
	}
	if !reflect.DeepEqual(options.AllowedTools, []string{"Bash", "Edit"}) {
		t.Fatalf("allowed tools = %#v, want Bash/Edit", options.AllowedTools)
	}
	if !reflect.DeepEqual(options.DisallowedTools, []string{"WebFetch"}) {
		t.Fatalf("disallowed tools = %#v, want WebFetch", options.DisallowedTools)
	}
	if !options.IncludePartialMessages {
		t.Fatal("include partial messages = false, want true")
	}
	if options.TurnTimeoutMS != 600000 {
		t.Fatalf("turn timeout = %d, want 600000", options.TurnTimeoutMS)
	}
	if options.StallTimeoutMS != 0 {
		t.Fatalf("stall timeout = %d, want 0", options.StallTimeoutMS)
	}
	if options.Shell != "bash" {
		t.Fatalf("shell = %q, want bash", options.Shell)
	}
	if !reflect.DeepEqual(options.ExtraArgs, []string{"--model", "claude-sonnet-4"}) {
		t.Fatalf("extra args = %#v, want model args", options.ExtraArgs)
	}
}

func TestAgentBackendConfigsMergesLegacyCodexDefaults(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
codex:
  command: codex app-server
  shell: bash
  approval_policy:
    reject:
      sandbox_approval: true
  thread_sandbox: workspace-write
  turn_sandbox_policy:
    type: workspaceWrite
  turn_timeout_ms: 700000
  read_timeout_ms: 7000
  stall_timeout_ms: 70000
agents:
  backends:
    - id: codex-custom
      kind: codex
      protocol: app-server
      command: codex app-server --profile custom
      options:
        approval_policy: never
        read_timeout_ms: 1000
        stall_timeout_ms: 0
  routes:
    - backend: codex-custom
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

	backends := workflow.Config.AgentBackendConfigs()
	if len(backends) != 1 {
		t.Fatalf("AgentBackendConfigs() len = %d, want 1", len(backends))
	}
	backend := backends[0]
	if backend.ID != "codex-custom" {
		t.Fatalf("backend ID = %q, want codex-custom", backend.ID)
	}
	if backend.Command != "codex app-server --profile custom" {
		t.Fatalf("backend Command = %q, want configured command", backend.Command)
	}
	options := backend.CodexOptions()
	if options.Shell != "bash" {
		t.Fatalf("backend shell = %q, want legacy default", options.Shell)
	}
	if !options.ApprovalPolicy.IsString || options.ApprovalPolicy.String != "never" {
		t.Fatalf("backend approval policy = %#v, want backend override", options.ApprovalPolicy)
	}
	if options.ThreadSandbox != "workspace-write" {
		t.Fatalf("backend thread sandbox = %q, want legacy default", options.ThreadSandbox)
	}
	if got := options.TurnSandboxPolicy["type"]; got != "workspaceWrite" {
		t.Fatalf("backend turn sandbox policy type = %v, want workspaceWrite", got)
	}
	if options.TurnTimeoutMS != 700000 {
		t.Fatalf("backend turn timeout = %d, want legacy default", options.TurnTimeoutMS)
	}
	if options.ReadTimeoutMS != 1000 {
		t.Fatalf("backend read timeout = %d, want backend override", options.ReadTimeoutMS)
	}
	if options.StallTimeoutMS != 70000 {
		t.Fatalf("backend stall timeout = %d, want legacy default for zero backend value", options.StallTimeoutMS)
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

func TestParseWorkflowCommandGateCIFailureAction(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
gate:
  kind: command
  run: make check
  ci_failure_action: rework
  transient_ci_retry_limit: 3
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
	if cfg.CIFailureAction != gate.CIFailureActionRework {
		t.Fatalf("Gate.CIFailureAction = %q, want %q", cfg.CIFailureAction, gate.CIFailureActionRework)
	}
	if cfg.TransientCIRetryLimit == nil || *cfg.TransientCIRetryLimit != 3 {
		t.Fatalf("Gate.TransientCIRetryLimit = %v, want 3", cfg.TransientCIRetryLimit)
	}
}

func TestParseWorkflowGateRequiredStatusChecks(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
gate:
  required_status_checks:
    - " Lint "
    - Windows Core
    - Lint
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	want := []string{"Lint", "Windows Core"}
	if !reflect.DeepEqual(workflow.Config.Gate.RequiredStatusChecks, want) {
		t.Fatalf("Gate.RequiredStatusChecks = %#v, want %#v", workflow.Config.Gate.RequiredStatusChecks, want)
	}
}

func TestParseWorkflowGateTransientCIRetryLimitCanDisable(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
gate:
  transient_ci_retry_limit: 0
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}

	cfg := workflow.Config.Gate
	if cfg.TransientCIRetryLimit == nil || *cfg.TransientCIRetryLimit != 0 {
		t.Fatalf("Gate.TransientCIRetryLimit = %v, want 0", cfg.TransientCIRetryLimit)
	}
}

func TestParseWorkflowGateValidatorConfig(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: memory
gate:
  kind: command
  validator:
    enabled: true
    model: gpt-5-validator
    min_score: 0.85
    turn_timeout_ms: 120000
    max_inline_diff_bytes: 32768
    block_on:
      - P1
      - p2
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	validator := workflow.Config.Gate.Validator
	if !validator.Enabled {
		t.Fatal("Gate.Validator.Enabled = false, want true")
	}
	if validator.Model != "gpt-5-validator" {
		t.Fatalf("Gate.Validator.Model = %q, want gpt-5-validator", validator.Model)
	}
	if validator.MinScore != 0.85 {
		t.Fatalf("Gate.Validator.MinScore = %v, want 0.85", validator.MinScore)
	}
	if validator.TurnTimeoutMS != 120000 {
		t.Fatalf("Gate.Validator.TurnTimeoutMS = %d, want 120000", validator.TurnTimeoutMS)
	}
	if validator.MaxInlineDiffBytes == nil || *validator.MaxInlineDiffBytes != 32768 {
		t.Fatalf("Gate.Validator.MaxInlineDiffBytes = %v, want 32768", validator.MaxInlineDiffBytes)
	}
	if got := validator.BlockOn; !reflect.DeepEqual(got, []string{"p1", "p2"}) {
		t.Fatalf("Gate.Validator.BlockOn = %#v, want p1/p2", got)
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

func TestParseWorkflowLocalSQLiteArtifactWorkflow(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: local_sqlite
  local_sqlite:
    path: .detent/work-items.db
    project_id: video-production
  active_states:
    - Todo
    - Production
  observed_states:
    - Review
    - Blocked
  terminal_states:
    - Done
workspace:
  kind: filesystem
  root: .detent/workspaces
  source_root: assets
  output_root: .detent/renders
deliverable:
  kind: artifact
  output_root: .detent/renders
  review_url: http://127.0.0.1:8080/review
agent:
  auto_promote:
    enabled: true
    source_state: Review
    pass_state: Done
    rework_state: Production
gate:
  kind: artifact
  artifact:
    status_field: render_status
    pass_statuses:
      - approved
    wait_statuses:
      - queued
    rework_statuses:
      - recut
server:
  kanban:
    mode: integration
---
Produce the video artifact.
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	cfg := workflow.Config
	if cfg.Tracker.Kind != TrackerLocalSQLite || cfg.Tracker.LocalSQLite.Path != ".detent/work-items.db" || cfg.Tracker.LocalSQLite.ProjectID != "video-production" {
		t.Fatalf("Tracker local sqlite config = %#v", cfg.Tracker)
	}
	if cfg.Workspace.Kind != WorkspaceFilesystem || cfg.Workspace.Root != ".detent/workspaces" || cfg.Workspace.OutputRoot != ".detent/renders" {
		t.Fatalf("Workspace = %#v", cfg.Workspace)
	}
	if cfg.Deliverable.Kind != DeliverableArtifact || cfg.Deliverable.OutputRoot != ".detent/renders" {
		t.Fatalf("Deliverable = %#v", cfg.Deliverable)
	}
	if cfg.Agent.AutoPromote.SourceState != "Review" || cfg.Agent.AutoPromote.PassState != "Done" || cfg.Agent.AutoPromote.ReworkState != "Production" {
		t.Fatalf("AutoPromote = %#v", cfg.Agent.AutoPromote)
	}
	if cfg.Gate.Kind != gate.KindArtifact ||
		cfg.Gate.Artifact.StatusField != "render_status" ||
		len(cfg.Gate.Artifact.PassStatuses) != 1 ||
		cfg.Gate.Artifact.PassStatuses[0] != "approved" {
		t.Fatalf("Gate = %#v", cfg.Gate)
	}
	if cfg.Server.Kanban.Mode != KanbanModeIntegration {
		t.Fatalf("Kanban mode = %q, want %q", cfg.Server.Kanban.Mode, KanbanModeIntegration)
	}
}

func TestParseWorkflowGitHubLocalTracker(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: github_local
  api_key: ghp_example
  repository: digitaldrywood/detent
  local_sqlite:
    path: .detent/work-items.db
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	cfg := workflow.Config
	if cfg.Tracker.Kind != TrackerGitHubLocal {
		t.Fatalf("Tracker.Kind = %q, want %q", cfg.Tracker.Kind, TrackerGitHubLocal)
	}
	if cfg.Tracker.Endpoint != defaultGitHubEndpoint {
		t.Fatalf("Tracker.Endpoint = %q, want %q", cfg.Tracker.Endpoint, defaultGitHubEndpoint)
	}
	if cfg.Tracker.GitHubStatusSource != GitHubStatusSourceProjectV2 {
		t.Fatalf("GitHubStatusSource default changed to %q", cfg.Tracker.GitHubStatusSource)
	}
}

func TestParseWorkflowGitHubLocalRejectsGitHubStatusSource(t *testing.T) {
	t.Parallel()

	workflow, err := ParseWorkflow([]byte(`---
tracker:
  kind: github_local
  api_key: ghp_example
  repository: digitaldrywood/detent
  github_status_source: label
  local_sqlite:
    path: .detent/work-items.db
---
Prompt
`))
	if err != nil {
		t.Fatalf("ParseWorkflow() error = %v", err)
	}
	err = workflow.Config.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want github_status_source rejection")
	}
	if !strings.Contains(err.Error(), "tracker.github_status_source must be omitted when tracker.kind is github_local") {
		t.Fatalf("Validate() error = %q, want github_status_source rejection", err)
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
  github_graphql_warn_remaining: 0
  github_graphql_min_remaining_reserve: 0
  github_rest_min_remaining_reserve: 0
  github_rest_fanout_max_requests: 0
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
  max_session_tokens: -1
  max_session_context_multiplier: -0.5
  output_truncation:
    max_bytes: -1
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
  kanban:
    mode: edit
    issue_state_field_id: -1
---
Prompt
`,
			want: []string{
				"tracker.active_states state names must not be blank",
				"tracker.http_max_idle_conns must be greater than 0",
				"tracker.http_max_idle_conns_per_host must be greater than 0",
				"tracker.http_idle_conn_timeout_ms must be greater than 0",
				"tracker.github_graphql_warn_remaining must be greater than 0",
				"tracker.github_graphql_min_remaining_reserve must be greater than 0",
				"tracker.github_rest_min_remaining_reserve must be greater than 0",
				"tracker.github_rest_fanout_max_requests must be greater than 0",
				"polling.interval_ms must be greater than 0",
				"worker.max_concurrent_agents_per_host must be greater than 0",
				"workspace.cleanup_idle_ttl_ms must be greater than 0",
				"workspace.cleanup_sweep_interval_ms must be greater than 0",
				"agent.max_concurrent_agents must be greater than 0",
				"agent.max_session_tokens must be greater than or equal to 0",
				"agent.max_session_context_multiplier must be greater than or equal to 0",
				"agent.output_truncation.max_bytes must be greater than or equal to 0",
				"agent.max_concurrent_agents_by_state limits must be positive integers",
				"agent.dispatch_priority_by_state state names must be unique",
				"agent.dispatch_priority_by_label labels must not be blank",
				"codex.turn_timeout_ms must be greater than 0",
				"hooks.timeout_ms must be greater than 0",
				"observability.refresh_ms must be greater than 0",
				"budget.per_day_max_usd must be greater than 0",
				"server.port must be greater than or equal to 0",
				"server.kanban.mode must be one of read_only, integration",
				"server.kanban.issue_state_field_id must be greater than 0 when set",
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
				"tracker.active_states must include Rework when tracker.dependency_auto_unblock.enabled is true",
			},
		},
		{
			name: "dependency auto unblock requires active rework",
			raw: `---
tracker:
  kind: memory
  active_states:
    - Todo
    - In Progress
  dependency_auto_unblock:
    enabled: true
    source_states:
      - Blocked
    target_state: Todo
    readiness: terminal_or_merged
---
Prompt
`,
			want: []string{
				"tracker.active_states must include Rework when tracker.dependency_auto_unblock.enabled is true",
			},
		},
		{
			name: "invalid blocker auto promote config",
			raw: `---
tracker:
  kind: memory
  blocker_auto_promote:
    enabled: true
    source_states: [""]
    blocker_states: [""]
    target_state: ""
---
Prompt
`,
			want: []string{
				"tracker.blocker_auto_promote.source_states state names must not be blank",
				"tracker.blocker_auto_promote.blocker_states state names must not be blank",
				"tracker.blocker_auto_promote.target_state is required when tracker.blocker_auto_promote.enabled is true",
			},
		},
		{
			name: "invalid transient ci retry limit",
			raw: `---
tracker:
  kind: memory
gate:
  transient_ci_retry_limit: -1
---
Prompt
`,
			want: []string{
				"gate.transient_ci_retry_limit must be greater than or equal to 0",
			},
		},
		{
			name: "invalid auto promote rework limit",
			raw: `---
tracker:
  kind: memory
agent:
  auto_promote:
    rework_limit: -1
---
Prompt
`,
			want: []string{
				"agent.auto_promote.rework_limit must be greater than or equal to 0",
			},
		},
		{
			name: "invalid auto promote gate wait settings",
			raw: `---
tracker:
  kind: memory
agent:
  auto_promote:
    gate_wait_state: backlog
    gate_wait_timeout_seconds: -1
---
Prompt
`,
			want: []string{
				"agent.auto_promote.gate_wait_state must be one of source, review",
				"agent.auto_promote.gate_wait_timeout_seconds must be greater than 0",
			},
		},
		{
			name: "auto promote rework limit requires blocked state",
			raw: `---
tracker:
  kind: memory
  active_states:
    - Todo
    - In Progress
    - Rework
    - Merging
  observed_states:
    - Human Review
  terminal_states:
    - Done
agent:
  auto_promote:
    enabled: true
    rework_limit: 1
---
Prompt
`,
			want: []string{
				"tracker.active_states, tracker.observed_states, or tracker.terminal_states must include Blocked when agent.auto_promote.rework_limit is greater than 0",
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
  knowledge:
    sources:
      - path: ""
  skills:
    path: /tmp/skills
    creation:
      max_drafts_per_run: 0
---
Prompt
`,
			want: []string{
				"tracker.priority_map option names must not be blank",
				"tracker.priority_map ranks must be integers 1 through 4 or null",
				"agent.lessons.path must be a relative path inside the workspace",
				"agent.knowledge.sources[0].path must not be blank",
				"agent.skills.path must be a relative path inside the workspace",
				"agent.skills.creation.max_drafts_per_run must be greater than 0",
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
				"agents.backends.kind must be one of codex, claude_code",
				"agents.backends.command is required",
				"agents.routes.backend must reference a configured backend",
				"agents.routes.selector.priority_in values must be integers 1 through 4",
				"agents.routes must not define multiple default routes for the same role",
			},
		},
		{
			name: "invalid claude code protocol",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: claude
      kind: claude_code
      protocol: app-server
  routes:
    - backend: claude
      default: true
---
Prompt
`,
			want: []string{
				"agents.backends.protocol must be headless for claude_code",
			},
		},
		{
			name: "invalid agent backend options decode",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: codex
      kind: codex
      protocol: app-server
      command: codex app-server
      options:
        approval_policy: [never]
  routes:
    - backend: codex
      default: true
---
Prompt
`,
			want: []string{
				"agents.backends.options must decode for codex",
			},
		},
		{
			name: "invalid claude code permission mode",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: claude
      kind: claude_code
      options:
        permission_mode: ask
  routes:
    - backend: claude
      default: true
---
Prompt
`,
			want: []string{
				"agents.backends.options.permission_mode must be one of default, acceptEdits, bypassPermissions",
			},
		},
		{
			name: "invalid claude code plan permission mode",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: claude
      kind: claude_code
      options:
        permission_mode: plan
  routes:
    - backend: claude
      default: true
---
Prompt
`,
			want: []string{
				"agents.backends.options.permission_mode must not be plan for unattended workers",
			},
		},
		{
			name: "invalid claude code option timeouts",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: claude
      kind: claude_code
      options:
        turn_timeout_ms: -1
        stall_timeout_ms: -1
  routes:
    - backend: claude
      default: true
---
Prompt
`,
			want: []string{
				"agents.backends.options.turn_timeout_ms must be greater than or equal to 0",
				"agents.backends.options.stall_timeout_ms must be greater than or equal to 0",
			},
		},
		{
			name: "invalid agent backend option timeouts",
			raw: `---
tracker:
  kind: memory
agents:
  backends:
    - id: codex
      kind: codex
      protocol: app-server
      command: codex app-server
      options:
        turn_timeout_ms: -1
        read_timeout_ms: -1
        stall_timeout_ms: -1
  routes:
    - backend: codex
      default: true
---
Prompt
`,
			want: []string{
				"agents.backends.options.turn_timeout_ms must be greater than or equal to 0",
				"agents.backends.options.read_timeout_ms must be greater than or equal to 0",
				"agents.backends.options.stall_timeout_ms must be greater than or equal to 0",
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
  ci_failure_action: bounce
---
Prompt
`,
			want: []string{
				"gate.kind must be one of command, human_review, artifact",
				"gate.ci_failure_action must be one of skip, rework",
			},
		},
		{
			name: "invalid validator config",
			raw: `---
tracker:
  kind: memory
gate:
  validator:
    enabled: true
    min_score: 1.2
    turn_timeout_ms: -1
    max_inline_diff_bytes: -1
    block_on:
      - ""
---
Prompt
`,
			want: []string{
				"gate.validator.min_score must be greater than 0 and less than or equal to 1",
				"gate.validator.turn_timeout_ms must be greater than or equal to 0",
				"gate.validator.max_inline_diff_bytes must be greater than or equal to 0",
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
