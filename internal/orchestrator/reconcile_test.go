package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
)

func TestTickReconcilesRunningIssueTrackerState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	prNumber := 226
	prior := connector.Issue{
		ID:         "issue-running",
		Identifier: "digitaldrywood/detent#225",
		Title:      "Dispatch title",
		State:      "Todo",
		URL:        "https://github.com/digitaldrywood/detent/issues/225",
		Labels:     []string{"bug"},
		PRNumber:   &prNumber,
		PullRequest: &connector.PullRequest{
			Number:           prNumber,
			URL:              "https://github.com/digitaldrywood/detent/pull/226",
			BranchName:       "detent/digitaldrywood_detent_225",
			State:            "OPEN",
			CIStatus:         "success",
			CodexReviewState: "COMMENTED",
		},
	}

	tests := []struct {
		name       string
		tracker    []connector.Issue
		err        error
		wantState  string
		wantTitle  string
		wantURL    string
		wantLabels []string
	}{
		{
			name: "updates running issue from tracker",
			tracker: []connector.Issue{
				{
					ID:         prior.ID,
					Identifier: prior.Identifier,
					Title:      "Live title",
					State:      "In Progress",
					URL:        "https://github.com/digitaldrywood/detent/issues/225#live",
					Labels:     []string{"bug", "live"},
				},
			},
			wantState:  "In Progress",
			wantTitle:  "Live title",
			wantURL:    "https://github.com/digitaldrywood/detent/issues/225#live",
			wantLabels: []string{"bug", "live"},
		},
		{
			name:       "retains previous running issue on fetch error",
			err:        errors.New("tracker unavailable"),
			wantState:  "Todo",
			wantTitle:  "Dispatch title",
			wantURL:    "https://github.com/digitaldrywood/detent/issues/225",
			wantLabels: []string{"bug"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := normalizeConfig(Config{
				PollInterval:        time.Minute,
				MaxConcurrentAgents: 1,
				ActiveStates:        []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"},
				TerminalStates:      []string{"Done", "Cancelled"},
			})
			state := newState(cfg)
			state.Running[prior.ID] = Running{Issue: cloneIssue(prior)}
			state.Claimed[prior.ID] = Claimed{Issue: cloneIssue(prior)}

			tracker := &runningStateConnector{
				issues: tt.tracker,
				err:    tt.err,
			}
			orch := &Orchestrator{
				cfg:       cfg,
				connector: tracker,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			}

			orch.tick(context.Background(), &state, now)

			snapshot := state.Snapshot(now)
			if len(snapshot.Running) != 1 {
				t.Fatalf("Running len = %d, want 1", len(snapshot.Running))
			}
			got := snapshot.Running[0].Issue
			if got.State != tt.wantState {
				t.Fatalf("running snapshot state = %q, want %q", got.State, tt.wantState)
			}
			if got.Title != tt.wantTitle {
				t.Fatalf("running snapshot title = %q, want %q", got.Title, tt.wantTitle)
			}
			if got.URL != tt.wantURL {
				t.Fatalf("running snapshot URL = %q, want %q", got.URL, tt.wantURL)
			}
			if !slices.Equal(got.Labels, tt.wantLabels) {
				t.Fatalf("running snapshot labels = %#v, want %#v", got.Labels, tt.wantLabels)
			}
			if got.PullRequest == nil {
				t.Fatal("running snapshot pull request = nil, want preserved metadata")
			}
			if got.PullRequest.URL != "https://github.com/digitaldrywood/detent/pull/226" {
				t.Fatalf("running snapshot pull request URL = %q, want preserved metadata", got.PullRequest.URL)
			}
			if got.PullRequest.CIStatus != "success" || got.PullRequest.CodexReviewState != "COMMENTED" {
				t.Fatalf("running snapshot pull request status = %#v, want preserved metadata", got.PullRequest)
			}
			if !slices.Equal(tracker.requestedIDs, []string{prior.ID}) {
				t.Fatalf("FetchIssueStatesByIDs() ids = %#v, want [%s]", tracker.requestedIDs, prior.ID)
			}
		})
	}
}

func TestTickReapsTerminalRunningIssue(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	startedAt := now.Add(-10 * time.Minute)
	terminalAt := now.Add(-30 * time.Second)
	issue := connector.Issue{
		ID:         "issue-cancelled",
		Identifier: "digitaldrywood/detent#356",
		Title:      "Cancelled session",
		State:      "In Progress",
		URL:        "https://github.com/digitaldrywood/detent/issues/356",
	}
	cancelled := cloneIssue(issue)
	cancelled.State = "Cancelled"
	cancelled.StageUpdatedAt = &terminalAt

	project := scheduler.ProjectCandidate{ID: "detent", Weight: 1}
	gate := scheduler.NewGlobalDispatchGate(scheduler.NewWeightedFair(scheduler.Config{Capacity: 1}))
	slot, ok, err := gate.TryAcquire(context.Background(), project, scheduler.SlotRequest{State: issue.State}, startedAt)
	if err != nil {
		t.Fatalf("TryAcquire() error = %v", err)
	}
	if !ok {
		t.Fatal("TryAcquire() ok = false, want true")
	}

	runCtx, cancel := context.WithCancel(context.Background())
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		Project:             project,
		ActiveStates:        []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled", "Failed"},
	})
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:      cloneIssue(issue),
		StartedAt:  startedAt,
		Tokens:     CodexTotals{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, RuntimeSeconds: 90},
		globalSlot: slot,
		cancel:     cancel,
	}
	state.Claimed[issue.ID] = Claimed{Issue: cloneIssue(issue), ClaimedAt: startedAt, Owner: "worker-1"}

	tracker := &runningStateConnector{issues: []connector.Issue{cancelled}}
	orch := &Orchestrator{
		cfg:                cfg,
		connector:          tracker,
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		globalDispatchGate: gate,
	}

	orch.tick(context.Background(), &state, now)

	if _, ok := state.Running[issue.ID]; ok {
		t.Fatalf("Running[%q] present after terminal reconciliation", issue.ID)
	}
	if _, ok := state.Claimed[issue.ID]; ok {
		t.Fatalf("Claimed[%q] present after terminal reconciliation", issue.ID)
	}
	completed, ok := state.Completed[issue.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing after terminal reconciliation", issue.ID)
	}
	if completed.FinalState != "Cancelled" {
		t.Fatalf("Completed[%q].FinalState = %q, want Cancelled", issue.ID, completed.FinalState)
	}
	if !completed.CompletedAt.Equal(terminalAt) {
		t.Fatalf("Completed[%q].CompletedAt = %v, want %v", issue.ID, completed.CompletedAt, terminalAt)
	}
	if completed.Tokens.TotalTokens != 15 {
		t.Fatalf("Completed[%q].Tokens.TotalTokens = %d, want 15", issue.ID, completed.Tokens.TotalTokens)
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("running context was not cancelled")
	}

	nextSlot, ok, err := gate.TryAcquire(context.Background(), project, scheduler.SlotRequest{State: "Todo"}, now)
	if err != nil {
		t.Fatalf("TryAcquire() after terminal reap error = %v", err)
	}
	if !ok {
		t.Fatal("TryAcquire() after terminal reap ok = false, want true")
	}
	if err := gate.Release(nextSlot); err != nil {
		t.Fatalf("Release() after terminal reap error = %v", err)
	}

	snapshot := state.Snapshot(now)
	if snapshot.Counts.Running != 0 || len(snapshot.Running) != 0 {
		t.Fatalf("snapshot running count = %d len = %d, want 0", snapshot.Counts.Running, len(snapshot.Running))
	}
	if snapshot.Counts.Completed != 1 || len(snapshot.Completed) != 1 {
		t.Fatalf("snapshot completed count = %d len = %d, want 1", snapshot.Counts.Completed, len(snapshot.Completed))
	}
}

func TestTickMarksClosedCompletedRunningIssueDoneBeforeReaping(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 16, 13, 43, 15, 0, time.UTC)
	startedAt := now.Add(-15 * time.Minute)
	closedAt := now.Add(-46 * time.Second)
	issue := connector.Issue{
		ID:         "issue-closed-completed",
		Identifier: "digitaldrywood/detent#487",
		Title:      "Windows package managers",
		State:      "Merging",
		URL:        "https://github.com/digitaldrywood/detent/issues/487",
	}
	closed := cloneIssue(issue)
	closed.State = "Done"
	closed.Closed = true
	closed.ClosedReason = "completed"
	closed.StageUpdatedAt = &closedAt

	runCtx, cancel := context.WithCancel(context.Background())
	cfg := normalizeConfig(Config{
		PollInterval:        time.Minute,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"},
		TerminalStates:      []string{"Done", "Cancelled"},
	})
	state := newState(cfg)
	state.Running[issue.ID] = Running{
		Issue:     cloneIssue(issue),
		StartedAt: startedAt,
		Tokens:    CodexTotals{TotalTokens: 42, RuntimeSeconds: 90},
		cancel:    cancel,
	}

	tracker := &runningStateConnector{issues: []connector.Issue{closed}}
	reaper := &cleanupSweepReaper{}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		reaper:    reaper,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if got, want := tracker.updates, []statusUpdate{{issueID: issue.ID, state: "Done"}}; !slices.Equal(got, want) {
		t.Fatalf("updates = %#v, want %#v", got, want)
	}
	completed, ok := state.Completed[issue.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing", issue.ID)
	}
	if completed.FinalState != "Done" || completed.Issue.State != "Done" {
		t.Fatalf("completed state = (%q, %q), want Done/Done", completed.Issue.State, completed.FinalState)
	}
	if len(reaper.issues) != 1 || reaper.issues[0].State != "Done" {
		t.Fatalf("reaped issues = %#v, want Done issue", reaper.issues)
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("running context was not cancelled")
	}
}

func TestTickCompletesTerminalRunningIssueDuringWorkspaceCleanupSweep(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 14, 15, 15, 0, 0, time.UTC)
	startedAt := now.Add(-12 * time.Minute)
	terminalAt := now.Add(-30 * time.Second)
	prior := connector.Issue{
		ID:         "issue-merged",
		Identifier: "digitaldrywood/detent#453",
		Title:      "Release snapshot",
		State:      "Merging",
		URL:        "https://github.com/digitaldrywood/detent/issues/453",
	}
	done := cloneIssue(prior)
	done.State = "Done"
	done.StageUpdatedAt = &terminalAt

	runCtx, cancel := context.WithCancel(context.Background())
	cfg := normalizeConfig(Config{
		PollInterval:                  time.Minute,
		MaxConcurrentAgents:           1,
		ActiveStates:                  []string{"Todo", "In Progress", "Human Review", "Rework", "Merging"},
		ObservedStates:                []string{"Human Review", "Merging"},
		TerminalStates:                []string{"Done", "Cancelled"},
		WorkspaceCleanupSweepInterval: time.Hour,
	})
	state := newState(cfg)
	state.LastRunningReconcileAt = now.Add(-time.Second)
	state.Running[prior.ID] = Running{
		Issue:       cloneIssue(prior),
		StartedAt:   startedAt,
		LastMessage: "GoReleaser Snapshot remains in progress; continuing to wait.",
		Tokens:      CodexTotals{TotalTokens: 42, RuntimeSeconds: 90},
		cancel:      cancel,
	}
	state.Claimed[prior.ID] = Claimed{Issue: cloneIssue(prior), ClaimedAt: startedAt, Owner: "worker-1"}

	tracker := &runningStateConnector{issuesByState: []connector.Issue{done}}
	reaper := &cleanupSweepReaper{}
	orch := &Orchestrator{
		cfg:       cfg,
		connector: tracker,
		reaper:    reaper,
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	orch.tick(context.Background(), &state, now)

	if _, ok := state.Running[prior.ID]; ok {
		t.Fatalf("Running[%q] present after terminal cleanup sweep", prior.ID)
	}
	if _, ok := state.Claimed[prior.ID]; ok {
		t.Fatalf("Claimed[%q] present after terminal cleanup sweep", prior.ID)
	}
	completed, ok := state.Completed[prior.ID]
	if !ok {
		t.Fatalf("Completed[%q] missing after terminal cleanup sweep", prior.ID)
	}
	if completed.Issue.State != "Done" || completed.FinalState != "Done" {
		t.Fatalf("Completed[%q] state = (%q, %q), want Done/Done", prior.ID, completed.Issue.State, completed.FinalState)
	}
	if !completed.CompletedAt.Equal(terminalAt) {
		t.Fatalf("Completed[%q].CompletedAt = %v, want %v", prior.ID, completed.CompletedAt, terminalAt)
	}
	if completed.Tokens.TotalTokens != 42 {
		t.Fatalf("Completed[%q].Tokens.TotalTokens = %d, want 42", prior.ID, completed.Tokens.TotalTokens)
	}
	if !slices.Equal(tracker.requestedIDs, nil) {
		t.Fatalf("FetchIssueStatesByIDs() ids = %#v, want no throttled running reconcile", tracker.requestedIDs)
	}
	if len(reaper.issues) != 1 || reaper.issues[0].ID != prior.ID || reaper.issues[0].State != "Done" {
		t.Fatalf("reaped issues = %#v, want terminal Done issue", reaper.issues)
	}
	select {
	case <-runCtx.Done():
	default:
		t.Fatal("running context was not cancelled")
	}

	snapshot := state.Snapshot(now.Add(time.Second))
	if !snapshot.GeneratedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("snapshot GeneratedAt = %v, want fresh publish time", snapshot.GeneratedAt)
	}
	if snapshot.Counts.Running != 0 || len(snapshot.Running) != 0 {
		t.Fatalf("snapshot running count = %d len = %d, want 0", snapshot.Counts.Running, len(snapshot.Running))
	}
	if snapshot.Counts.Completed != 1 || len(snapshot.Completed) != 1 || snapshot.Completed[0].State != "Done" {
		t.Fatalf("snapshot completed = %#v, want terminal Done row", snapshot.Completed)
	}
}

func TestTerminalCompletedAtUsesTerminalConditionTimestamp(t *testing.T) {
	t.Parallel()

	stageUpdatedAt := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	fallback := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	terminalStates := normalizeConfig(Config{TerminalStates: []string{"Done", "Cancelled"}}).TerminalStates

	tests := []struct {
		name  string
		issue connector.Issue
		want  time.Time
	}{
		{
			name: "status terminal uses stage update time",
			issue: connector.Issue{
				State:          "Cancelled",
				StageUpdatedAt: &stageUpdatedAt,
				UpdatedAt:      &updatedAt,
			},
			want: stageUpdatedAt,
		},
		{
			name: "closed active issue uses issue update time",
			issue: connector.Issue{
				State:          "In Progress",
				Closed:         true,
				StageUpdatedAt: &stageUpdatedAt,
				UpdatedAt:      &updatedAt,
			},
			want: updatedAt,
		},
		{
			name: "merged active pull request uses issue update time",
			issue: connector.Issue{
				State:          "Merging",
				StageUpdatedAt: &stageUpdatedAt,
				UpdatedAt:      &updatedAt,
				PullRequest:    &connector.PullRequest{State: "MERGED"},
			},
			want: updatedAt,
		},
		{
			name: "missing tracker timestamps uses fallback",
			issue: connector.Issue{
				State: "Cancelled",
			},
			want: fallback,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := terminalCompletedAt(tt.issue, terminalStates, fallback)
			if !got.Equal(tt.want) {
				t.Fatalf("terminalCompletedAt() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMergeIssueTrackerFieldsDistinguishesMissingAndEmptyMetadata(t *testing.T) {
	t.Parallel()

	current := connector.Issue{
		ID:        "issue-running",
		Assignees: []string{"worker-1"},
		Fields:    map[string]string{"Status": "Todo"},
	}

	tests := []struct {
		name          string
		refreshed     connector.Issue
		wantAssignees []string
		wantFields    map[string]string
	}{
		{
			name:          "missing metadata preserves current values",
			refreshed:     connector.Issue{ID: current.ID},
			wantAssignees: []string{"worker-1"},
			wantFields:    map[string]string{"Status": "Todo"},
		},
		{
			name:          "explicit empty metadata clears current values",
			refreshed:     connector.Issue{ID: current.ID, Assignees: []string{}, Fields: map[string]string{}},
			wantAssignees: []string{},
			wantFields:    map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := mergeIssueTrackerFields(current, tt.refreshed)
			if !slices.Equal(got.Assignees, tt.wantAssignees) {
				t.Fatalf("Assignees = %#v, want %#v", got.Assignees, tt.wantAssignees)
			}
			if len(got.Fields) != len(tt.wantFields) {
				t.Fatalf("Fields = %#v, want %#v", got.Fields, tt.wantFields)
			}
			for key, value := range tt.wantFields {
				if got.Fields[key] != value {
					t.Fatalf("Fields[%q] = %q, want %q", key, got.Fields[key], value)
				}
			}
		})
	}
}

type runningStateConnector struct {
	issues        []connector.Issue
	issuesByState []connector.Issue
	err           error
	requestedIDs  []string
	updates       []statusUpdate
}

func (c *runningStateConnector) Name() string {
	return "running-state"
}

func (c *runningStateConnector) FetchCandidateIssues(context.Context) ([]connector.Issue, error) {
	return []connector.Issue{}, nil
}

func (c *runningStateConnector) FetchIssuesByStates(context.Context, []string) ([]connector.Issue, error) {
	return cloneIssues(c.issuesByState), nil
}

func (c *runningStateConnector) FetchIssueStatesByIDs(_ context.Context, ids []string) ([]connector.Issue, error) {
	c.requestedIDs = append([]string(nil), ids...)
	if c.err != nil {
		return nil, c.err
	}
	return cloneIssues(c.issues), nil
}

func (c *runningStateConnector) CreateComment(context.Context, string, string) error {
	return nil
}

func (c *runningStateConnector) UpdateIssueState(_ context.Context, issueID string, state string) error {
	c.updates = append(c.updates, statusUpdate{issueID: issueID, state: state})
	return nil
}

func (c *runningStateConnector) SetAssignee(context.Context, string, string) error {
	return nil
}

func (c *runningStateConnector) SetField(context.Context, string, string, string) error {
	return nil
}

type cleanupSweepReaper struct {
	issues []connector.Issue
}

func (r *cleanupSweepReaper) ReapWorkspace(_ context.Context, issue connector.Issue) (WorkspaceReapResult, error) {
	r.issues = append(r.issues, cloneIssue(issue))
	return WorkspaceReapResult{}, nil
}
