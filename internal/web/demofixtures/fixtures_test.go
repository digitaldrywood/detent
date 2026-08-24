package demofixtures

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/observability"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestSnapshotForScenarioVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		projectID string
		variant   string
	}{
		{name: "empty", variant: "empty"},
		{name: "healthy", projectID: "dogfood", variant: "healthy"},
		{name: "overloaded", projectID: "dogfood", variant: "overloaded"},
		{name: "draining", projectID: "dogfood", variant: "draining"},
		{name: "dense", projectID: "dogfood", variant: "dense"},
		{name: "degraded", projectID: "dogfood", variant: "degraded"},
		{name: "agent pools", projectID: "dogfood", variant: "agent-pools"},
		{name: "default agent pool", projectID: "dogfood", variant: "agent-pool-default"},
		{name: "pending update", projectID: "dogfood", variant: "pending-update"},
		{name: "github api healthy", variant: "github-api-healthy"},
		{name: "github api warning", variant: "github-api-warning"},
		{name: "github api secondary backoff", variant: "github-api-secondary-backoff"},
		{name: "github api primary exhausted", variant: "github-api-primary-exhausted"},
		{name: "dense kanban", projectID: "dogfood", variant: "dense-kanban"},
		{name: "paused", projectID: "mobile-client", variant: "paused"},
		{name: "project empty", projectID: "agent-lab", variant: "project-empty"},
		{name: "hot path", projectID: "billing-api", variant: "hot-path"},
		{name: "not found", projectID: "missing-project", variant: "not-found"},
		{name: "startup loading", projectID: "dogfood", variant: "startup-loading"},
		{name: "transition blocked", projectID: "dogfood", variant: "transition-blocked"},
		{name: "terminal", projectID: "dogfood", variant: "terminal"},
		{name: "tracker refresh gap", projectID: "dogfood", variant: "tracker-refresh-gap"},
		{name: "external lane timer", projectID: "dogfood", variant: "external-lane-timer"},
		{name: "backoff heavy", projectID: "dogfood", variant: "backoff-heavy"},
		{name: "blocked heavy", projectID: "billing-api", variant: "blocked-heavy"},
		{name: "long content", projectID: "dogfood", variant: "long-content"},
		{name: "budget refusals", projectID: "dogfood", variant: "budget-refusals"},
		{name: "no history", projectID: "agent-lab", variant: "no-history"},
		{name: "one board staleness warning", variant: "board-staleness-one"},
		{name: "twenty board staleness warnings", variant: "board-staleness-twenty"},
		{name: "settings empty", variant: "settings-empty"},
		{name: "settings long paths", variant: "settings-long-paths"},
		{name: "settings missing", variant: "settings-missing"},
		{name: "reports empty", variant: "reports-empty"},
		{name: "model heavy", variant: "model-heavy"},
		{name: "filtered project", projectID: "dogfood", variant: "filtered-project"},
		{name: "invalid date range", variant: "invalid-date-range"},
		{name: "tracker", variant: "tracker"},
		{name: "credentials", variant: "credentials"},
		{name: "project", variant: "project"},
		{name: "agent", variant: "agent"},
		{name: "write", variant: "write"},
		{name: "validation errors", variant: "validation-errors"},
		{name: "write exists", variant: "write-exists"},
		{name: "write success", variant: "write-success"},
		{name: "kanban move dialog", projectID: "dogfood", variant: "kanban-move-dialog"},
		{name: "kanban move missing target", projectID: "dogfood", variant: "kanban-move-missing-target"},
		{name: "kanban read only", projectID: "dogfood", variant: "kanban-read-only"},
		{name: "kanban move success", projectID: "dogfood", variant: "kanban-move-success"},
		{name: "connector failure", projectID: "dogfood", variant: "connector-failure"},
		{name: "kanban comment issue", projectID: "dogfood", variant: "kanban-comment-issue"},
		{name: "kanban comment pr", projectID: "dogfood", variant: "kanban-comment-pr"},
		{name: "kanban comment invalid target", projectID: "dogfood", variant: "kanban-comment-invalid-target"},
		{name: "kanban comment success", projectID: "dogfood", variant: "kanban-comment-success"},
		{name: "kanban comment empty body", projectID: "dogfood", variant: "kanban-comment-empty-body"},
		{name: "refresh accepted", variant: "refresh-accepted"},
		{name: "refresh unavailable", variant: "refresh-unavailable"},
		{name: "invalid query", variant: "invalid-query"},
		{name: "play", variant: "play"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snapshot := SnapshotForScenario(tt.projectID, tt.variant)
			if snapshot.Instance.Name == "" && snapshot.DashboardURL == "" && snapshot.Shutdown.Status == "" {
				t.Fatalf("SnapshotForScenario(%q, %q) returned an empty snapshot", tt.projectID, tt.variant)
			}
			assertSnapshotProjectIDs(t, snapshot)
			assertSnapshotIssueIDs(t, snapshot)
		})
	}
}

func TestBoardStalenessWarningScenarios(t *testing.T) {
	t.Parallel()

	tests := []struct {
		variant string
		want    int
	}{
		{variant: "board-staleness-one", want: 1},
		{variant: "board-staleness-twenty", want: 20},
	}
	for _, tt := range tests {
		t.Run(tt.variant, func(t *testing.T) {
			t.Parallel()
			snapshot := SnapshotForScenario("", tt.variant)
			if len(snapshot.StalenessWarnings) != tt.want {
				t.Fatalf("staleness warnings = %d, want %d", len(snapshot.StalenessWarnings), tt.want)
			}
			for _, warning := range snapshot.StalenessWarnings {
				if warning.Class != observability.ClassFault || warning.Kind != "repeated_decision" || warning.IssueURL == "" {
					t.Fatalf("warning = %#v, want linked fault-class repeated decision", warning)
				}
			}
		})
	}
}

func TestHealthySnapshotIncludesRuntimeIdentityFixtures(t *testing.T) {
	t.Parallel()

	snapshot := SnapshotForScenario("", "healthy")
	if len(snapshot.Running) != 3 {
		t.Fatalf("Running len = %d, want 3", len(snapshot.Running))
	}
	if got := snapshot.Running[0].RuntimeIdentity; got.BackendKind != "codex" || got.Provider.Value != "openai" || got.Model() != "gpt-5.6-sol" || got.ReasoningEffort.Value != "xhigh" {
		t.Fatalf("Codex runtime identity = %#v", got)
	}
	if got := snapshot.Running[1].RuntimeIdentity; got.BackendKind != "claude_code" || got.Provider.Value != "ollama" || got.Model() != "qwen3-coder" || got.ReasoningEffort.Known() {
		t.Fatalf("Claude runtime identity = %#v", got)
	}
}

func assertSnapshotProjectIDs(t *testing.T, snapshot telemetry.Snapshot) {
	t.Helper()

	seen := map[string]struct{}{}
	for _, project := range snapshot.Projects {
		id := project.Project.ID
		if id == "" {
			t.Fatalf("snapshot project has empty ID: %#v", project.Project)
		}
		if _, ok := seen[id]; ok {
			t.Fatalf("snapshot project ID %q is duplicated", id)
		}
		seen[id] = struct{}{}
	}
}

func assertSnapshotIssueIDs(t *testing.T, snapshot telemetry.Snapshot) {
	t.Helper()

	seen := map[string]struct{}{}
	for _, issue := range snapshotIssues(snapshot) {
		if issue.ID == "" {
			t.Fatalf("snapshot issue has empty ID: %#v", issue)
		}
		if issue.ProjectID == "" {
			t.Fatalf("snapshot issue %q has empty project ID", issue.ID)
		}
		if _, ok := seen[issue.ID]; ok {
			t.Fatalf("snapshot issue ID %q is duplicated", issue.ID)
		}
		seen[issue.ID] = struct{}{}
	}
}

func snapshotIssues(snapshot telemetry.Snapshot) []telemetry.Issue {
	issues := make([]telemetry.Issue, 0, len(snapshot.BoardIssues)+len(snapshot.Pipeline)+len(snapshot.Running)+len(snapshot.Queue)+len(snapshot.Blocked)+len(snapshot.Completed))
	issues = append(issues, snapshot.BoardIssues...)
	issues = append(issues, snapshot.Pipeline...)
	for _, issue := range snapshot.Running {
		issues = append(issues, issue.Issue)
	}
	for _, issue := range snapshot.Queue {
		issues = append(issues, issue.Issue)
	}
	for _, issue := range snapshot.Blocked {
		issues = append(issues, issue.Issue)
	}
	for _, issue := range snapshot.Completed {
		issues = append(issues, issue.Issue)
	}
	return issues
}
