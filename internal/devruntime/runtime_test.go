package devruntime

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector"
)

func TestBuildCreatesIsolatedRuntimeDefaults(t *testing.T) {
	t.Parallel()

	runtime, err := Build(Config{Home: t.TempDir(), Port: 0})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if runtime.DBPath != defaultDBPath || runtime.DBMode != "memory" {
		t.Fatalf("DB = %q mode %q, want :memory: memory", runtime.DBPath, runtime.DBMode)
	}
	if runtime.TrackerMode != TrackerMemory {
		t.Fatalf("TrackerMode = %q, want %q", runtime.TrackerMode, TrackerMemory)
	}
	if runtime.Global.Path != runtime.ConfigPath {
		t.Fatalf("Global.Path = %q, want %q", runtime.Global.Path, runtime.ConfigPath)
	}
	for _, path := range []string{runtime.Home, runtime.ConfigPath, runtime.WorkflowPath, runtime.WorkspaceRoot, runtime.SourceRoot} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("Stat(%s) error = %v", path, err)
		}
	}
	if samePath(runtime.Home, filepath.Dir(runtime.ConfigPath)) != true {
		t.Fatalf("config path %s is not under home %s", runtime.ConfigPath, runtime.Home)
	}

	workflow, err := workflowconfig.LoadWorkflow(runtime.WorkflowPath)
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}
	if workflow.Config.Tracker.Kind != workflowconfig.TrackerMemory {
		t.Fatalf("workflow tracker = %q, want memory", workflow.Config.Tracker.Kind)
	}
	if workflow.Config.Workspace.Root != runtime.WorkspaceRoot {
		t.Fatalf("workspace root = %q, want %q", workflow.Config.Workspace.Root, runtime.WorkspaceRoot)
	}
	if len(workflow.Config.Tracker.Issues) < 4 {
		t.Fatalf("fixture issues = %d, want default dogfood coverage", len(workflow.Config.Tracker.Issues))
	}
}

func TestBuildLoadsFixtureIssues(t *testing.T) {
	t.Parallel()

	fixture := filepath.Join(t.TempDir(), "fixture.yaml")
	raw, err := yaml.Marshal(struct {
		Issues []connector.Issue `yaml:"issues"`
	}{
		Issues: []connector.Issue{{
			ID:         "fixture-issue",
			Identifier: "digitaldrywood/detent#42",
			State:      "Human Review",
			PullRequest: &connector.PullRequest{
				Number:   42,
				State:    "OPEN",
				CIStatus: "success",
			},
		}},
	})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(fixture, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	runtime, err := Build(Config{Home: t.TempDir(), FixturePath: fixture})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if runtime.FixturePath == "" {
		t.Fatal("FixturePath is blank, want resolved fixture path")
	}
	if len(runtime.Issues) != 1 || runtime.Issues[0].ID != "fixture-issue" {
		t.Fatalf("Issues = %#v, want fixture issue", runtime.Issues)
	}
}

func TestBuildKanbanDemoCreatesIntegrationWorkflow(t *testing.T) {
	t.Parallel()

	runtime, err := Build(Config{Home: t.TempDir(), Demo: DemoKanban, Port: 0})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if runtime.Demo != DemoKanban {
		t.Fatalf("Demo = %q, want %q", runtime.Demo, DemoKanban)
	}
	if runtime.TrackerMode != TrackerMemory || runtime.FixturePath != "" {
		t.Fatalf("TrackerMode = %q FixturePath = %q, want memory demo without external fixture", runtime.TrackerMode, runtime.FixturePath)
	}

	workflow, err := workflowconfig.LoadWorkflow(runtime.WorkflowPath)
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}
	if workflow.Config.Tracker.Kind != workflowconfig.TrackerMemory {
		t.Fatalf("workflow tracker = %q, want memory", workflow.Config.Tracker.Kind)
	}
	if workflow.Config.Server.Kanban.Mode != workflowconfig.KanbanModeIntegration {
		t.Fatalf("Kanban mode = %q, want integration", workflow.Config.Server.Kanban.Mode)
	}
	if got := workflow.Config.Tracker.ActiveStates; !slices.Equal(got, []string{"Cancelled"}) {
		t.Fatalf("ActiveStates = %#v, want terminal-only demo state", got)
	}
	if !workflow.Config.KanbanTransitionAllowed("Backlog", "Todo") {
		t.Fatal("KanbanTransitionAllowed(Backlog, Todo) = false, want explicit demo transition")
	}
	if workflow.Config.KanbanTransitionAllowed("Done", "Todo") {
		t.Fatal("KanbanTransitionAllowed(Done, Todo) = true, want terminal demo lane without active move")
	}
	if workflow.Config.Agent.AutoPromote.Enabled {
		t.Fatal("AutoPromote.Enabled = true, want stable demo board")
	}

	states := map[string]int{}
	activeStates := stateSet(workflow.Config.Tracker.ActiveStates)
	terminalStates := stateSet(workflow.Config.Tracker.TerminalStates)
	var issueOnly, linkedPR, ciPass, ciPending, ciFail, reviewClean, reviewFinding bool
	var labels, assignees, blockers, waitMetadata bool
	for _, issue := range workflow.Config.Tracker.Issues {
		states[issue.State]++
		if _, active := activeStates[strings.ToLower(issue.State)]; active {
			if _, terminal := terminalStates[strings.ToLower(issue.State)]; !terminal {
				t.Fatalf("demo issue %q in state %q would be dispatchable", issue.ID, issue.State)
			}
		}
		issueOnly = issueOnly || issue.PullRequest == nil
		labels = labels || len(issue.Labels) > 0
		assignees = assignees || len(issue.Assignees) > 0
		blockers = blockers || len(issue.BlockedBy) > 0 || strings.TrimSpace(issue.BlockerReason) != ""
		if issue.PullRequest == nil {
			continue
		}
		linkedPR = true
		switch strings.ToLower(strings.TrimSpace(issue.PullRequest.CIStatus)) {
		case "success", "pass":
			ciPass = true
		case "pending", "in_progress":
			ciPending = true
		case "failure", "fail":
			ciFail = true
		}
		reviewClean = reviewClean || strings.EqualFold(issue.PullRequest.CodexReviewState, "CLEAN")
		reviewFinding = reviewFinding || strings.EqualFold(issue.PullRequest.CodexReviewState, "P1") || len(issue.PullRequest.CodexReviewFindings) > 0
		waitMetadata = waitMetadata ||
			issue.PullRequest.CIDurationSeconds > 0 ||
			len(issue.PullRequest.SlowChecks) > 0 ||
			len(issue.PullRequest.RunningChecks) > 0
	}
	for _, state := range []string{"Backlog", "Todo", "In Progress", "Blocked", "Human Review", "Rework", "Merging", "Done", "Cancelled"} {
		if states[state] == 0 {
			t.Fatalf("demo state %q count = 0; states = %#v", state, states)
		}
	}
	checks := map[string]bool{
		"issue-only":     issueOnly,
		"linked PR":      linkedPR,
		"CI pass":        ciPass,
		"CI pending":     ciPending,
		"CI fail":        ciFail,
		"review clean":   reviewClean,
		"review finding": reviewFinding,
		"labels":         labels,
		"assignees":      assignees,
		"blockers":       blockers,
		"wait metadata":  waitMetadata,
	}
	for name, ok := range checks {
		if !ok {
			t.Fatalf("demo fixture missing %s coverage", name)
		}
	}
}

func stateSet(states []string) map[string]struct{} {
	out := make(map[string]struct{}, len(states))
	for _, state := range states {
		state = strings.ToLower(strings.TrimSpace(state))
		if state != "" {
			out[state] = struct{}{}
		}
	}
	return out
}

func TestBuildRejectsUnsafeRuntimeInputs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
		want error
	}{
		{
			name: "live dogfood port",
			cfg:  Config{Home: t.TempDir(), Port: liveDogfoodPort},
			want: ErrLivePort,
		},
		{
			name: "unsupported tracker",
			cfg:  Config{Home: t.TempDir(), TrackerMode: "github"},
			want: ErrUnsupportedTracker,
		},
		{
			name: "unsupported demo",
			cfg:  Config{Home: t.TempDir(), Demo: "unknown"},
			want: ErrUnsupportedDemo,
		},
		{
			name: "demo fixture conflict",
			cfg:  Config{Home: t.TempDir(), Demo: DemoKanban, FixturePath: "fixture.yaml"},
			want: ErrDemoFixtureConflict,
		},
		{
			name: "live database",
			cfg:  Config{Home: t.TempDir(), DBPath: liveDatabasePath()},
			want: ErrLiveDatabase,
		},
		{
			name: "live database sqlite file uri",
			cfg:  Config{Home: t.TempDir(), DBPath: sqliteLiveDatabaseURI()},
			want: ErrLiveDatabase,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := Build(tt.cfg)
			if !errors.Is(err, tt.want) {
				t.Fatalf("Build() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestBuildAllowsExplicitUnsafeOverrides(t *testing.T) {
	t.Parallel()

	runtime, err := Build(Config{
		Home:                t.TempDir(),
		Port:                liveDogfoodPort,
		DBPath:              liveDatabasePath(),
		AllowProductionPort: true,
		AllowLiveDatabase:   true,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if runtime.Port != liveDogfoodPort {
		t.Fatalf("Port = %d, want %d", runtime.Port, liveDogfoodPort)
	}
}

func TestBuildAllowsNamedSharedMemoryURI(t *testing.T) {
	t.Parallel()

	dbPath := "file:detent-dev-runtime?mode=memory&cache=shared"
	runtime, err := Build(Config{
		Home:   t.TempDir(),
		DBPath: dbPath,
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if runtime.DBPath != dbPath || runtime.DBMode != "memory" {
		t.Fatalf("DB = %q mode %q, want shared memory URI", runtime.DBPath, runtime.DBMode)
	}
}

func sqliteLiveDatabaseURI() string {
	return "file:" + strings.ReplaceAll(filepath.ToSlash(liveDatabasePath()), " ", "%20") + "?mode=rw"
}
