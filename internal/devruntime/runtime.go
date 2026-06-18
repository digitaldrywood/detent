package devruntime

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
)

const (
	TrackerMemory = "memory"
	DemoKanban    = "kanban"

	defaultProjectID    = "dogfood"
	defaultInstanceName = "detent-dev-runtime"
	defaultHost         = "127.0.0.1"
	defaultDBPath       = ":memory:"
	liveDogfoodPort     = 4000
)

var (
	ErrUnsupportedTracker  = errors.New("unsupported isolated runtime tracker")
	ErrLiveDatabase        = errors.New("isolated runtime refuses the live Detent database")
	ErrLivePort            = errors.New("isolated runtime refuses the live Detent port")
	ErrInvalidPort         = errors.New("isolated runtime port must be greater than or equal to 0")
	ErrUnsupportedDemo     = errors.New("unsupported isolated runtime demo")
	ErrDemoFixtureConflict = errors.New("isolated runtime demo cannot be combined with a fixture")
)

type Config struct {
	Home                string
	DBPath              string
	TrackerMode         string
	FixturePath         string
	Demo                string
	Port                int
	AllowLiveDatabase   bool
	AllowProductionPort bool
}

type Runtime struct {
	Home          string
	ConfigPath    string
	WorkflowPath  string
	WorkspaceRoot string
	SourceRoot    string
	DBPath        string
	DBMode        string
	TrackerMode   string
	FixturePath   string
	Demo          string
	Port          int
	Global        globalconfig.Config
	Issues        []connector.Issue
}

func Build(cfg Config) (Runtime, error) {
	if cfg.Port < 0 {
		return Runtime{}, fmt.Errorf("%w: %d", ErrInvalidPort, cfg.Port)
	}
	if cfg.Port == liveDogfoodPort && !cfg.AllowProductionPort {
		return Runtime{}, fmt.Errorf("%w: port %d is reserved for the live dogfood instance; use --port 0", ErrLivePort, liveDogfoodPort)
	}

	trackerMode := strings.ToLower(strings.TrimSpace(cfg.TrackerMode))
	if trackerMode == "" {
		trackerMode = TrackerMemory
	}
	if trackerMode != TrackerMemory {
		return Runtime{}, fmt.Errorf("%w: %s", ErrUnsupportedTracker, trackerMode)
	}
	demo, err := runtimeDemo(cfg.Demo)
	if err != nil {
		return Runtime{}, err
	}

	home, err := runtimeHome(cfg.Home)
	if err != nil {
		return Runtime{}, err
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return Runtime{}, fmt.Errorf("create isolated runtime home: %w", err)
	}

	dbPath, err := runtimeDBPath(home, cfg.DBPath, cfg.AllowLiveDatabase)
	if err != nil {
		return Runtime{}, err
	}
	workspaceRoot := filepath.Join(home, "workspaces")
	sourceRoot := filepath.Join(home, "source")
	for _, dir := range []string{workspaceRoot, sourceRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Runtime{}, fmt.Errorf("create isolated runtime directory %s: %w", dir, err)
		}
	}

	issues, fixturePath, err := runtimeIssues(cfg.FixturePath, demo)
	if err != nil {
		return Runtime{}, err
	}

	workflowPath := filepath.Join(home, "WORKFLOW.md")
	if err := writeWorkflow(workflowPath, workflowInput{
		TrackerMode:   trackerMode,
		WorkspaceRoot: workspaceRoot,
		SourceRoot:    sourceRoot,
		Port:          cfg.Port,
		Issues:        issues,
		Demo:          demo,
	}); err != nil {
		return Runtime{}, err
	}

	configPath := filepath.Join(home, "global.yaml")
	global := globalConfig(configPath, workflowPath, sourceRoot, cfg.Port)
	if err := globalconfig.Write(configPath, global); err != nil {
		return Runtime{}, fmt.Errorf("write isolated global config: %w", err)
	}
	global.Path = configPath

	return Runtime{
		Home:          home,
		ConfigPath:    configPath,
		WorkflowPath:  workflowPath,
		WorkspaceRoot: workspaceRoot,
		SourceRoot:    sourceRoot,
		DBPath:        dbPath,
		DBMode:        dbMode(dbPath),
		TrackerMode:   trackerMode,
		FixturePath:   fixturePath,
		Demo:          demo,
		Port:          cfg.Port,
		Global:        global,
		Issues:        append([]connector.Issue(nil), issues...),
	}, nil
}

func runtimeDemo(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", DemoKanban:
		return value, nil
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedDemo, value)
	}
}

func runtimeHome(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		home, err := os.MkdirTemp("", "detent-dev-runtime-*")
		if err != nil {
			return "", fmt.Errorf("create isolated runtime home: %w", err)
		}
		return home, nil
	}
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("resolve isolated runtime home: %w", err)
	}
	return absolute, nil
}

func runtimeDBPath(home string, path string, allowLive bool) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = defaultDBPath
	}
	if path != defaultDBPath && !strings.HasPrefix(path, "file:") {
		expanded, err := expandHome(path)
		if err != nil {
			return "", err
		}
		path = expanded
		if !filepath.IsAbs(path) {
			path = filepath.Join(home, path)
		}
	}
	guardPath := path
	if uriPath, ok := sqliteURIFilePath(path); ok {
		guardPath = uriPath
	}
	if !allowLive && samePath(guardPath, liveDatabasePath()) {
		return "", fmt.Errorf("%w: %s", ErrLiveDatabase, path)
	}
	return path, nil
}

func sqliteURIFilePath(path string) (string, bool) {
	path = strings.TrimSpace(path)
	if !strings.HasPrefix(strings.ToLower(path), "file:") {
		return "", false
	}
	parsed, err := url.Parse(path)
	if err != nil {
		return "", false
	}
	if strings.EqualFold(parsed.Query().Get("mode"), "memory") {
		return "", false
	}

	candidate := parsed.Path
	if candidate == "" {
		candidate = parsed.Opaque
	}
	candidate = strings.TrimSpace(candidate)
	if candidate == "" || strings.HasPrefix(candidate, ":memory:") {
		return "", false
	}
	unescaped, err := url.PathUnescape(candidate)
	if err == nil {
		candidate = unescaped
	}
	return filepath.FromSlash(candidate), true
}

func liveDatabasePath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".detent", "detent.db")
}

func samePath(left string, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}
	leftAbs, leftErr := filepath.Abs(left)
	rightAbs, rightErr := filepath.Abs(right)
	if leftErr == nil {
		left = leftAbs
	}
	if rightErr == nil {
		right = rightAbs
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func expandHome(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

type fixtureFile struct {
	Issues []connector.Issue `yaml:"issues"`
}

func runtimeIssues(path string, demo string) ([]connector.Issue, string, error) {
	path = strings.TrimSpace(path)
	if demo != "" && path != "" {
		return nil, "", ErrDemoFixtureConflict
	}
	if demo == DemoKanban {
		return kanbanDemoIssues(), "", nil
	}
	if path == "" {
		return defaultIssues(), "", nil
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read isolated runtime fixture: %w", err)
	}
	var fixture fixtureFile
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		return nil, "", fmt.Errorf("decode isolated runtime fixture: %w", err)
	}
	if len(fixture.Issues) == 0 {
		return nil, "", fmt.Errorf("decode isolated runtime fixture: issues must not be empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve isolated runtime fixture: %w", err)
	}
	return fixture.Issues, absolute, nil
}

func kanbanDemoIssues() []connector.Issue {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	at := func(hoursAgo int) *time.Time {
		value := now.Add(-time.Duration(hoursAgo) * time.Hour)
		return &value
	}
	fields := func(state string, owner string) map[string]string {
		return map[string]string{
			"Status": state,
			"Owner":  owner,
		}
	}
	return []connector.Issue{
		{
			ID:             "kanban-demo-backlog",
			Identifier:     "digitaldrywood/detent#9511",
			Title:          "Kanban demo backlog intake",
			Description:    "Issue-only card for evaluating Backlog to Todo moves.",
			State:          "Backlog",
			URL:            "https://github.test/digitaldrywood/detent/issues/9511",
			Labels:         []string{"enhancement", "operator-demo"},
			AssigneeID:     "operator-a",
			Assignees:      []string{"operator-a"},
			Fields:         fields("Backlog", "operations"),
			UpdatedAt:      at(72),
			StageUpdatedAt: at(72),
		},
		{
			ID:             "kanban-demo-todo",
			Identifier:     "digitaldrywood/detent#9512",
			Title:          "Kanban demo todo ready card",
			Description:    "Issue-only Todo card with labels and assignees.",
			State:          "Todo",
			URL:            "https://github.test/digitaldrywood/detent/issues/9512",
			Labels:         []string{"ui", "good-first-demo"},
			AssigneeID:     "operator-b",
			Assignees:      []string{"operator-b", "operator-c"},
			Fields:         fields("Todo", "product"),
			UpdatedAt:      at(8),
			StageUpdatedAt: at(8),
		},
		{
			ID:             "kanban-demo-progress",
			Identifier:     "digitaldrywood/detent#9513",
			Title:          "Kanban demo implementation in progress",
			State:          "In Progress",
			URL:            "https://github.test/digitaldrywood/detent/issues/9513",
			Labels:         []string{"enhancement", "backend"},
			AssigneeID:     "agent-one",
			Assignees:      []string{"agent-one"},
			Fields:         fields("In Progress", "agents"),
			UpdatedAt:      at(3),
			StageUpdatedAt: at(3),
			PullRequest: &connector.PullRequest{
				Number:           9513,
				URL:              "https://github.test/digitaldrywood/detent/pull/9513",
				BranchName:       "detent/kanban-demo-progress",
				State:            "OPEN",
				CIStatus:         "pending",
				RunningChecks:    []string{"make check"},
				CheckRunCount:    1,
				CodexReviewState: "CLEAN",
			},
		},
		{
			ID:             "kanban-demo-blocked",
			Identifier:     "digitaldrywood/detent#9514",
			Title:          "Kanban demo blocked dependency",
			State:          "Blocked",
			URL:            "https://github.test/digitaldrywood/detent/issues/9514",
			Labels:         []string{"blocked", "needs-dependency"},
			AssigneeID:     "operator-d",
			Assignees:      []string{"operator-d"},
			BlockedBy:      []connector.BlockedRef{{ID: "kanban-demo-todo", Identifier: "digitaldrywood/detent#9512", State: "Todo"}},
			BlockerReason:  "Depends on the Todo demo card before work can continue.",
			Fields:         fields("Blocked", "operations"),
			UpdatedAt:      at(28),
			StageUpdatedAt: at(28),
		},
		{
			ID:             "kanban-demo-review",
			Identifier:     "digitaldrywood/detent#9515",
			Title:          "Kanban demo human review candidate",
			State:          "Human Review",
			URL:            "https://github.test/digitaldrywood/detent/issues/9515",
			Labels:         []string{"enhancement", "requires-human-review"},
			AssigneeID:     "reviewer-a",
			Assignees:      []string{"reviewer-a"},
			Fields:         fields("Human Review", "review"),
			UpdatedAt:      at(2),
			StageUpdatedAt: at(2),
			PullRequest: &connector.PullRequest{
				Number:                 9515,
				URL:                    "https://github.test/digitaldrywood/detent/pull/9515",
				BranchName:             "detent/kanban-demo-review",
				State:                  "OPEN",
				MergeableState:         "clean",
				CIStatus:               "success",
				CheckRunCount:          4,
				StatusContextCount:     1,
				CodexReviewState:       "CLEAN",
				CodexReviewSubmittedAt: at(1),
			},
		},
		{
			ID:             "kanban-demo-rework",
			Identifier:     "digitaldrywood/detent#9516",
			Title:          "Kanban demo rework finding",
			State:          "Rework",
			URL:            "https://github.test/digitaldrywood/detent/issues/9516",
			Labels:         []string{"bug", "codex-review:p1"},
			AssigneeID:     "agent-two",
			Assignees:      []string{"agent-two"},
			Fields:         fields("Rework", "agents"),
			UpdatedAt:      at(10),
			StageUpdatedAt: at(10),
			PullRequest: &connector.PullRequest{
				Number:           9516,
				URL:              "https://github.test/digitaldrywood/detent/pull/9516",
				BranchName:       "detent/kanban-demo-rework",
				State:            "OPEN",
				MergeableState:   "dirty",
				CIStatus:         "failure",
				CheckRunCount:    3,
				CodexReviewState: "P1",
				CodexReviewFindings: []connector.PullRequestFinding{{
					Body: "P1 demo finding for operator review",
					URL:  "https://github.test/digitaldrywood/detent/pull/9516#discussion_r1",
					Path: "internal/demo.go",
					Line: 37,
				}},
			},
		},
		{
			ID:             "kanban-demo-merging",
			Identifier:     "digitaldrywood/detent#9517",
			Title:          "Kanban demo merging train",
			State:          "Merging",
			URL:            "https://github.test/digitaldrywood/detent/issues/9517",
			Labels:         []string{"release-readiness", "merge-train"},
			AssigneeID:     "merger-a",
			Assignees:      []string{"merger-a"},
			Fields:         fields("Merging", "release"),
			UpdatedAt:      at(1),
			StageUpdatedAt: at(1),
			PullRequest: &connector.PullRequest{
				Number:            9517,
				URL:               "https://github.test/digitaldrywood/detent/pull/9517",
				BranchName:        "detent/kanban-demo-merging",
				State:             "OPEN",
				MergeableState:    "clean",
				CIStatus:          "pending",
				CIDurationSeconds: 780,
				CheckRunCount:     5,
				RunningChecks:     []string{"release-smoke"},
				CodexReviewState:  "CLEAN",
				SlowChecks:        []connector.PullRequestCheck{{Name: "go test -race", Status: "completed", Conclusion: "success", DurationSeconds: 620}},
			},
		},
		{
			ID:             "kanban-demo-done",
			Identifier:     "digitaldrywood/detent#9518",
			Title:          "Kanban demo completed PR",
			State:          "Done",
			URL:            "https://github.test/digitaldrywood/detent/issues/9518",
			Closed:         true,
			ClosedReason:   "completed",
			Labels:         []string{"enhancement", "shipped"},
			AssigneeID:     "operator-e",
			Assignees:      []string{"operator-e"},
			Fields:         fields("Done", "release"),
			UpdatedAt:      at(24),
			StageUpdatedAt: at(24),
			PullRequest: &connector.PullRequest{
				Number:           9518,
				URL:              "https://github.test/digitaldrywood/detent/pull/9518",
				BranchName:       "detent/kanban-demo-done",
				State:            "MERGED",
				MergeableState:   "clean",
				CIStatus:         "success",
				CodexReviewState: "CLEAN",
			},
		},
		{
			ID:             "kanban-demo-cancelled",
			Identifier:     "digitaldrywood/detent#9519",
			Title:          "Kanban demo cancelled request",
			State:          "Cancelled",
			URL:            "https://github.test/digitaldrywood/detent/issues/9519",
			Closed:         true,
			ClosedReason:   "not_planned",
			Labels:         []string{"cancelled", "operator-demo"},
			AssigneeID:     "operator-f",
			Assignees:      []string{"operator-f"},
			Fields:         fields("Cancelled", "operations"),
			UpdatedAt:      at(48),
			StageUpdatedAt: at(48),
		},
	}
}

func defaultIssues() []connector.Issue {
	reviewedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	updatedAt := reviewedAt
	p1Line := 42
	return []connector.Issue{
		{
			ID:               "mock-issue-autopromote",
			Identifier:       "digitaldrywood/detent#9001",
			Title:            "Dogfood auto-promote fixture",
			Description:      "Safe fixture issue for isolated runtime auto-promote validation.",
			State:            "Human Review",
			URL:              "https://github.test/digitaldrywood/detent/issues/9001",
			Labels:           []string{"enhancement", "stage:s7"},
			AssignedToWorker: true,
			UpdatedAt:        &updatedAt,
			StageUpdatedAt:   &updatedAt,
			PullRequest: &connector.PullRequest{
				Number:                 9001,
				URL:                    "https://github.test/digitaldrywood/detent/pull/9001",
				BranchName:             "detent/mock-issue-autopromote",
				State:                  "OPEN",
				CIStatus:               "success",
				CodexReviewState:       "COMMENTED",
				CodexReviewSubmittedAt: &reviewedAt,
			},
		},
		{
			ID:               "mock-issue-review-finding",
			Identifier:       "digitaldrywood/detent#9002",
			Title:            "Dogfood review finding fixture",
			State:            "Human Review",
			URL:              "https://github.test/digitaldrywood/detent/issues/9002",
			Labels:           []string{"bug", "stage:s7"},
			AssignedToWorker: true,
			UpdatedAt:        &updatedAt,
			StageUpdatedAt:   &updatedAt,
			PullRequest: &connector.PullRequest{
				Number:                 9002,
				URL:                    "https://github.test/digitaldrywood/detent/pull/9002",
				BranchName:             "detent/mock-issue-review-finding",
				State:                  "OPEN",
				CIStatus:               "success",
				CodexReviewState:       "P1",
				CodexReviewSubmittedAt: &reviewedAt,
				CodexReviewFindings: []connector.PullRequestFinding{{
					Body: "P1 mock finding",
					URL:  "https://github.test/digitaldrywood/detent/pull/9002#discussion_r1",
					Path: "internal/mock.go",
					Line: p1Line,
				}},
			},
		},
		{
			ID:               "mock-issue-resume",
			Identifier:       "digitaldrywood/detent#9003",
			Title:            "Dogfood restart resume fixture",
			State:            "In Progress",
			URL:              "https://github.test/digitaldrywood/detent/issues/9003",
			Labels:           []string{"enhancement", "stage:s7"},
			AssignedToWorker: true,
			UpdatedAt:        &updatedAt,
			StageUpdatedAt:   &updatedAt,
			PullRequest: &connector.PullRequest{
				Number:     9003,
				URL:        "https://github.test/digitaldrywood/detent/pull/9003",
				BranchName: "detent/mock-issue-resume",
				State:      "OPEN",
				CIStatus:   "pending",
			},
		},
		{
			ID:               "mock-issue-merged",
			Identifier:       "digitaldrywood/detent#9004",
			Title:            "Dogfood merged PR fixture",
			State:            "Done",
			URL:              "https://github.test/digitaldrywood/detent/issues/9004",
			Labels:           []string{"enhancement", "stage:s7"},
			AssignedToWorker: true,
			Closed:           true,
			UpdatedAt:        &updatedAt,
			StageUpdatedAt:   &updatedAt,
			PullRequest: &connector.PullRequest{
				Number:     9004,
				URL:        "https://github.test/digitaldrywood/detent/pull/9004",
				BranchName: "detent/mock-issue-merged",
				State:      "MERGED",
				CIStatus:   "success",
			},
		},
	}
}

type workflowInput struct {
	TrackerMode   string
	WorkspaceRoot string
	SourceRoot    string
	Port          int
	Issues        []connector.Issue
	Demo          string
}

type workflowFrontmatter struct {
	Tracker   workflowTracker   `yaml:"tracker"`
	Polling   workflowPolling   `yaml:"polling"`
	Workspace workflowWorkspace `yaml:"workspace"`
	Agent     workflowAgent     `yaml:"agent"`
	Gate      workflowGate      `yaml:"gate"`
	Server    workflowServer    `yaml:"server"`
}

type workflowTracker struct {
	Kind           string            `yaml:"kind"`
	ActiveStates   []string          `yaml:"active_states"`
	ObservedStates []string          `yaml:"observed_states"`
	TerminalStates []string          `yaml:"terminal_states"`
	StateMap       map[string]string `yaml:"state_map"`
	Issues         []connector.Issue `yaml:"issues"`
}

type workflowPolling struct {
	IntervalMS int `yaml:"interval_ms"`
}

type workflowWorkspace struct {
	Root       string `yaml:"root"`
	SourceRoot string `yaml:"source_root"`
	AutoBranch bool   `yaml:"auto_branch"`
}

type workflowAgent struct {
	MaxConcurrentAgents        int                 `yaml:"max_concurrent_agents"`
	MaxConcurrentAgentsByState map[string]int      `yaml:"max_concurrent_agents_by_state"`
	AutoPromote                workflowAutoPromote `yaml:"auto_promote"`
}

type workflowAutoPromote struct {
	Enabled      bool   `yaml:"enabled"`
	QuietSeconds int    `yaml:"quiet_seconds"`
	OptoutLabel  string `yaml:"optout_label"`
}

type workflowGate struct {
	Kind                   string `yaml:"kind"`
	Run                    string `yaml:"run"`
	RequireAutomatedReview bool   `yaml:"require_automated_review"`
	CIFailureAction        string `yaml:"ci_failure_action"`
}

type workflowServer struct {
	Host   string          `yaml:"host"`
	Port   *int            `yaml:"port"`
	Kanban *workflowKanban `yaml:"kanban,omitempty"`
}

type workflowKanban struct {
	Mode               string              `yaml:"mode,omitempty"`
	AllowedTransitions map[string][]string `yaml:"allowed_transitions,omitempty"`
}

func writeWorkflow(path string, input workflowInput) error {
	port := input.Port
	activeStates := []string{"Todo", "In Progress", "Rework"}
	observedStates := []string{"Backlog", "Human Review", "Blocked", "Merging", "Done"}
	terminalStates := []string{"Done", "Cancelled", "Canceled"}
	autoPromote := workflowAutoPromote{
		Enabled:      true,
		QuietSeconds: 0,
		OptoutLabel:  "requires-human-review",
	}
	var kanban *workflowKanban
	body := "Isolated mock Detent runtime for dogfood-safe e2e validation.\n"
	if input.Demo == DemoKanban {
		activeStates = []string{"Cancelled"}
		observedStates = []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Rework", "Merging", "Done", "Cancelled"}
		autoPromote.Enabled = false
		kanban = &workflowKanban{
			Mode:               "integration",
			AllowedTransitions: kanbanDemoAllowedTransitions(),
		}
		body = "Isolated Kanban demo runtime for dogfood-safe board evaluation.\n"
	}
	frontmatter := workflowFrontmatter{
		Tracker: workflowTracker{
			Kind:           input.TrackerMode,
			ActiveStates:   activeStates,
			ObservedStates: observedStates,
			TerminalStates: terminalStates,
			StateMap:       map[string]string{},
			Issues:         append([]connector.Issue(nil), input.Issues...),
		},
		Polling: workflowPolling{IntervalMS: 60000},
		Workspace: workflowWorkspace{
			Root:       input.WorkspaceRoot,
			SourceRoot: input.SourceRoot,
			AutoBranch: true,
		},
		Agent: workflowAgent{
			MaxConcurrentAgents:        2,
			MaxConcurrentAgentsByState: map[string]int{"Merging": 1},
			AutoPromote:                autoPromote,
		},
		Gate: workflowGate{
			Kind:                   "command",
			Run:                    "true",
			RequireAutomatedReview: true,
			CIFailureAction:        "skip",
		},
		Server: workflowServer{
			Host:   defaultHost,
			Port:   &port,
			Kanban: kanban,
		},
	}
	raw, err := yaml.Marshal(frontmatter)
	if err != nil {
		return fmt.Errorf("marshal isolated workflow: %w", err)
	}
	content := "---\n" + string(raw) + "---\n" + body
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write isolated workflow: %w", err)
	}
	return nil
}

func kanbanDemoAllowedTransitions() map[string][]string {
	return map[string][]string{
		"Backlog":      {"Todo", "Cancelled"},
		"Todo":         {"In Progress", "Blocked", "Cancelled"},
		"In Progress":  {"Blocked", "Human Review", "Rework", "Cancelled"},
		"Blocked":      {"Todo", "In Progress", "Rework", "Cancelled"},
		"Human Review": {"Rework", "Merging", "Blocked", "Cancelled"},
		"Rework":       {"In Progress", "Human Review", "Blocked", "Cancelled"},
		"Merging":      {"Done", "Blocked", "Rework", "Cancelled"},
		"Done":         {},
		"Cancelled":    {"Backlog"},
		"Canceled":     {},
	}
}

func globalConfig(path string, workflowPath string, sourceRoot string, port int) globalconfig.Config {
	return globalconfig.Config{
		Path:         path,
		APIVersion:   globalconfig.APIVersion,
		Kind:         globalconfig.Kind,
		Port:         &port,
		InstanceName: defaultInstanceName,
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 2,
			Scheduling:          globalconfig.SchedulingWeighted,
			Startup: map[string]any{
				"jitter_seconds":       0,
				"max_spawn_per_second": 100,
			},
		},
		Projects: []globalconfig.Project{{
			ID:       defaultProjectID,
			Workflow: workflowPath,
			Workdir:  sourceRoot,
			Weight:   1,
			Priority: 1,
		}},
	}
}

func dbMode(path string) string {
	path = strings.TrimSpace(path)
	if path == defaultDBPath || strings.Contains(path, "mode=memory") {
		return "memory"
	}
	return "file"
}
