package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/symphony/internal/connector"
)

const (
	TrackerGitHub = "github"
	TrackerLinear = "linear"
	TrackerMemory = "memory"

	defaultLinearEndpoint = "https://api.linear.app/graphql"
	defaultGitHubEndpoint = "https://api.github.com/graphql"
)

var windowsAbsPathPattern = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

type Workflow struct {
	Config Config
	Prompt string
}

type Config struct {
	Tracker       Tracker       `yaml:"tracker"`
	Polling       Polling       `yaml:"polling"`
	Workspace     Workspace     `yaml:"workspace"`
	Worker        Worker        `yaml:"worker"`
	Agent         Agent         `yaml:"agent"`
	Codex         Codex         `yaml:"codex"`
	Server        Server        `yaml:"server"`
	Observability Observability `yaml:"observability"`
	Budget        Budget        `yaml:"budget"`
	Hooks         Hooks         `yaml:"hooks"`
}

type Tracker struct {
	Kind                    string            `yaml:"kind"`
	Endpoint                string            `yaml:"endpoint"`
	APIKey                  string            `yaml:"api_key"`
	GitHubAppID             string            `yaml:"github_app_id"`
	GitHubAppPrivateKey     string            `yaml:"github_app_private_key"`
	GitHubAppPrivateKeyPath string            `yaml:"github_app_private_key_path"`
	GitHubAppInstallationID string            `yaml:"github_app_installation_id"`
	ProjectSlug             string            `yaml:"project_slug"`
	Assignee                string            `yaml:"assignee"`
	ActiveStates            []string          `yaml:"active_states"`
	ObservedStates          []string          `yaml:"observed_states"`
	TerminalStates          []string          `yaml:"terminal_states"`
	StateMap                StringOrMap       `yaml:"state_map"`
	PriorityMap             StringOrMap       `yaml:"priority_map"`
	AutoProvision           bool              `yaml:"auto_provision"`
	Issues                  []connector.Issue `yaml:"issues"`
}

type Polling struct {
	IntervalMS int `yaml:"interval_ms"`
}

type Workspace struct {
	Root       string `yaml:"root"`
	AutoBranch bool   `yaml:"auto_branch"`
}

type Worker struct {
	SSHHosts                   []string `yaml:"ssh_hosts"`
	MaxConcurrentAgentsPerHost *int     `yaml:"max_concurrent_agents_per_host"`
}

type Agent struct {
	MaxConcurrentAgents        int            `yaml:"max_concurrent_agents"`
	MaxTurns                   int            `yaml:"max_turns"`
	MaxRetryBackoffMS          int            `yaml:"max_retry_backoff_ms"`
	MaxConcurrentAgentsByState map[string]int `yaml:"max_concurrent_agents_by_state"`
	DispatchPriorityByState    []string       `yaml:"dispatch_priority_by_state"`
	AutoPromote                AutoPromote    `yaml:"auto_promote"`
	Budget                     Budget         `yaml:"budget"`
	Lessons                    Lessons        `yaml:"lessons"`
	Skills                     Skills         `yaml:"skills"`
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
	ApprovalPolicy    StringOrMap    `yaml:"approval_policy"`
	ThreadSandbox     string         `yaml:"thread_sandbox"`
	TurnSandboxPolicy map[string]any `yaml:"turn_sandbox_policy"`
	TurnTimeoutMS     int            `yaml:"turn_timeout_ms"`
	ReadTimeoutMS     int            `yaml:"read_timeout_ms"`
	StallTimeoutMS    int            `yaml:"stall_timeout_ms"`
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
	AfterCreate  string `yaml:"after_create"`
	BeforeRun    string `yaml:"before_run"`
	AfterRun     string `yaml:"after_run"`
	BeforeRemove string `yaml:"before_remove"`
	TimeoutMS    int    `yaml:"timeout_ms"`
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
			Endpoint:       defaultLinearEndpoint,
			ActiveStates:   []string{"Todo", "In Progress"},
			ObservedStates: []string{"Backlog", "Human Review", "Blocked"},
			TerminalStates: []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"},
			StateMap:       MapValue(map[string]any{}),
			PriorityMap:    MapValue(defaultPriorityMap()),
			AutoProvision:  true,
		},
		Polling: Polling{
			IntervalMS: 30000,
		},
		Workspace: Workspace{
			Root:       filepath.Join(os.TempDir(), "symphony_workspaces"),
			AutoBranch: true,
		},
		Worker: Worker{
			SSHHosts: []string{},
		},
		Agent: Agent{
			MaxConcurrentAgents:        10,
			MaxTurns:                   20,
			MaxRetryBackoffMS:          300000,
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

func (c Config) Validate() error {
	var problems []string

	c.validateTracker(&problems)
	validatePositive("polling.interval_ms", c.Polling.IntervalMS, &problems)
	if c.Worker.MaxConcurrentAgentsPerHost != nil {
		validatePositive("worker.max_concurrent_agents_per_host", *c.Worker.MaxConcurrentAgentsPerHost, &problems)
	}
	c.Agent.validate("agent", &problems)
	c.Codex.validate(&problems)
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
	c.Tracker.Kind = strings.ToLower(strings.TrimSpace(c.Tracker.Kind))
	if c.Tracker.Kind == TrackerGitHub && c.Tracker.Endpoint == defaultLinearEndpoint {
		c.Tracker.Endpoint = defaultGitHubEndpoint
	}

	c.Agent.MaxConcurrentAgentsByState = normalizeStateLimits(c.Agent.MaxConcurrentAgentsByState)
	c.Agent.DispatchPriorityByState = normalizeStateList(c.Agent.DispatchPriorityByState)
	c.Agent.AutoPromote.OptoutLabel = normalizeLabel(c.Agent.AutoPromote.OptoutLabel)
	c.Agent.AutoPromote.AllowedIssueLabels = normalizeLabels(c.Agent.AutoPromote.AllowedIssueLabels)
}

func (c Config) validateTracker(problems *[]string) {
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
}

func (t Tracker) validateGitHubAuth(problems *[]string) {
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

func (t Tracker) hasGitHubAppCredentials() bool {
	return strings.TrimSpace(t.GitHubAppID) != "" &&
		strings.TrimSpace(t.GitHubAppInstallationID) != "" &&
		(strings.TrimSpace(t.GitHubAppPrivateKey) != "" || strings.TrimSpace(t.GitHubAppPrivateKeyPath) != "")
}

func (a Agent) validate(prefix string, problems *[]string) {
	validatePositive(prefix+".max_concurrent_agents", a.MaxConcurrentAgents, problems)
	validatePositive(prefix+".max_turns", a.MaxTurns, problems)
	validatePositive(prefix+".max_retry_backoff_ms", a.MaxRetryBackoffMS, problems)
	validateStateLimits(prefix+".max_concurrent_agents_by_state", a.MaxConcurrentAgentsByState, problems)
	validateStateList(prefix+".dispatch_priority_by_state", a.DispatchPriorityByState, problems)
	a.AutoPromote.validate(prefix+".auto_promote", problems)
	a.Budget.validate(prefix+".budget", problems)
	a.Lessons.validate(prefix+".lessons", problems)
	a.Skills.validate(prefix+".skills", problems)
}

func (a AutoPromote) validate(prefix string, problems *[]string) {
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

func (l Lessons) validate(prefix string, problems *[]string) {
	validateWorkspaceRelativePath(prefix+".path", l.Path, problems)
	validatePositive(prefix+".max_entries", l.MaxEntries, problems)
	if l.RecallN < 0 {
		*problems = append(*problems, prefix+".recall_n must be greater than or equal to 0")
	}
	validatePositive(prefix+".postmortem_max_tokens", l.PostmortemMaxTokens, problems)
}

func (s Skills) validate(prefix string, problems *[]string) {
	validateWorkspaceRelativePath(prefix+".path", s.Path, problems)
	validatePositive(prefix+".max_skills_in_prompt", s.MaxSkillsInPrompt, problems)
}

func (b Budget) validate(prefix string, problems *[]string) {
	validatePositiveFloat(prefix+".per_day_max_usd", b.PerDayMaxUSD, problems)
	validatePositiveFloat(prefix+".per_issue_max_usd", b.PerIssueMaxUSD, problems)
	if b.RefusalCooldownSeconds < 0 {
		*problems = append(*problems, prefix+".refusal_cooldown_seconds must be greater than or equal to 0")
	}
	if strings.TrimSpace(b.PricingPath) == "" {
		*problems = append(*problems, prefix+".pricing_path is required")
	}
}

func (c Codex) validate(problems *[]string) {
	if strings.TrimSpace(c.Command) == "" {
		*problems = append(*problems, "codex.command is required")
	}
	validatePositive("codex.turn_timeout_ms", c.TurnTimeoutMS, problems)
	validatePositive("codex.read_timeout_ms", c.ReadTimeoutMS, problems)
	if c.StallTimeoutMS < 0 {
		*problems = append(*problems, "codex.stall_timeout_ms must be greater than or equal to 0")
	}
}

func (s Server) validate(problems *[]string) {
	if s.Port != nil && *s.Port < 0 {
		*problems = append(*problems, "server.port must be greater than or equal to 0")
	}
	if strings.TrimSpace(s.Host) == "" {
		*problems = append(*problems, "server.host is required")
	}
}

func (o Observability) validate(problems *[]string) {
	validatePositive("observability.refresh_ms", o.RefreshMS, problems)
	validatePositive("observability.render_interval_ms", o.RenderIntervalMS, problems)
}

func (h Hooks) validate(problems *[]string) {
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
		Path:                ".symphony/lessons.md",
		MaxEntries:          50,
		RecallN:             10,
		PostmortemMaxTokens: 1024,
	}
}

func defaultSkills() Skills {
	return Skills{
		Enabled:           true,
		Path:              ".symphony/skills",
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
	trimmed := strings.TrimSpace(path)
	if trimmed == "" || strings.HasPrefix(trimmed, "~") || filepath.IsAbs(trimmed) ||
		strings.HasPrefix(trimmed, `\`) || windowsAbsPathPattern.MatchString(trimmed) ||
		pathEscapesWorkspace(trimmed) {
		*problems = append(*problems, field+" must be a relative path inside the workspace")
	}
}

func pathEscapesWorkspace(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}
