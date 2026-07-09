package orchestrator_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	local "github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/orchestrator"
)

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
	state := waitForState(t, orch, func(state orchestrator.State) bool {
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
	state = waitForState(t, orch, func(state orchestrator.State) bool {
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
