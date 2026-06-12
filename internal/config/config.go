package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/pathsafe"
	"github.com/digitaldrywood/detent/internal/selector"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

const (
	TrackerGitHub = "github"
	TrackerLinear = "linear"
	TrackerMemory = "memory"

	defaultLinearEndpoint = "https://api.linear.app/graphql"
	defaultGitHubEndpoint = "https://api.github.com/graphql"

	DefaultAgentBackendID = "codex"
	AgentBackendCodex     = "codex"

	DefaultPollingIntervalMS      = 120000
	MinPollingIntervalMS          = 60000
	DefaultShutdownDrainTimeoutMS = 600000

	defaultCodexProtocol = "app-server"

	IdentityOwnershipAssignee = "assignee"
	IdentityOwnershipField    = "field"
)

type Workflow struct {
	Config Config
	Prompt string
}

type Config struct {
	Identity      Identity      `yaml:"identity,omitempty"`
	Tracker       Tracker       `yaml:"tracker"`
	Polling       Polling       `yaml:"polling"`
	Workspace     Workspace     `yaml:"workspace"`
	Worker        Worker        `yaml:"worker"`
	Agent         Agent         `yaml:"agent"`
	Agents        Agents        `yaml:"agents"`
	Codex         Codex         `yaml:"codex"`
	Gate          gate.Config   `yaml:"gate"`
	Server        Server        `yaml:"server"`
	Observability Observability `yaml:"observability"`
	Budget        Budget        `yaml:"budget"`
	Hooks         Hooks         `yaml:"hooks"`
}

type Tracker struct {
	Kind                       string            `yaml:"kind"`
	Endpoint                   string            `yaml:"endpoint"`
	APIKey                     string            `yaml:"api_key"`
	HTTPMaxIdleConns           int               `yaml:"http_max_idle_conns"`
	HTTPMaxIdleConnsPerHost    int               `yaml:"http_max_idle_conns_per_host"`
	HTTPIdleConnTimeoutMS      int               `yaml:"http_idle_conn_timeout_ms"`
	GitHubGraphQLWarnRemaining int               `yaml:"github_graphql_warn_remaining"`
	GitHubAppID                string            `yaml:"github_app_id"`
	GitHubAppPrivateKey        string            `yaml:"github_app_private_key"`
	GitHubAppPrivateKeyPath    string            `yaml:"github_app_private_key_path"`
	GitHubAppInstallationID    string            `yaml:"github_app_installation_id"`
	ProjectSlug                string            `yaml:"project_slug"`
	Assignee                   string            `yaml:"assignee"`
	ActiveStates               []string          `yaml:"active_states"`
	ObservedStates             []string          `yaml:"observed_states"`
	TerminalStates             []string          `yaml:"terminal_states"`
	StateMap                   StringOrMap       `yaml:"state_map"`
	PriorityMap                StringOrMap       `yaml:"priority_map"`
	AutoProvision              bool              `yaml:"auto_provision"`
	Claims                     Claims            `yaml:"claims,omitempty"`
	Authorization              selector.Selector `yaml:"authorization,omitempty"`
	Issues                     []connector.Issue `yaml:"issues"`
}

type Identity struct {
	Name          string `yaml:"name"`
	GitHubLogin   string `yaml:"github_login,omitempty"`
	OwnershipMode string `yaml:"ownership_mode,omitempty"`
	OwnerField    string `yaml:"owner_field,omitempty"`
}

type Polling struct {
	IntervalMS int `yaml:"interval_ms"`
}

type Claims struct {
	Enabled          bool   `yaml:"enabled"`
	LeaseField       string `yaml:"lease_field,omitempty"`
	TTLSeconds       int    `yaml:"ttl_seconds,omitempty"`
	HeartbeatSeconds int    `yaml:"heartbeat_seconds,omitempty"`
}

type Workspace struct {
	Root                   string `yaml:"root"`
	SourceRoot             string `yaml:"source_root"`
	AutoBranch             bool   `yaml:"auto_branch"`
	CleanupIdleTTLMS       int    `yaml:"cleanup_idle_ttl_ms"`
	CleanupSweepIntervalMS int    `yaml:"cleanup_sweep_interval_ms"`
}

type Worker struct {
	SSHHosts                   []string `yaml:"ssh_hosts"`
	MaxConcurrentAgentsPerHost *int     `yaml:"max_concurrent_agents_per_host"`
}

type Agent struct {
	MaxConcurrentAgents        int            `yaml:"max_concurrent_agents"`
	MaxTurns                   int            `yaml:"max_turns"`
	MaxRetryBackoffMS          int            `yaml:"max_retry_backoff_ms"`
	Shutdown                   Shutdown       `yaml:"shutdown"`
	MaxConcurrentAgentsByState map[string]int `yaml:"max_concurrent_agents_by_state"`
	DispatchPriorityByState    []string       `yaml:"dispatch_priority_by_state"`
	AutoPromote                AutoPromote    `yaml:"auto_promote"`
	Budget                     Budget         `yaml:"budget"`
	Lessons                    Lessons        `yaml:"lessons"`
	Skills                     Skills         `yaml:"skills"`
}

type Shutdown struct {
	DrainTimeoutMS int `yaml:"drain_timeout_ms"`
}

type Agents struct {
	Backends []AgentBackend `yaml:"backends"`
	Routes   []AgentRoute   `yaml:"routes"`
}

type AgentBackend struct {
	ID       string              `yaml:"id"`
	Kind     string              `yaml:"kind"`
	Protocol string              `yaml:"protocol"`
	Command  string              `yaml:"command"`
	Options  AgentBackendOptions `yaml:"options"`
}

type AgentBackendOptions struct {
	Shell             string         `yaml:"shell"`
	ApprovalPolicy    StringOrMap    `yaml:"approval_policy"`
	ThreadSandbox     string         `yaml:"thread_sandbox"`
	TurnSandboxPolicy map[string]any `yaml:"turn_sandbox_policy"`
	TurnTimeoutMS     int            `yaml:"turn_timeout_ms"`
	ReadTimeoutMS     int            `yaml:"read_timeout_ms"`
	StallTimeoutMS    int            `yaml:"stall_timeout_ms"`
}

type AgentRoute struct {
	Name       string            `yaml:"name"`
	Backend    string            `yaml:"backend"`
	Model      string            `yaml:"model"`
	ModelField string            `yaml:"model_field"`
	Default    bool              `yaml:"default"`
	Selector   selector.Selector `yaml:"selector"`
}

type AutoPromote struct {
	Enabled            bool     `yaml:"enabled"`
	QuietSeconds       int      `yaml:"quiet_seconds"`
	OptoutLabel        string   `yaml:"optout_label"`
	AllowedIssueLabels []string `yaml:"allowed_issue_labels"`
}

type Lessons struct {
	Enabled             bool   `yaml:"enabled"`
	Path                string `yaml:"path"`
	MaxEntries          int    `yaml:"max_entries"`
	RecallN             int    `yaml:"recall_n"`
	PostmortemMaxTokens int    `yaml:"postmortem_max_tokens"`
}

type Skills struct {
	Enabled           bool   `yaml:"enabled"`
	Path              string `yaml:"path"`
	MaxSkillsInPrompt int    `yaml:"max_skills_in_prompt"`
}

type Budget struct {
	Enabled                bool    `yaml:"enabled"`
	PerDayMaxUSD           float64 `yaml:"per_day_max_usd"`
	PerIssueMaxUSD         float64 `yaml:"per_issue_max_usd"`
	RefusalCooldownSeconds int     `yaml:"refusal_cooldown_seconds"`
	PricingPath            string  `yaml:"pricing_path"`
}

type Codex struct {
	Command           string         `yaml:"command"`
	Shell             string         `yaml:"shell"`
	ApprovalPolicy    StringOrMap    `yaml:"approval_policy"`
	ThreadSandbox     string         `yaml:"thread_sandbox"`
	TurnSandboxPolicy map[string]any `yaml:"turn_sandbox_policy"`
	TurnTimeoutMS     int            `yaml:"turn_timeout_ms"`
	ReadTimeoutMS     int            `yaml:"read_timeout_ms"`
	StallTimeoutMS    int            `yaml:"stall_timeout_ms"`
}

func (c Config) AgentBackendConfigs() []AgentBackend {
	if len(c.Agents.Backends) > 0 {
		backends := make([]AgentBackend, len(c.Agents.Backends))
		copy(backends, c.Agents.Backends)
		return backends
	}
	return []AgentBackend{CodexAgentBackend(c.Codex)}
}

func (c Config) AgentRouteConfigs() []AgentRoute {
	if len(c.Agents.Routes) > 0 {
		routes := make([]AgentRoute, len(c.Agents.Routes))
		copy(routes, c.Agents.Routes)
		return routes
	}

	backendID := DefaultAgentBackendID
	if backends := c.AgentBackendConfigs(); len(backends) > 0 {
		backendID = backends[0].ID
	}
	return []AgentRoute{{
		Name:    "default",
		Backend: backendID,
		Default: true,
	}}
}

func CodexAgentBackend(codex Codex) AgentBackend {
	return AgentBackend{
		ID:       DefaultAgentBackendID,
		Kind:     AgentBackendCodex,
		Protocol: defaultCodexProtocol,
		Command:  strings.TrimSpace(codex.Command),
		Options: AgentBackendOptions{
			Shell:             codex.Shell,
			ApprovalPolicy:    codex.ApprovalPolicy,
			ThreadSandbox:     codex.ThreadSandbox,
			TurnSandboxPolicy: codex.TurnSandboxPolicy,
			TurnTimeoutMS:     codex.TurnTimeoutMS,
			ReadTimeoutMS:     codex.ReadTimeoutMS,
			StallTimeoutMS:    codex.StallTimeoutMS,
		},
	}
}

func (b AgentBackend) CodexConfig(fallback Codex) Codex {
	cfg := fallback
	if strings.TrimSpace(b.Command) != "" {
		cfg.Command = strings.TrimSpace(b.Command)
	}
	if strings.TrimSpace(b.Options.Shell) != "" {
		cfg.Shell = b.Options.Shell
	}
	if b.Options.ApprovalPolicy.IsString || b.Options.ApprovalPolicy.IsMap {
		cfg.ApprovalPolicy = b.Options.ApprovalPolicy
	}
	if strings.TrimSpace(b.Options.ThreadSandbox) != "" {
		cfg.ThreadSandbox = strings.TrimSpace(b.Options.ThreadSandbox)
	}
	if b.Options.TurnSandboxPolicy != nil {
		cfg.TurnSandboxPolicy = b.Options.TurnSandboxPolicy
	}
	if b.Options.TurnTimeoutMS > 0 {
		cfg.TurnTimeoutMS = b.Options.TurnTimeoutMS
	}
	if b.Options.ReadTimeoutMS > 0 {
		cfg.ReadTimeoutMS = b.Options.ReadTimeoutMS
	}
	if b.Options.StallTimeoutMS > 0 {
		cfg.StallTimeoutMS = b.Options.StallTimeoutMS
	}
	return cfg
}

type Server struct {
	Port *int   `yaml:"port"`
	Host string `yaml:"host"`
}

type Observability struct {
	DashboardEnabled bool `yaml:"dashboard_enabled"`
	RefreshMS        int  `yaml:"refresh_ms"`
	RenderIntervalMS int  `yaml:"render_interval_ms"`
}

type Hooks struct {
	Shell        string `yaml:"shell"`
	AfterCreate  string `yaml:"after_create"`
	BeforeRun    string `yaml:"before_run"`
	AfterRun     string `yaml:"after_run"`
	BeforeRemove string `yaml:"before_remove"`
	TimeoutMS    int    `yaml:"timeout_ms"`
}

func (i Identity) Configured() bool {
	return strings.TrimSpace(i.Name) != "" ||
		strings.TrimSpace(i.GitHubLogin) != "" ||
		strings.TrimSpace(i.OwnershipMode) != "" ||
		strings.TrimSpace(i.OwnerField) != ""
}

func (i Identity) IsZero() bool {
	return !i.Configured()
}

func (i *Identity) Normalize() {
	if i == nil {
		return
	}
	i.Name = strings.TrimSpace(i.Name)
	i.GitHubLogin = strings.TrimSpace(i.GitHubLogin)
	i.OwnershipMode = strings.ToLower(strings.TrimSpace(i.OwnershipMode))
	i.OwnerField = strings.TrimSpace(i.OwnerField)
	if i.OwnershipMode == "" && i.Configured() {
		i.OwnershipMode = IdentityOwnershipAssignee
	}
}

func (i Identity) Validate(prefix string) []string {
	if !i.Configured() {
		return nil
	}

	identity := i
	identity.Normalize()

	var problems []string
	if identity.Name == "" {
		problems = append(problems, prefix+".name must not be blank")
	}
	switch identity.OwnershipMode {
	case IdentityOwnershipAssignee:
		if identity.OwnerField != "" {
			problems = append(problems, prefix+".owner_field must be blank when "+prefix+".ownership_mode is assignee")
		}
	case IdentityOwnershipField:
		if identity.OwnerField == "" {
			problems = append(problems, prefix+".owner_field is required when "+prefix+".ownership_mode is field")
		}
	default:
		problems = append(problems, prefix+".ownership_mode must be one of assignee, field")
	}
	return problems
}

func (c *Claims) Normalize() {
	if c == nil {
		return
	}
	c.LeaseField = strings.TrimSpace(c.LeaseField)
}

func (c Claims) Validate(prefix string) []string {
	if !c.Enabled {
		return nil
	}

	var problems []string
	if strings.TrimSpace(c.LeaseField) == "" {
		problems = append(problems, prefix+".lease_field must not be blank when "+prefix+".enabled is true")
	}
	if c.TTLSeconds <= 0 {
		problems = append(problems, prefix+".ttl_seconds must be greater than 0 when "+prefix+".enabled is true")
	}
	if c.HeartbeatSeconds <= 0 {
		problems = append(problems, prefix+".heartbeat_seconds must be greater than 0 when "+prefix+".enabled is true")
	}
	if c.TTLSeconds > 0 && c.HeartbeatSeconds > c.TTLSeconds {
		problems = append(problems, prefix+".heartbeat_seconds must be less than or equal to "+prefix+".ttl_seconds")
	}
	return problems
}

type StringOrMap struct {
	IsString bool
	String   string
	IsMap    bool
	Map      map[string]any
}

type ValidationError struct {
	Problems []string
}

func LoadWorkflow(path string) (Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Workflow{}, fmt.Errorf("read workflow file: %w", err)
	}

	return ParseWorkflow(raw)
}

func ParseWorkflow(raw []byte) (Workflow, error) {
	frontmatter, prompt, err := splitFrontmatter(raw)
	if err != nil {
		return Workflow{}, err
	}

	cfg := Default()
	if len(bytes.TrimSpace(frontmatter)) > 0 {
		var doc yaml.Node
		if err := yaml.Unmarshal(frontmatter, &doc); err != nil {
			return Workflow{}, fmt.Errorf("parse YAML frontmatter: %w", err)
		}

		root, err := documentRoot(&doc)
		if err != nil {
			return Workflow{}, err
		}
		if root.Kind != yaml.MappingNode {
			return Workflow{}, errors.New("workflow frontmatter must be a mapping")
		}
		normalizeTrackerIDFields(root)
		if err := root.Decode(&cfg); err != nil {
			return Workflow{}, fmt.Errorf("decode YAML frontmatter: %w", err)
		}
	}

	cfg.normalize()

	return Workflow{
		Config: cfg,
		Prompt: string(prompt),
	}, nil
}

func Default() Config {
	budget := defaultBudget()

	return Config{
		Tracker: Tracker{
			Endpoint:                   defaultLinearEndpoint,
			HTTPMaxIdleConns:           100,
			HTTPMaxIdleConnsPerHost:    32,
			HTTPIdleConnTimeoutMS:      90000,
			GitHubGraphQLWarnRemaining: 500,
			ActiveStates:               []string{"Todo", "In Progress"},
			ObservedStates:             []string{"Backlog", "Human Review", "Blocked"},
			TerminalStates:             []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
			StateMap:                   MapValue(map[string]any{}),
			PriorityMap:                MapValue(defaultPriorityMap()),
			AutoProvision:              true,
		},
		Polling: Polling{
			IntervalMS: DefaultPollingIntervalMS,
		},
		Workspace: Workspace{
			Root:                   filepath.Join(os.TempDir(), "detent_workspaces"),
			AutoBranch:             true,
			CleanupIdleTTLMS:       86400000,
			CleanupSweepIntervalMS: 600000,
		},
		Worker: Worker{
			SSHHosts: []string{},
		},
		Agent: Agent{
			MaxConcurrentAgents:        10,
			MaxTurns:                   20,
			MaxRetryBackoffMS:          300000,
			Shutdown:                   Shutdown{DrainTimeoutMS: DefaultShutdownDrainTimeoutMS},
			MaxConcurrentAgentsByState: map[string]int{},
			DispatchPriorityByState:    []string{},
			AutoPromote: AutoPromote{
				QuietSeconds:       600,
				OptoutLabel:        "requires-human-review",
				AllowedIssueLabels: []string{},
			},
			Budget:  budget,
			Lessons: defaultLessons(),
			Skills:  defaultSkills(),
		},
		Codex: Codex{
			Command: "codex app-server",
			Shell:   commandshell.Default(),
			ApprovalPolicy: MapValue(map[string]any{
				"reject": map[string]any{
					"sandbox_approval": true,
					"rules":            true,
					"mcp_elicitations": true,
				},
			}),
			ThreadSandbox:  "workspace-write",
			TurnTimeoutMS:  3600000,
			ReadTimeoutMS:  5000,
			StallTimeoutMS: 300000,
		},
		Gate: gate.DefaultConfig(),
		Server: Server{
			Host: "127.0.0.1",
		},
		Observability: Observability{
			DashboardEnabled: true,
			RefreshMS:        1000,
			RenderIntervalMS: 16,
		},
		Budget: budget,
		Hooks: Hooks{
			Shell:     commandshell.Default(),
			TimeoutMS: 60000,
		},
	}
}

func MapValue(value map[string]any) StringOrMap {
	return StringOrMap{
		IsMap: true,
		Map:   value,
	}
}

func StringValue(value string) StringOrMap {
	return StringOrMap{
		IsString: true,
		String:   value,
	}
}

func (c *Config) Validate() error {
	var problems []string

	problems = append(problems, c.Identity.Validate("identity")...)
	c.validateTracker(&problems)
	validatePollingInterval(c.Polling.IntervalMS, &problems)
	c.Workspace.validate(&problems)
	if c.Worker.MaxConcurrentAgentsPerHost != nil {
		validatePositive("worker.max_concurrent_agents_per_host", *c.Worker.MaxConcurrentAgentsPerHost, &problems)
	}
	c.Agent.validate("agent", &problems)
	c.Agents.validate(&problems)
	c.Codex.validate(&problems)
	problems = append(problems, gate.Validate("gate", c.Gate)...)
	c.Server.validate(&problems)
	c.Observability.validate(&problems)
	c.Budget.validate("budget", &problems)
	c.Hooks.validate(&problems)

	if len(problems) > 0 {
		return ValidationError{Problems: problems}
	}

	return nil
}

func (e ValidationError) Error() string {
	return strings.Join(e.Problems, "; ")
}

func (s *StringOrMap) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
		return nil
	}

	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag != "!!str" {
			return fmt.Errorf("must be a string or mapping, got %s", yamlKindName(value))
		}
		*s = StringValue(value.Value)
		return nil
	case yaml.MappingNode:
		decoded, err := decodeMapNode(value)
		if err != nil {
			return err
		}
		*s = MapValue(decoded)
		return nil
	default:
		return fmt.Errorf("must be a string or mapping, got %s", yamlKindName(value))
	}
}

func (c *Config) normalize() {
	c.Identity.Normalize()
	c.Tracker.Kind = strings.ToLower(strings.TrimSpace(c.Tracker.Kind))
	if c.Tracker.Kind == TrackerGitHub && c.Tracker.Endpoint == defaultLinearEndpoint {
		c.Tracker.Endpoint = defaultGitHubEndpoint
	}
	c.Tracker.Claims.Normalize()
	c.Tracker.Authorization.Normalize()

	c.Agent.MaxConcurrentAgentsByState = normalizeStateLimits(c.Agent.MaxConcurrentAgentsByState)
	c.Agent.DispatchPriorityByState = normalizeStateList(c.Agent.DispatchPriorityByState)
	c.Agent.AutoPromote.OptoutLabel = normalizeLabel(c.Agent.AutoPromote.OptoutLabel)
	c.Agent.AutoPromote.AllowedIssueLabels = normalizeLabels(c.Agent.AutoPromote.AllowedIssueLabels)
	c.Agents.normalize()
	c.Codex.Shell = commandshell.Normalize(c.Codex.Shell)
	c.Gate = gate.Effective(c.Gate)
	c.Hooks.Shell = commandshell.Normalize(c.Hooks.Shell)
}

func (c *Config) validateTracker(problems *[]string) {
	switch c.Tracker.Kind {
	case "":
		*problems = append(*problems, "tracker.kind is required")
	case TrackerLinear:
		validateRequired("tracker.api_key", c.Tracker.APIKey, " for linear", problems)
		validateRequired("tracker.project_slug", c.Tracker.ProjectSlug, " for linear", problems)
	case TrackerGitHub:
		c.Tracker.validateGitHubAuth(problems)
		validateRequired("tracker.project_slug", c.Tracker.ProjectSlug, " for github", problems)
	case TrackerMemory:
	default:
		*problems = append(*problems, "tracker.kind must be one of github, linear, memory")
	}

	validateStateList("tracker.active_states", c.Tracker.ActiveStates, problems)
	validateStateList("tracker.observed_states", c.Tracker.ObservedStates, problems)
	validateStateList("tracker.terminal_states", c.Tracker.TerminalStates, problems)
	validateStateMap("tracker.state_map", c.Tracker.StateMap, problems)
	validatePriorityMap("tracker.priority_map", c.Tracker.PriorityMap, problems)
	validatePositive("tracker.http_max_idle_conns", c.Tracker.HTTPMaxIdleConns, problems)
	validatePositive("tracker.http_max_idle_conns_per_host", c.Tracker.HTTPMaxIdleConnsPerHost, problems)
	validatePositive("tracker.http_idle_conn_timeout_ms", c.Tracker.HTTPIdleConnTimeoutMS, problems)
	validatePositive("tracker.github_graphql_warn_remaining", c.Tracker.GitHubGraphQLWarnRemaining, problems)
	*problems = append(*problems, c.Tracker.Claims.Validate("tracker.claims")...)
	*problems = append(*problems, c.Tracker.Authorization.Validate("tracker.authorization")...)
}

func (t *Tracker) validateGitHubAuth(problems *[]string) {
	if strings.TrimSpace(t.APIKey) != "" || t.hasGitHubAppCredentials() {
		return
	}

	if strings.TrimSpace(t.GitHubAppID) == "" &&
		strings.TrimSpace(t.GitHubAppInstallationID) == "" &&
		strings.TrimSpace(t.GitHubAppPrivateKey) == "" &&
		strings.TrimSpace(t.GitHubAppPrivateKeyPath) == "" {
		*problems = append(*problems, "tracker.api_key or GitHub App credentials are required for github")
		return
	}

	validateRequired("tracker.github_app_id", t.GitHubAppID, " for github app", problems)
	validateRequired("tracker.github_app_installation_id", t.GitHubAppInstallationID, " for github app", problems)
	if strings.TrimSpace(t.GitHubAppPrivateKey) == "" && strings.TrimSpace(t.GitHubAppPrivateKeyPath) == "" {
		*problems = append(*problems, "tracker.github_app_private_key or tracker.github_app_private_key_path is required for github app")
	}
}

func (t *Tracker) hasGitHubAppCredentials() bool {
	return strings.TrimSpace(t.GitHubAppID) != "" &&
		strings.TrimSpace(t.GitHubAppInstallationID) != "" &&
		(strings.TrimSpace(t.GitHubAppPrivateKey) != "" || strings.TrimSpace(t.GitHubAppPrivateKeyPath) != "")
}

func (w *Workspace) validate(problems *[]string) {
	validatePositive("workspace.cleanup_idle_ttl_ms", w.CleanupIdleTTLMS, problems)
	validatePositive("workspace.cleanup_sweep_interval_ms", w.CleanupSweepIntervalMS, problems)
}

func (a *Agent) validate(prefix string, problems *[]string) {
	validatePositive(prefix+".max_concurrent_agents", a.MaxConcurrentAgents, problems)
	validatePositive(prefix+".max_turns", a.MaxTurns, problems)
	validatePositive(prefix+".max_retry_backoff_ms", a.MaxRetryBackoffMS, problems)
	a.Shutdown.validate(prefix+".shutdown", problems)
	validateStateLimits(prefix+".max_concurrent_agents_by_state", a.MaxConcurrentAgentsByState, problems)
	validateStateList(prefix+".dispatch_priority_by_state", a.DispatchPriorityByState, problems)
	a.AutoPromote.validate(prefix+".auto_promote", problems)
	a.Budget.validate(prefix+".budget", problems)
	a.Lessons.validate(prefix+".lessons", problems)
	a.Skills.validate(prefix+".skills", problems)
}

func (s Shutdown) validate(prefix string, problems *[]string) {
	if s.DrainTimeoutMS < 0 {
		*problems = append(*problems, prefix+".drain_timeout_ms must be greater than or equal to 0")
	}
}

func (a *Agents) normalize() {
	for index := range a.Backends {
		backend := &a.Backends[index]
		backend.ID = strings.TrimSpace(backend.ID)
		backend.Kind = strings.ToLower(strings.TrimSpace(backend.Kind))
		backend.Protocol = normalizeAgentProtocol(backend.Protocol)
		if backend.Protocol == "" && backend.Kind == AgentBackendCodex {
			backend.Protocol = defaultCodexProtocol
		}
		backend.Command = strings.TrimSpace(backend.Command)
		backend.Options.Shell = strings.TrimSpace(backend.Options.Shell)
		if backend.Options.Shell != "" {
			backend.Options.Shell = commandshell.Normalize(backend.Options.Shell)
		}
		backend.Options.ThreadSandbox = strings.TrimSpace(backend.Options.ThreadSandbox)
	}
	for index := range a.Routes {
		route := &a.Routes[index]
		route.Name = strings.TrimSpace(route.Name)
		route.Backend = strings.TrimSpace(route.Backend)
		route.Model = strings.TrimSpace(route.Model)
		route.ModelField = strings.TrimSpace(route.ModelField)
	}
}

func normalizeAgentProtocol(protocol string) string {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "app_server", "appserver":
		return defaultCodexProtocol
	default:
		return strings.ToLower(strings.TrimSpace(protocol))
	}
}

func (a *Agents) validate(problems *[]string) {
	backendIDs := make(map[string]struct{}, len(a.Backends))
	if len(a.Backends) == 0 {
		backendIDs[DefaultAgentBackendID] = struct{}{}
	}
	for _, backend := range a.Backends {
		if strings.TrimSpace(backend.ID) == "" {
			*problems = append(*problems, "agents.backends.id is required")
			continue
		}
		if _, ok := backendIDs[backend.ID]; ok {
			*problems = append(*problems, "agents.backends ids must be unique")
		}
		backendIDs[backend.ID] = struct{}{}

		switch backend.Kind {
		case "":
			*problems = append(*problems, "agents.backends.kind is required")
		case AgentBackendCodex:
		default:
			*problems = append(*problems, "agents.backends.kind must be codex")
		}
		if backend.Kind == AgentBackendCodex && backend.Protocol != defaultCodexProtocol {
			*problems = append(*problems, "agents.backends.protocol must be app-server for codex")
		}
		validateRequired("agents.backends.command", backend.Command, "", problems)
		backend.Options.validate("agents.backends.options", problems)
	}

	defaultRoutes := 0
	for _, route := range a.Routes {
		if strings.TrimSpace(route.Backend) == "" {
			*problems = append(*problems, "agents.routes.backend is required")
		} else if _, ok := backendIDs[route.Backend]; !ok {
			*problems = append(*problems, "agents.routes.backend must reference a configured backend")
		}
		if route.Default {
			defaultRoutes++
		}
		validatePriorityValues("agents.routes.selector.priority_in", route.Selector.PriorityIn, problems)
	}
	if defaultRoutes > 1 {
		*problems = append(*problems, "agents.routes must not define multiple default routes")
	}
}

func (o *AgentBackendOptions) validate(prefix string, problems *[]string) {
	if o.TurnTimeoutMS < 0 {
		*problems = append(*problems, prefix+".turn_timeout_ms must be greater than or equal to 0")
	}
	if o.ReadTimeoutMS < 0 {
		*problems = append(*problems, prefix+".read_timeout_ms must be greater than or equal to 0")
	}
	if o.StallTimeoutMS < 0 {
		*problems = append(*problems, prefix+".stall_timeout_ms must be greater than or equal to 0")
	}
}

func validatePriorityValues(field string, priorities []int, problems *[]string) {
	for _, priority := range priorities {
		if priority < 1 || priority > 4 {
			*problems = append(*problems, field+" values must be integers 1 through 4")
			return
		}
	}
}

func (a *AutoPromote) validate(prefix string, problems *[]string) {
	if a.QuietSeconds < 0 {
		*problems = append(*problems, prefix+".quiet_seconds must be greater than or equal to 0")
	}
	if strings.TrimSpace(a.OptoutLabel) == "" {
		*problems = append(*problems, prefix+".optout_label must not be blank")
	}
	for _, label := range a.AllowedIssueLabels {
		if strings.TrimSpace(label) == "" {
			*problems = append(*problems, prefix+".allowed_issue_labels labels must not be blank")
			return
		}
	}
}

func (l *Lessons) validate(prefix string, problems *[]string) {
	validateWorkspaceRelativePath(prefix+".path", l.Path, problems)
	validatePositive(prefix+".max_entries", l.MaxEntries, problems)
	if l.RecallN < 0 {
		*problems = append(*problems, prefix+".recall_n must be greater than or equal to 0")
	}
	validatePositive(prefix+".postmortem_max_tokens", l.PostmortemMaxTokens, problems)
}

func (s *Skills) validate(prefix string, problems *[]string) {
	validateWorkspaceRelativePath(prefix+".path", s.Path, problems)
	validatePositive(prefix+".max_skills_in_prompt", s.MaxSkillsInPrompt, problems)
}

func (b *Budget) validate(prefix string, problems *[]string) {
	validatePositiveFloat(prefix+".per_day_max_usd", b.PerDayMaxUSD, problems)
	validatePositiveFloat(prefix+".per_issue_max_usd", b.PerIssueMaxUSD, problems)
	if b.RefusalCooldownSeconds < 0 {
		*problems = append(*problems, prefix+".refusal_cooldown_seconds must be greater than or equal to 0")
	}
	if strings.TrimSpace(b.PricingPath) == "" {
		*problems = append(*problems, prefix+".pricing_path is required")
	}
}

func (c *Codex) validate(problems *[]string) {
	if strings.TrimSpace(c.Command) == "" {
		*problems = append(*problems, "codex.command is required")
	}
	validatePositive("codex.turn_timeout_ms", c.TurnTimeoutMS, problems)
	validatePositive("codex.read_timeout_ms", c.ReadTimeoutMS, problems)
	if c.StallTimeoutMS < 0 {
		*problems = append(*problems, "codex.stall_timeout_ms must be greater than or equal to 0")
	}
}

func (s *Server) validate(problems *[]string) {
	if s.Port != nil && *s.Port < 0 {
		*problems = append(*problems, "server.port must be greater than or equal to 0")
	}
	if strings.TrimSpace(s.Host) == "" {
		*problems = append(*problems, "server.host is required")
	}
}

func (o *Observability) validate(problems *[]string) {
	validatePositive("observability.refresh_ms", o.RefreshMS, problems)
	validatePositive("observability.render_interval_ms", o.RenderIntervalMS, problems)
}

func (h *Hooks) validate(problems *[]string) {
	validatePositive("hooks.timeout_ms", h.TimeoutMS, problems)
}

func splitFrontmatter(raw []byte) ([]byte, []byte, error) {
	normalized := strings.ReplaceAll(strings.TrimPrefix(string(raw), "\ufeff"), "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return nil, nil, errors.New("missing YAML frontmatter")
	}

	body := normalized[len("---\n"):]
	if strings.HasPrefix(body, "---\n") {
		return []byte{}, []byte(body[len("---\n"):]), nil
	}
	if body == "---" {
		return []byte{}, []byte{}, nil
	}

	closeIndex := strings.Index(body, "\n---\n")
	if closeIndex >= 0 {
		return []byte(body[:closeIndex]), []byte(body[closeIndex+len("\n---\n"):]), nil
	}

	if strings.HasSuffix(body, "\n---") {
		return []byte(strings.TrimSuffix(body, "\n---")), []byte{}, nil
	}

	return nil, nil, errors.New("unterminated YAML frontmatter")
}

func documentRoot(doc *yaml.Node) (*yaml.Node, error) {
	if doc.Kind == 0 {
		return &yaml.Node{Kind: yaml.MappingNode}, nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return &yaml.Node{Kind: yaml.MappingNode}, nil
		}
		return doc.Content[0], nil
	}
	return nil, errors.New("workflow frontmatter must be a YAML document")
}

func normalizeTrackerIDFields(root *yaml.Node) {
	tracker := mappingValue(root, "tracker")
	if tracker == nil || tracker.Kind != yaml.MappingNode {
		return
	}

	normalizeScalarField(tracker, "github_app_id")
	normalizeScalarField(tracker, "github_app_installation_id")
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node.Kind != yaml.MappingNode {
		return nil
	}

	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}

	return nil
}

func normalizeScalarField(node *yaml.Node, key string) {
	value := mappingValue(node, key)
	if value == nil || value.Kind != yaml.ScalarNode || value.Tag == "!!null" {
		return
	}

	value.Tag = "!!str"
}

func decodeMapNode(node *yaml.Node) (map[string]any, error) {
	out := make(map[string]any, len(node.Content)/2)
	for i := 0; i < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		if keyNode.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("map keys must be scalars, got %s", yamlKindName(keyNode))
		}

		value, err := decodeAnyNode(node.Content[i+1])
		if err != nil {
			return nil, err
		}
		out[keyNode.Value] = value
	}

	return out, nil
}

func decodeAnyNode(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		return decodeScalarNode(node)
	case yaml.SequenceNode:
		out := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := decodeAnyNode(child)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	case yaml.MappingNode:
		return decodeMapNode(node)
	default:
		return nil, fmt.Errorf("unsupported YAML node %s", yamlKindName(node))
	}
}

func decodeScalarNode(node *yaml.Node) (any, error) {
	switch node.Tag {
	case "!!null":
		return nil, nil
	case "!!bool":
		return strconv.ParseBool(node.Value)
	case "!!int":
		value, err := strconv.ParseInt(strings.ReplaceAll(node.Value, "_", ""), 0, 64)
		if err != nil {
			return nil, err
		}
		return int(value), nil
	case "!!float":
		return strconv.ParseFloat(strings.ReplaceAll(node.Value, "_", ""), 64)
	default:
		return node.Value, nil
	}
}

func yamlKindName(node *yaml.Node) string {
	if node.Tag != "" {
		return node.Tag
	}

	switch node.Kind {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
}

func defaultBudget() Budget {
	return Budget{
		PerDayMaxUSD:           50.0,
		PerIssueMaxUSD:         5.0,
		RefusalCooldownSeconds: 3600,
		PricingPath:            "priv/pricing/models.yaml",
	}
}

func defaultLessons() Lessons {
	return Lessons{
		Path:                ".detent/lessons.md",
		MaxEntries:          50,
		RecallN:             10,
		PostmortemMaxTokens: 1024,
	}
}

func defaultSkills() Skills {
	return Skills{
		Enabled:           true,
		Path:              ".detent/skills",
		MaxSkillsInPrompt: 50,
	}
}

func defaultPriorityMap() map[string]any {
	return map[string]any{
		"Urgent":      1,
		"High":        2,
		"Medium":      3,
		"Low":         4,
		"No priority": nil,
	}
}

func normalizeStateLimits(limits map[string]int) map[string]int {
	if limits == nil {
		return map[string]int{}
	}

	normalized := make(map[string]int, len(limits))
	for state, limit := range limits {
		normalized[normalizeIssueState(state)] = limit
	}
	return normalized
}

func normalizeStateList(states []string) []string {
	if states == nil {
		return []string{}
	}

	normalized := make([]string, 0, len(states))
	for _, state := range states {
		normalized = append(normalized, normalizeIssueState(state))
	}
	return normalized
}

func normalizeIssueState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

func normalizeLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

func normalizeLabels(labels []string) []string {
	if labels == nil {
		return []string{}
	}

	normalized := make([]string, 0, len(labels))
	seen := map[string]struct{}{}
	for _, label := range labels {
		candidate := normalizeLabel(label)
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}
		normalized = append(normalized, candidate)
	}
	return normalized
}

func validateRequired(field string, value string, suffix string, problems *[]string) {
	if strings.TrimSpace(value) == "" {
		*problems = append(*problems, field+" is required"+suffix)
	}
}

func validatePositive(field string, value int, problems *[]string) {
	if value <= 0 {
		*problems = append(*problems, field+" must be greater than 0")
	}
}

func validatePollingInterval(value int, problems *[]string) {
	if value <= 0 {
		*problems = append(*problems, "polling.interval_ms must be greater than 0")
		return
	}
	if value < MinPollingIntervalMS {
		*problems = append(*problems, fmt.Sprintf("polling.interval_ms must be at least %d", MinPollingIntervalMS))
	}
}

func validatePositiveFloat(field string, value float64, problems *[]string) {
	if value <= 0 {
		*problems = append(*problems, field+" must be greater than 0")
	}
}

func validateStateList(field string, states []string, problems *[]string) {
	seen := make(map[string]struct{}, len(states))
	for _, state := range states {
		if strings.TrimSpace(state) == "" {
			*problems = append(*problems, field+" state names must not be blank")
			return
		}
		if _, ok := seen[state]; ok {
			*problems = append(*problems, field+" state names must be unique")
			return
		}
		seen[state] = struct{}{}
	}
}

func validateStateLimits(field string, limits map[string]int, problems *[]string) {
	for state, limit := range limits {
		if strings.TrimSpace(state) == "" {
			*problems = append(*problems, field+" state names must not be blank")
			return
		}
		if limit <= 0 {
			*problems = append(*problems, field+" limits must be positive integers")
			return
		}
	}
}

func validateStateMap(field string, value StringOrMap, problems *[]string) {
	if !value.IsMap {
		return
	}
	for state, mapped := range value.Map {
		if strings.TrimSpace(state) == "" {
			*problems = append(*problems, field+" state names must not be blank")
			return
		}
		if _, ok := mapped.(string); !ok {
			*problems = append(*problems, field+" values must be strings")
			return
		}
	}
}

func validatePriorityMap(field string, value StringOrMap, problems *[]string) {
	if !value.IsMap {
		return
	}
	if len(value.Map) == 0 {
		*problems = append(*problems, field+" must not be empty")
		return
	}

	for name, rank := range value.Map {
		if strings.TrimSpace(name) == "" {
			*problems = append(*problems, field+" option names must not be blank")
		}
		if !validPriorityRank(rank) {
			*problems = append(*problems, field+" ranks must be integers 1 through 4 or null")
		}
	}
}

func validPriorityRank(value any) bool {
	switch rank := value.(type) {
	case nil:
		return true
	case int:
		return rank >= 1 && rank <= 4
	default:
		return false
	}
}

func validateWorkspaceRelativePath(field string, path string, problems *[]string) {
	if !pathsafe.IsWorkspaceRelative(path) {
		*problems = append(*problems, field+" must be a relative path inside the workspace")
	}
}
