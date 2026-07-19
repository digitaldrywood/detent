package orchestrator_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	local "github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

const localSQLiteE2EWaitTimeout = 10 * time.Second

// TestLocalSQLiteLifecycleEndToEnd drives the real local_sqlite connector
// through a full work-item lifecycle with the non-code artifact template's
// capitalized state vocabulary: seed a Todo item, let the orchestrator
// schedule it, observe the dispatch-start transition to Production while a
// fake agent runs, then complete and check the item stays visible and the
// snapshot carries the fields the kanban board renders (state lane and
// stage_updated_at for the "In lane" footer).
func TestLocalSQLiteLifecycleEndToEnd(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "video-work-items.db")
	seed := connector.NewIssue()
	seed.ID = "wi-e2e-1"
	seed.Identifier = "wi-e2e-1"
	seed.Title = "Rework Storyboard — round 2"
	seed.State = "Todo"

	tracker, err := local.New(local.Config{
		Path:           dbPath,
		ProjectID:      "digitaldrywood-video",
		Issues:         []connector.Issue{seed},
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	defer tracker.Close()

	release := make(chan struct{})
	runner := &staticRunner{
		result: orchestrator.RunResult{FinalState: orchestrator.FinalStateCompleted},
		onRun: func(orchestrator.RunRequest) {
			<-release
		},
	}

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:        time.Millisecond,
		MaxConcurrentAgents: 1,
		// The orchestrator lowercases these before querying the connector;
		// the seeded rows keep the template's capitalized spellings. Before
		// #1067 was fixed this mismatch made the item invisible.
		ActiveStates:           []string{"Todo", "Production", "Rework"},
		ObservedStates:         []string{"Backlog", "Review", "Blocked"},
		TerminalStates:         []string{"Ready for Pickup", "Done", "Cancelled"},
		MaxRetryBackoff:        time.Millisecond,
		FailureRetryBaseDelay:  time.Millisecond,
		ContinuationRetryDelay: time.Second,
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	// Scheduling: the capitalized Todo row must be fetched and dispatched.
	state := waitForStateWithin(t, orch, localSQLiteE2EWaitTimeout, func(state orchestrator.State) bool {
		_, ok := state.Running[seed.ID]
		return ok
	})

	// Dispatch start: the item must leave Todo while the agent works it.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open tracker db: %v", err)
	}
	defer db.Close()
	var storedState string
	if err := db.QueryRowContext(context.Background(), "select state from detent_work_items where id = ?", seed.ID).Scan(&storedState); err != nil {
		t.Fatalf("read stored state: %v", err)
	}
	if storedState != "Production" {
		t.Fatalf("stored state while running = %q, want Production", storedState)
	}

	// Snapshot: the board derives the lane from the running issue state and
	// the "In lane" footer from stage_updated_at; both must be populated.
	snapshot := state.Snapshot(time.Now())
	if snapshot.Counts.Running != 1 {
		t.Fatalf("snapshot running count = %d, want 1", snapshot.Counts.Running)
	}
	if got := snapshot.Running[0].State; got != "Production" {
		t.Fatalf("snapshot running issue state = %q, want Production", got)
	}
	if snapshot.Running[0].StageUpdatedAt == nil {
		t.Fatal("snapshot running issue StageUpdatedAt = nil, want stage timestamp")
	}

	// Completion: release the fake agent and wait for the run to finish.
	close(release)
	state = waitForStateWithin(t, orch, localSQLiteE2EWaitTimeout, func(state orchestrator.State) bool {
		_, ok := state.Completed[seed.ID]
		return ok
	})
	if _, ok := state.Blocked[seed.ID]; ok {
		t.Fatalf("Blocked[%q] present after successful completion", seed.ID)
	}

	// The item must remain visible to a lowercased state query afterwards.
	issues, err := tracker.FetchIssuesByStates(context.Background(), []string{"todo", "production", "rework"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("FetchIssuesByStates() len = %d, want 1", len(issues))
	}
	if issues[0].StageUpdatedAt == nil {
		t.Fatal("fetched issue StageUpdatedAt = nil, want parsed timestamp")
	}
}

func TestLocalSQLiteArtifactLifecycleEndToEnd(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "artifact-work-items.db")
	seed := connector.NewIssue()
	seed.ID = "wi-artifact-1"
	seed.Identifier = "wi-artifact-1"
	seed.Title = "Render launch video"
	seed.State = "Todo"
	seed.Fields = map[string]string{"render_status": "queued"}
	seed.Deliverable = &connector.Deliverable{Kind: "artifact"}

	tracker, err := local.New(local.Config{
		Path:           dbPath,
		ProjectID:      "digitaldrywood-video",
		Issues:         []connector.Issue{seed},
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	defer tracker.Close()

	runner := &staticRunner{
		result: orchestrator.RunResult{FinalState: orchestrator.FinalStateCompleted},
		onRun: func(request orchestrator.RunRequest) {
			if err := tracker.SetField(context.Background(), request.Issue.ID, "render_status", "pending_review"); err != nil {
				t.Errorf("SetField(pending_review) error = %v", err)
			}
		},
	}

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:        time.Millisecond,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "Production", "Rework"},
		ObservedStates:      []string{"Backlog", "Review", "Blocked"},
		TerminalStates:      []string{"Ready for Pickup", "Done", "Cancelled"},
		AutoPromote: orchestrator.AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Review",
			PassState:   "Ready for Pickup",
			ReworkState: "Rework",
			Gate: gate.Config{
				Kind: gate.KindArtifact,
				Artifact: gate.ArtifactConfig{
					StatusField:    "render_status",
					PassStatuses:   []string{"approved", "valid"},
					WaitStatuses:   []string{"queued", "rendering", "pending_review"},
					ReworkStatuses: []string{"recut", "invalid", "missing_assets"},
				},
			},
		},
		MaxRetryBackoff:        time.Millisecond,
		FailureRetryBaseDelay:  time.Millisecond,
		ContinuationRetryDelay: time.Millisecond,
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	defer stop()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open tracker db: %v", err)
	}
	defer db.Close()

	waitForLocalStoredState(t, db, seed.ID, "Review")
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("runner calls after Review = %d, want 1", got)
	}

	refreshAfterReview := time.Now().UTC()
	waitForState(t, orch, func(state orchestrator.State) bool {
		return state.LastRefreshAt.After(refreshAfterReview) && len(state.Running) == 0 && len(state.Retry) == 0
	})
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("runner calls after wait-status refresh = %d, want 1", got)
	}

	if err := tracker.SetField(context.Background(), seed.ID, "render_status", "approved"); err != nil {
		t.Fatalf("SetField(approved) error = %v", err)
	}
	wantEvents := []string{"Production", "Review", "Ready for Pickup"}
	waitForLocalStateUpdateEvents(t, db, seed.ID, wantEvents)
	if got := localStoredState(t, db, seed.ID); got != "Ready for Pickup" {
		t.Fatalf("stored state after approval = %q, want Ready for Pickup", got)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("runner calls after approval = %d, want 1", got)
	}
}

func TestLocalSQLiteStatuslessCompletionReleasesClaimAndSchedulesRetry(t *testing.T) {
	t.Parallel()

	seed := connector.NewIssue()
	seed.ID = "wi-artifact-statusless"
	seed.Identifier = "wi-artifact-statusless"
	seed.Title = "Recut launch video"
	seed.State = "Rework"
	seed.Fields = map[string]string{"render_status": "recut"}
	seed.Deliverable = &connector.Deliverable{Kind: "artifact"}

	tracker, err := local.New(local.Config{
		Path:           filepath.Join(t.TempDir(), "artifact-statusless.db"),
		ProjectID:      "digitaldrywood-video",
		Issues:         []connector.Issue{seed},
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := tracker.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	runner := &staticRunner{result: orchestrator.RunResult{FinalState: orchestrator.FinalStateCompleted}}
	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:           time.Millisecond,
		MaxConcurrentAgents:    1,
		ActiveStates:           []string{"Todo", "Production", "Rework"},
		ObservedStates:         []string{"Backlog", "Review", "Blocked"},
		TerminalStates:         []string{"Ready for Pickup", "Done", "Cancelled"},
		ContinuationRetryDelay: time.Hour,
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	t.Cleanup(stop)

	state := waitForState(t, orch, func(state orchestrator.State) bool {
		_, completed := state.Completed[seed.ID]
		_, retry := state.Retry[seed.ID]
		return completed && retry
	})
	if _, ok := state.Running[seed.ID]; ok {
		t.Fatalf("Running[%q] present after completed run", seed.ID)
	}
	if _, ok := state.Claimed[seed.ID]; ok {
		t.Fatalf("Claimed[%q] present after completed run", seed.ID)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Fatalf("runner calls = %d, want 1 before retry is due", got)
	}
}

func TestLocalSQLiteArtifactReworkUsesConfiguredStatusField(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "artifact-rework.db")
	seed := connector.NewIssue()
	seed.ID = "wi-artifact-rework"
	seed.Identifier = "wi-artifact-rework"
	seed.Title = "Recut launch video"
	seed.State = "Review"
	seed.Fields = map[string]string{
		"render_status": "recut",
		"review_round":  "6",
	}
	seed.Deliverable = &connector.Deliverable{
		Kind:             "artifact",
		ValidationStatus: "pending_review",
	}

	tracker, err := local.New(local.Config{
		Path:           dbPath,
		ProjectID:      "digitaldrywood-video",
		Issues:         []connector.Issue{seed},
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := tracker.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open tracker db: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	const fieldsJSON = `{"render_status":"recut","review_round":6}`
	if _, err := db.ExecContext(t.Context(), `
update detent_work_items set fields_json = ? where project_id = ? and id = ?`,
		fieldsJSON, "digitaldrywood-video", seed.ID); err != nil {
		t.Fatalf("update fields_json error = %v", err)
	}

	runner := &staticRunner{
		result: orchestrator.RunResult{FinalState: orchestrator.FinalStateCompleted},
	}

	orch, err := orchestrator.New(orchestrator.Config{
		PollInterval:        time.Second,
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "Production", "Rework"},
		ObservedStates:      []string{"Backlog", "Review", "Blocked"},
		TerminalStates:      []string{"Ready for Pickup", "Done", "Cancelled"},
		AutoPromote: orchestrator.AutoPromoteConfig{
			Enabled:       true,
			QuietDuration: 0,
			GateWaitState: "source",
			SourceState:   "Review",
			PassState:     "Ready for Pickup",
			ReworkState:   "Rework",
			ReworkLimit:   0,
			Gate: gate.Config{
				Kind: gate.KindArtifact,
				Artifact: gate.ArtifactConfig{
					StatusField:    "render_status",
					PassStatuses:   []string{"approved", "valid"},
					WaitStatuses:   []string{"pending_review"},
					ReworkStatuses: []string{"recut", "invalid", "missing_assets"},
				},
			},
		},
		MaxRetryBackoff:        time.Millisecond,
		FailureRetryBaseDelay:  time.Millisecond,
		ContinuationRetryDelay: time.Millisecond,
	}, orchestrator.Dependencies{
		Connector: tracker,
		Runner:    runner,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stop := runOrchestrator(t, orch)
	t.Cleanup(stop)

	waitForLocalStateUpdateEvents(t, db, seed.ID, []string{"Rework"})
}

func TestLocalSQLiteHumanRestartDispatchesArtifactRound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	now := time.Date(2026, 7, 10, 1, 35, 0, 0, time.UTC)
	seed := connector.NewIssue()
	seed.ID = "wi-artifact-restart"
	seed.Identifier = "wi-artifact-restart"
	seed.Title = "Rework Storyboard — round 2"
	seed.State = "Production"
	seed.CreatedAt = &now
	seed.UpdatedAt = &now
	seed.StageUpdatedAt = &now

	tracker, err := local.New(local.Config{
		Path:           filepath.Join(t.TempDir(), "artifact-restart.db"),
		ProjectID:      "digitaldrywood-video",
		Issues:         []connector.Issue{seed},
		ActiveStates:   []string{"Todo", "Production", "Rework"},
		ObservedStates: []string{"Backlog", "Review", "Blocked"},
		TerminalStates: []string{"Ready for Pickup", "Done", "Cancelled"},
		Now: func() time.Time {
			return now
		},
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := tracker.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	cfg := orchestrator.Config{
		MaxConcurrentAgents: 1,
		ActiveStates:        []string{"Todo", "Production", "Rework"},
		ObservedStates:      []string{"Backlog", "Review", "Blocked"},
		TerminalStates:      []string{"Ready for Pickup", "Done", "Cancelled"},
		AutoPromote: orchestrator.AutoPromoteConfig{
			Enabled:     true,
			SourceState: "Review",
			PassState:   "Ready for Pickup",
			ReworkState: "Rework",
			Gate: gate.Config{
				Kind: gate.KindArtifact,
				Artifact: gate.ArtifactConfig{
					StatusField:    "render_status",
					PassStatuses:   []string{"approved", "valid"},
					WaitStatuses:   []string{"queued", "rendering", "pending_review"},
					ReworkStatuses: []string{"recut", "invalid", "missing_assets"},
				},
			},
		},
	}

	now = now.Add(time.Minute)
	if err := tracker.SetField(ctx, seed.ID, "render_status", "pending_review"); err != nil {
		t.Fatalf("SetField(pending_review) error = %v", err)
	}
	waiting, err := tracker.FetchIssuesByStates(ctx, []string{"Production"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates(Production) error = %v", err)
	}
	if plan := orchestrator.PlanDispatch(cfg, orchestrator.State{}, waiting, now); len(plan.Dispatches) != 0 {
		t.Fatalf("mid-wait dispatches = %#v, want none", plan.Dispatches)
	}

	now = now.Add(time.Minute)
	if err := tracker.UpdateIssueState(ctx, seed.ID, "Todo"); err != nil {
		t.Fatalf("UpdateIssueState(Todo) error = %v", err)
	}
	restarted, err := tracker.FetchIssuesByStates(ctx, []string{"Todo"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates(Todo) error = %v", err)
	}
	plan := orchestrator.PlanDispatch(cfg, orchestrator.State{}, restarted, now)
	if len(plan.Dispatches) != 1 || plan.Dispatches[0].IssueID != seed.ID {
		t.Fatalf("restarted dispatches = %#v, want %s", plan.Dispatches, seed.ID)
	}
}

func waitForLocalStoredState(t *testing.T, db *sql.DB, issueID string, want string) {
	t.Helper()

	deadline := time.After(localSQLiteE2EWaitTimeout)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := localStoredState(t, db, issueID); got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for stored state %q; got %q", want, localStoredState(t, db, issueID))
		case <-ticker.C:
		}
	}
}

func localStoredState(t *testing.T, db *sql.DB, issueID string) string {
	t.Helper()

	var state string
	if err := db.QueryRowContext(context.Background(), "select state from detent_work_items where id = ?", issueID).Scan(&state); err != nil {
		t.Fatalf("read stored state: %v", err)
	}
	return state
}

func waitForLocalStateUpdateEvents(t *testing.T, db *sql.DB, issueID string, want []string) {
	t.Helper()

	deadline := time.After(localSQLiteE2EWaitTimeout)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := localStateUpdateEvents(t, db, issueID); slices.Equal(got, want) {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for state_update events %#v; got %#v", want, localStateUpdateEvents(t, db, issueID))
		case <-ticker.C:
		}
	}
}

func localStateUpdateEvents(t *testing.T, db *sql.DB, issueID string) []string {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `
select state from detent_work_item_events
where item_id = ? and event_kind = 'state_update'
order by id`, issueID)
	if err != nil {
		t.Fatalf("read state_update events: %v", err)
	}
	defer rows.Close()

	var events []string
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatalf("scan state_update event: %v", err)
		}
		events = append(events, state)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate state_update events: %v", err)
	}
	return events
}
