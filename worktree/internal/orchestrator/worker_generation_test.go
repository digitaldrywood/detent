package orchestrator

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	"github.com/digitaldrywood/detent/internal/procstart"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type generationOwners struct {
	current store.Store
	foreign store.Store
	legacy  store.Store
}

func openGenerationOwners(t *testing.T) generationOwners {
	t.Helper()
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}
	path := filepath.Join(t.TempDir(), "detent.db")
	start, err := procstart.Identity(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	open := func(register bool) store.Store {
		backend, err := store.Open(t.Context(), store.Config{Backend: store.BackendSQLite, Path: path})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = backend.Close() })
		if register {
			if _, err := backend.StartWorkerGeneration(t.Context(), store.WorkerGenerationStart{PID: os.Getpid(), ProcessStart: start, Version: "test", StartedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
		}
		return backend
	}
	legacy := open(false)
	foreign := open(true)
	return generationOwners{legacy: legacy, foreign: foreign, current: open(true)}
}

func TestRecoverDurableWorkAttemptsLeavesLiveForeignGenerations(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Truncate(time.Second)
	tests := []struct {
		name          string
		start         store.WorkAttemptStart
		wantRecovered func(*testing.T, State, store.WorkAttempt) bool
	}{
		{
			name:  "deferred completion",
			start: store.WorkAttemptStart{Phase: deferredCompletionPhase, LeaseExpiresAt: now.Add(time.Hour), WorkerMetadataJSON: `{}`},
			wantRecovered: func(_ *testing.T, _ State, attempt store.WorkAttempt) bool {
				return attempt.Status == store.WorkAttemptStatusTerminal && attempt.ErrorClass == deferredCompletionInvalidClass
			},
		},
		{
			name:  "expired lease",
			start: store.WorkAttemptStart{LeaseExpiresAt: now.Add(-time.Minute)},
			wantRecovered: func(_ *testing.T, _ State, attempt store.WorkAttempt) bool {
				return attempt.Status == store.WorkAttemptStatusTerminal && attempt.ErrorClass == "lease_expired"
			},
		},
		{
			name:  "local admission",
			start: store.WorkAttemptStart{LeaseExpiresAt: now.Add(time.Hour), WorkerMetadataJSON: `{"run_mode":"local"}`},
			wantRecovered: func(_ *testing.T, state State, attempt store.WorkAttempt) bool {
				_, admitted := state.localAdmitted[attempt.IssueID]
				return admitted
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			owners := openGenerationOwners(t)
			attempts := map[string]int64{}
			for owner, backend := range map[string]store.Store{"legacy": owners.legacy, "foreign": owners.foreign} {
				start := test.start
				start.ProjectID, start.IssueID, start.Identifier = "detent", owner, "digitaldrywood/detent#"+owner
				start.WorkerType, start.StartedAt = "implement", now.Add(-time.Hour)
				id, err := backend.StartWorkAttempt(t.Context(), start)
				if err != nil {
					t.Fatal(err)
				}
				attempts[owner] = id
			}
			cfg := completionDeferralConfig()
			orch, err := New(cfg, Dependencies{Connector: memory.New(memory.Config{}), WorkAttempts: owners.current})
			if err != nil {
				t.Fatal(err)
			}
			state := newState(orch.cfg)
			orch.recoverDurableWorkAttempts(t.Context(), &state, now)
			for owner, want := range map[string]bool{"legacy": true, "foreign": false} {
				attempt, err := owners.current.WorkAttempt(t.Context(), attempts[owner])
				if err != nil {
					t.Fatal(err)
				}
				if got := test.wantRecovered(t, state, attempt); got != want {
					t.Errorf("%s attempt recovered = %t, want %t: %+v", owner, got, want, attempt)
				}
			}
		})
	}
}

type recordingRetentionReaper struct {
	cleanupSweepReaper
	active []workspace.Issue
}

func (r *recordingRetentionReaper) SweepRetention(_ context.Context, request workspace.RetentionRequest) (workspace.RetentionTotals, error) {
	r.active = request.Active
	return workspace.RetentionTotals{Workdir: "test", At: request.Now}, nil
}

func TestRetentionProtectsLiveForeignGenerationWorkspaces(t *testing.T) {
	t.Parallel()
	owners := openGenerationOwners(t)
	now := time.Now().UTC()
	for owner, backend := range map[string]store.Store{"legacy": owners.legacy, "foreign": owners.foreign, "current": owners.current} {
		if _, err := backend.StartWorkAttempt(t.Context(), store.WorkAttemptStart{
			ProjectID: "detent", IssueID: owner, Identifier: "digitaldrywood/detent#" + owner,
			WorkerType: "implement", StartedAt: now, LeaseExpiresAt: now.Add(time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	reaper := &recordingRetentionReaper{}
	o := &Orchestrator{
		cfg:          completionDeferralConfig(),
		reaper:       reaper,
		workAttempts: owners.current,
		connector:    memory.New(memory.Config{Issues: []connector.Issue{}}),
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	o.sweepRetention(t.Context(), &State{}, now)
	var protected []string
	for _, issue := range reaper.active {
		protected = append(protected, issue.ID)
	}
	if !slices.Equal(protected, []string{"foreign"}) {
		t.Fatalf("protected workspaces = %v, want only the live foreign generation's", protected)
	}
}
