package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLegacyRollbackPreservesOldReaderState(t *testing.T) {
	t.Parallel()

	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			statePath := filepath.Join(dir, startupRecoveryStateName)
			pending := PendingUpdate{
				FromVersion: "0.93.0", ToVersion: "0.94.0",
				ExecutablePath: filepath.Join(dir, "detent"), PreviousBinaryPath: filepath.Join(dir, "detent.previous"),
				AppliedAt: time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC),
			}
			writePendingUpdateStateWithoutCommit(t, statePath, pending, 1)
			rollbackCalls := 0
			for attempt := 1; attempt <= startupCrashLoopThreshold; attempt++ {
				recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
					StatePath: statePath, CurrentVersion: pending.ToVersion, GOOS: goos,
					Rollback: func(_ context.Context, _ PendingUpdate) error {
						rollbackCalls++
						state := readLegacyStartupRecoveryState(t, statePath)
						if state.PendingUpdate == nil || state.PendingUpdate.RollbackRequestedAt == nil {
							t.Fatal("rollback intent must be readable before replacing the executable")
						}
						return nil
					},
				})
				identityErr := recovery.MarkHealthy(t.Context())
				if identityErr == nil {
					t.Fatal("legacy target without tested commit must not become healthy")
				}
				recovery.HandleFailure(t.Context(), identityErr)
				state := readLegacyStartupRecoveryState(t, statePath)
				if state.ActiveFailure == nil || state.ActiveFailure.Count != attempt {
					t.Fatalf("ActiveFailure = %#v, want count %d", state.ActiveFailure, attempt)
				}
			}
			if rollbackCalls != 1 {
				t.Fatalf("rollback calls = %d, want 1", rollbackCalls)
			}
			state := readLegacyStartupRecoveryState(t, statePath)
			if state.LastRollback == nil || state.LastRollback.FromVersion != pending.ToVersion || state.LastRollback.ToVersion != pending.FromVersion {
				t.Fatalf("LastRollback = %#v, want completed rollback", state.LastRollback)
			}
			if state.ActiveFailure.RecoveryAction != "rolled_back_update" || !state.ActiveFailure.CrashLoop || state.LastCrashLoop == nil || state.LastCrashLoop.Count != startupCrashLoopThreshold || state.ActiveFailure.NextRetryAt == nil || state.ActiveFailure.Backoff != 30*time.Second {
				t.Fatalf("state = %#v, want rollback outcome and retry history", state)
			}
			if (state.PendingUpdate != nil) != (goos == "windows") {
				t.Fatalf("PendingUpdate = %#v, want pending only for delayed Windows replacement", state.PendingUpdate)
			}

			// The restored reader must retain the history on subsequent writes,
			// even though POSIX rollback already cleared the pending operation.
			recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
				StatePath: statePath, CurrentVersion: pending.FromVersion, CurrentCommit: testPreviousCommit, GOOS: goos,
				BinaryVerifier: func(context.Context, string) (string, error) {
					return "version: " + pending.FromVersion + "\ncommit: " + testPreviousCommit, nil
				},
			})
			if err := recovery.MarkHealthy(t.Context()); err != nil {
				t.Fatal(err)
			}
			state = readLegacyStartupRecoveryState(t, statePath)
			if state.PendingUpdate != nil || state.ActiveFailure != nil || state.LastHealthyAt == nil || state.LastCrashLoop == nil || state.LastRollback == nil {
				t.Fatalf("healthy rollback state = %#v, want preserved history", state)
			}
		})
	}
}

// This reproduces the schema-1 reader contract from before provenance support.
// Do not use the current loader, which accepts schema 2 and masks incompatibility.
func readLegacyStartupRecoveryState(t *testing.T, path string) startupRecoveryState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state startupRecoveryState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.Schema != 1 {
		t.Fatalf("legacy reader rejected schema %d, want 1", state.Schema)
	}
	return state
}
