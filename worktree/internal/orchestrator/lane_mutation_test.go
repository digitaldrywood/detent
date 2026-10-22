package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func laneMutationTestConfig() Config {
	return normalizeConfig(Config{
		Project:        scheduler.ProjectCandidate{ID: "detent"},
		ActiveStates:   []string{"Todo", "In Progress", "Rework", "Merging"},
		ObservedStates: []string{"Human Review", "Blocked"},
		TerminalStates: []string{"Done", "Cancelled"},
		AutoPromote: AutoPromoteConfig{
			Enabled:     true,
			PassState:   "Merging",
			ReworkState: "Rework",
		},
	})
}

func openLaneMutationTestStore(
	t *testing.T,
	ctx context.Context,
	projectID string,
	issue connector.Issue,
	now time.Time,
) (store.Store, int64) {
	t.Helper()

	runtimeStore, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: filepath.Join(t.TempDir(), "detent.db")})
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = runtimeStore.Close() })
	attemptID, err := runtimeStore.StartWorkAttempt(ctx, store.WorkAttemptStart{
		ProjectID:      projectID,
		IssueID:        issue.ID,
		Identifier:     issue.Identifier,
		WorkerType:     "agent",
		Lane:           issue.State,
		AttemptNumber:  1,
		StartedAt:      now.Add(-time.Minute),
		LeaseExpiresAt: now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("StartWorkAttempt() error = %v", err)
	}
	return runtimeStore, attemptID
}

func newLaneMutationTestOrchestrator(
	cfg Config,
	tracker connector.Connector,
	runtimeStore store.Store,
	attempts store.WorkAttemptStore,
	now time.Time,
) *Orchestrator {
	return &Orchestrator{
		cfg:             cfg,
		connector:       tracker,
		workflowMetrics: runtimeStore,
		workAttempts:    attempts,
		laneLedger:      runtimeStore,
		logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:             func() time.Time { return now },
	}
}
