package kanban

import (
	"slices"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestStateNamesIgnoreCompletedSessionStates(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  workflowconfig.Config
		want []string
	}{
		{
			name: "unconfigured completed handoff ignored",
			cfg: workflowconfig.Config{
				Tracker: workflowconfig.Tracker{
					ObservedStates: []string{"Backlog", "Blocked", "Human Review"},
					ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
					TerminalStates: []string{"Done", "Cancelled"},
				},
			},
			want: []string{"Backlog", "Blocked", "Human Review", "Todo", "In Progress", "Rework", "Merging", "Done", "Cancelled", "Needs Triage"},
		},
		{
			name: "configured handoff preserved",
			cfg: workflowconfig.Config{
				Tracker: workflowconfig.Tracker{
					ObservedStates: []string{"Backlog", "Handoff"},
					ActiveStates:   []string{"Todo"},
					TerminalStates: []string{"Done"},
				},
			},
			want: []string{"Backlog", "Handoff", "Todo", "Done", "Needs Triage"},
		},
	}

	snapshot := telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{
			{ID: "tracker-extra", State: "Needs Triage"},
		},
		Completed: []telemetry.Completed{
			{
				Issue: telemetry.Issue{
					ID:    "completed-open-pr",
					State: "Handoff",
					PullRequest: &telemetry.PullRequest{
						Number: 554,
						State:  "OPEN",
					},
				},
				FinalState: "completed",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := StateNames(tt.cfg, snapshot)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("StateNames() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestStateNamesIgnoreRawGitHubRuntimeStates(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Config{
		Tracker: workflowconfig.Tracker{
			ObservedStates: []string{"Backlog", "Human Review"},
			ActiveStates:   []string{"Todo", "In Progress", "Merging"},
			TerminalStates: []string{"Done"},
		},
	}
	snapshot := telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{
			{ID: "custom", State: "Needs Triage"},
		},
		Pipeline: []telemetry.Issue{
			{ID: "pipeline-open", State: "Open"},
		},
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "running-open", State: "OPEN"}},
		},
		Queue: []telemetry.Queued{
			{Issue: telemetry.Issue{ID: "queue-closed", State: "Closed"}},
		},
		Blocked: []telemetry.Blocked{
			{Issue: telemetry.Issue{ID: "blocked-closed", State: "CLOSED"}},
		},
	}

	got := StateNames(cfg, snapshot)
	want := []string{"Backlog", "Human Review", "Todo", "In Progress", "Merging", "Done", "Needs Triage"}
	if !slices.Equal(got, want) {
		t.Fatalf("StateNames() = %#v, want %#v", got, want)
	}
}

func TestStateNamesAllowConfiguredOpenState(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Config{
		Tracker: workflowconfig.Tracker{
			ObservedStates: []string{"Open"},
			ActiveStates:   []string{"In Progress"},
			TerminalStates: []string{"Done"},
		},
	}
	snapshot := telemetry.Snapshot{
		Running: []telemetry.Running{
			{Issue: telemetry.Issue{ID: "running-open", State: "OPEN"}},
		},
	}

	got := StateNames(cfg, snapshot)
	want := []string{"Open", "In Progress", "Done"}
	if !slices.Equal(got, want) {
		t.Fatalf("StateNames() = %#v, want %#v", got, want)
	}
}

func TestSnapshotProjectDataSeq(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		Refresh: telemetry.Refresh{DataSeq: 3},
		Projects: []telemetry.ProjectSnapshot{
			{
				Project: telemetry.Project{ID: "alpha"},
				Refresh: telemetry.Refresh{DataSeq: 7},
			},
			{
				Project: telemetry.Project{ID: "bravo"},
				Refresh: telemetry.Refresh{DataSeq: 9},
			},
		},
	}

	tests := []struct {
		name      string
		projectID string
		want      uint64
	}{
		{name: "project match", projectID: "bravo", want: 9},
		{name: "fallback", projectID: "charlie", want: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := SnapshotProjectDataSeq(snapshot, tt.projectID); got != tt.want {
				t.Fatalf("SnapshotProjectDataSeq() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFilterAndApplySnapshotIssues(t *testing.T) {
	t.Parallel()

	snapshot := telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{{ID: "keep-board"}, {ID: "drop"}},
		Pipeline:    []telemetry.Issue{{ID: "keep-pipeline"}, {ID: "drop"}},
		Running:     []telemetry.Running{{Issue: telemetry.Issue{ID: "keep-running"}}, {Issue: telemetry.Issue{ID: "drop"}}},
		Queue:       []telemetry.Queued{{Issue: telemetry.Issue{ID: "keep-queue"}}, {Issue: telemetry.Issue{ID: "drop"}}},
		Blocked:     []telemetry.Blocked{{Issue: telemetry.Issue{ID: "keep-blocked"}}, {Issue: telemetry.Issue{ID: "drop"}}},
		Completed:   []telemetry.Completed{{Issue: telemetry.Issue{ID: "completed"}}},
	}

	FilterSnapshotIssues(&snapshot, func(issue telemetry.Issue) bool {
		return issue.ID != "drop"
	})
	if len(snapshot.BoardIssues) != 1 || len(snapshot.Pipeline) != 1 || len(snapshot.Running) != 1 || len(snapshot.Queue) != 1 || len(snapshot.Blocked) != 1 {
		t.Fatalf("filtered snapshot counts = board:%d pipeline:%d running:%d queue:%d blocked:%d", len(snapshot.BoardIssues), len(snapshot.Pipeline), len(snapshot.Running), len(snapshot.Queue), len(snapshot.Blocked))
	}
	if len(snapshot.Completed) != 1 {
		t.Fatalf("completed rows were filtered, count = %d", len(snapshot.Completed))
	}

	ApplySnapshotIssues(&snapshot, func(issue *telemetry.Issue) {
		issue.State = "Touched"
	})
	for _, issue := range SnapshotIssues(snapshot) {
		if issue.State != "Touched" {
			t.Fatalf("SnapshotIssues() issue %q state = %q, want Touched", issue.ID, issue.State)
		}
	}
	if snapshot.Completed[0].State != "Touched" {
		t.Fatalf("completed issue state = %q, want Touched", snapshot.Completed[0].State)
	}
}

func TestIssueStateIndexAndBlockedRefs(t *testing.T) {
	t.Parallel()

	refs := []telemetry.BlockedRef{
		{ID: "blocker"},
		{Identifier: "digitaldrywood/detent#99"},
		{ID: "missing", State: "Unknown"},
	}
	states := IssueStateIndex(telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{{
			ID:         "blocker",
			Identifier: "digitaldrywood/detent#98",
			State:      "In Progress",
		}},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{
				ID:         "done",
				Identifier: "digitaldrywood/detent#99",
				State:      "Done",
			},
		}},
	})

	got := BlockedRefsWithCurrentStates(refs, states)
	if got[0].State != "In Progress" {
		t.Fatalf("id ref state = %q, want In Progress", got[0].State)
	}
	if got[1].State != "Done" {
		t.Fatalf("identifier ref state = %q, want Done", got[1].State)
	}
	if got[2].State != "Unknown" {
		t.Fatalf("missing ref state = %q, want Unknown", got[2].State)
	}
	if refs[0].State != "" {
		t.Fatalf("source refs mutated: %#v", refs)
	}
}

func TestCardFreshEntrySelectsVisibleIssue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		snapshot  telemetry.Snapshot
		issueID   string
		wantState string
		wantOK    bool
	}{
		{
			name: "board issue wins over raw runtime state",
			snapshot: telemetry.Snapshot{
				Project:     telemetry.Project{ID: "detent"},
				BoardIssues: []telemetry.Issue{{ID: "card", ProjectID: "detent", State: "Backlog"}},
				Queue:       []telemetry.Queued{{Issue: telemetry.Issue{ID: "card", ProjectID: "detent", State: "OPEN"}}},
			},
			issueID:   "card",
			wantState: "Backlog",
			wantOK:    true,
		},
		{
			name: "runtime state falls back to lane state",
			snapshot: telemetry.Snapshot{
				Project: telemetry.Project{ID: "detent"},
				Queue:   []telemetry.Queued{{Issue: telemetry.Issue{ID: "queued", State: "OPEN"}}},
			},
			issueID:   "queued",
			wantState: "Todo",
			wantOK:    true,
		},
		{
			name: "missing issue",
			snapshot: telemetry.Snapshot{
				Project: telemetry.Project{ID: "detent"},
			},
			issueID: "missing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			entry, ok := CardFreshEntry(tt.snapshot, "detent", tt.issueID)
			if ok != tt.wantOK {
				t.Fatalf("CardFreshEntry() ok = %t, want %t", ok, tt.wantOK)
			}
			if entry.State != tt.wantState {
				t.Fatalf("CardFreshEntry() state = %q, want %q", entry.State, tt.wantState)
			}
		})
	}
}

func TestStateAllowedAndAllowedTransitions(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Config{
		Tracker: workflowconfig.Tracker{
			ObservedStates: []string{"Backlog", "Blocked"},
			ActiveStates:   []string{"Todo", "In Progress"},
			TerminalStates: []string{"Done", "Cancelled"},
		},
		Server: workflowconfig.Server{
			Kanban: workflowconfig.Kanban{
				AllowedTransitions: map[string][]string{
					"Todo":        {"In Progress", "Blocked"},
					"In Progress": {"Done", "Blocked"},
				},
			},
		},
	}

	tests := []struct {
		name           string
		state          string
		wantAllowed    bool
		target         string
		wantTransition bool
	}{
		{name: "configured state and transition allowed", state: "Todo", wantAllowed: true, target: "In Progress", wantTransition: true},
		{name: "case-insensitive configured state allowed", state: " in   progress ", wantAllowed: true, target: "Done", wantTransition: true},
		{name: "unknown state rejected by state allowlist", state: "Needs Triage"},
		{name: "configured state with blocked explicit transition", state: "Todo", wantAllowed: true, target: "Done"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := StateAllowed(cfg, tt.state); got != tt.wantAllowed {
				t.Fatalf("StateAllowed(%q) = %t, want %t", tt.state, got, tt.wantAllowed)
			}
			if tt.target == "" {
				return
			}
			gotTransition := cfg.KanbanTransitionAllowed(tt.state, tt.target)
			if gotTransition != tt.wantTransition {
				t.Fatalf("KanbanTransitionAllowed(%q, %q) = %t, want %t", tt.state, tt.target, gotTransition, tt.wantTransition)
			}
		})
	}

	transitions := AllowedTransitions(cfg, []string{"Todo", "In Progress", "Done"})
	if !slices.Equal(transitions["Todo"], []string{"In Progress", "Blocked"}) {
		t.Fatalf("AllowedTransitions()[Todo] = %#v, want In Progress/Blocked", transitions["Todo"])
	}
	if !slices.Equal(transitions["In Progress"], []string{"Done", "Blocked"}) {
		t.Fatalf("AllowedTransitions()[In Progress] = %#v, want Done/Blocked", transitions["In Progress"])
	}
	if !slices.Equal(transitions["Done"], cfg.KanbanAllowedTransitionTargets("Done")) {
		t.Fatalf("AllowedTransitions()[Done] = %#v, want workflowconfig targets", transitions["Done"])
	}
}

func TestMappedStateAndIssueRepository(t *testing.T) {
	t.Parallel()

	cfg := workflowconfig.Config{
		Tracker: workflowconfig.Tracker{
			StateMap: workflowconfig.MapValue(map[string]any{
				"Todo":        "To Do",
				"In Progress": "Doing",
				"Blocked":     "",
			}),
		},
	}

	tests := []struct {
		name  string
		state string
		want  string
	}{
		{name: "exact map", state: "Todo", want: "To Do"},
		{name: "normalized map", state: " in   progress ", want: "Doing"},
		{name: "empty map value falls back to original", state: "Blocked", want: "Blocked"},
		{name: "unmapped falls back to original", state: "Needs Triage", want: "Needs Triage"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := MappedState(cfg, tt.state); got != tt.want {
				t.Fatalf("MappedState(%q) = %q, want %q", tt.state, got, tt.want)
			}
		})
	}

	if got := MappedState(workflowconfig.Config{}, " Todo "); got != "Todo" {
		t.Fatalf("MappedState() without map = %q, want Todo", got)
	}
	if got := IssueRepository("digitaldrywood/detent#1032"); got != "digitaldrywood/detent" {
		t.Fatalf("IssueRepository() = %q, want digitaldrywood/detent", got)
	}
	if got := IssueRepository("DDW-1032"); got != "" {
		t.Fatalf("IssueRepository() = %q, want empty for non-repository identifier", got)
	}
}

func TestCloneIssueSlicesIsolatesSourceSnapshot(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 8, 13, 0, 0, 0, time.UTC)
	priority := 1
	snapshot := telemetry.Snapshot{
		BoardIssues: []telemetry.Issue{{
			ID:        "board",
			State:     "Todo",
			Priority:  &priority,
			Labels:    []string{"feature"},
			BlockedBy: []telemetry.BlockedRef{{ID: "blocker", State: "Backlog"}},
			Metadata:  map[string]string{"source": "board"},
			Comments: []telemetry.IssueComment{{
				ID:        "comment",
				CreatedAt: &now,
			}},
			PullRequest: &telemetry.PullRequest{
				Number:        7,
				RunningChecks: []string{"test"},
			},
		}},
		Running: []telemetry.Running{{
			Issue: telemetry.Issue{
				ID:       "running",
				Metadata: map[string]string{"source": "running"},
			},
		}},
		Completed: []telemetry.Completed{{
			Issue: telemetry.Issue{
				ID:        "completed",
				BlockedBy: []telemetry.BlockedRef{{ID: "done-blocker", State: "Done"}},
			},
		}},
	}

	clone := CloneIssueSlices(snapshot)
	*clone.BoardIssues[0].Priority = 2
	clone.BoardIssues[0].Labels[0] = "bug"
	clone.BoardIssues[0].BlockedBy[0].State = "Done"
	clone.BoardIssues[0].Metadata["source"] = "clone"
	clone.BoardIssues[0].Comments[0].CreatedAt = ptrTime(now.Add(time.Hour))
	clone.BoardIssues[0].PullRequest.RunningChecks[0] = "lint"
	clone.Running[0].Metadata["source"] = "clone"
	clone.Completed[0].Issue.BlockedBy[0].State = "Cancelled"

	if *snapshot.BoardIssues[0].Priority != 1 {
		t.Fatalf("source priority changed to %d", *snapshot.BoardIssues[0].Priority)
	}
	if snapshot.BoardIssues[0].Labels[0] != "feature" {
		t.Fatalf("source label changed to %q", snapshot.BoardIssues[0].Labels[0])
	}
	if snapshot.BoardIssues[0].BlockedBy[0].State != "Backlog" {
		t.Fatalf("source blocked ref changed to %q", snapshot.BoardIssues[0].BlockedBy[0].State)
	}
	if snapshot.BoardIssues[0].Metadata["source"] != "board" {
		t.Fatalf("source metadata changed to %q", snapshot.BoardIssues[0].Metadata["source"])
	}
	if !snapshot.BoardIssues[0].Comments[0].CreatedAt.Equal(now) {
		t.Fatalf("source comment time changed to %v", snapshot.BoardIssues[0].Comments[0].CreatedAt)
	}
	if snapshot.BoardIssues[0].PullRequest.RunningChecks[0] != "test" {
		t.Fatalf("source pull request checks changed to %#v", snapshot.BoardIssues[0].PullRequest.RunningChecks)
	}
	if snapshot.Running[0].Metadata["source"] != "running" {
		t.Fatalf("source running metadata changed to %q", snapshot.Running[0].Metadata["source"])
	}
	if snapshot.Completed[0].Issue.BlockedBy[0].State != "Done" {
		t.Fatalf("source completed blocked ref changed to %q", snapshot.Completed[0].Issue.BlockedBy[0].State)
	}
}

func ptrTime(value time.Time) *time.Time {
	return &value
}
