package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStartupRecoveryUnexpectedRestartIdentity(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name             string
		currentVersion   string
		currentCommit    string
		installedVersion string
		installedCommit  string
		rollback         bool
		readErr          bool
		wantErr          bool
	}{
		{name: "previous copy while target installed", currentVersion: "1.0.0", currentCommit: testPreviousCommit, installedVersion: "1.1.0", installedCommit: testUpdatedCommit, wantErr: true},
		{name: "rollback intent without completion", currentVersion: "1.0.0", currentCommit: testPreviousCommit, installedVersion: "1.1.0", installedCommit: testUpdatedCommit, rollback: true, wantErr: true},
		{name: "unrelated restarted version", currentVersion: "0.9.0", currentCommit: testPreviousCommit, wantErr: true},
		{name: "previous version wrong commit", currentVersion: "1.0.0", currentCommit: testUpdatedCommit, installedVersion: "1.0.0", installedCommit: testUpdatedCommit, wantErr: true},
		{name: "installed previous version wrong commit", currentVersion: "1.0.0", currentCommit: testPreviousCommit, installedVersion: "1.0.0", installedCommit: testUpdatedCommit, wantErr: true},
		{name: "installed identity unavailable", currentVersion: "1.0.0", currentCommit: testPreviousCommit, readErr: true, wantErr: true},
		{name: "replacement never applied", currentVersion: "1.0.0", currentCommit: testPreviousCommit, installedVersion: "1.0.0", installedCommit: testPreviousCommit},
		{name: "completed rollback", currentVersion: "1.0.0", currentCommit: testPreviousCommit, installedVersion: "1.0.0", installedCommit: testPreviousCommit, rollback: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			statePath := filepath.Join(dir, startupRecoveryStateName)
			previous := filepath.Join(dir, "detent.previous")
			if err := os.WriteFile(previous, []byte("rollback material"), 0o600); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
			pending := &PendingUpdate{FromVersion: "1.0.0", FromCommit: testPreviousCommit, ToVersion: "1.1.0", ToCommit: testUpdatedCommit, ExecutablePath: filepath.Join(dir, "detent"), PreviousBinaryPath: previous, AppliedAt: now}
			if tt.rollback {
				pending.RollbackRequestedAt = &now
			}
			initial := startupRecoveryState{PendingUpdate: pending, ActiveFailure: &StartupFailure{Version: "1.1.0", Count: 3, CrashLoop: true}, LastRecoveryUpdateAttemptVersion: "1.0.0"}
			if err := saveStartupRecoveryState(statePath, initial); err != nil {
				t.Fatal(err)
			}
			before := readTestStartupRecoveryState(t, statePath)
			for range 2 {
				recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
					StatePath: statePath, CurrentVersion: tt.currentVersion, CurrentCommit: tt.currentCommit,
					ExecutablePath: filepath.Join(dir, "another-copy"),
					BinaryVerifier: func(_ context.Context, path string) (string, error) {
						if path != pending.ExecutablePath {
							t.Fatalf("verified path = %q, want installation %q", path, pending.ExecutablePath)
						}
						if tt.readErr {
							return "", errors.New("unreadable installed binary")
						}
						return "Version: " + tt.installedVersion + "\nCommit: " + tt.installedCommit, nil
					},
				})
				err := recovery.MarkHealthy(t.Context())
				if (err != nil) != tt.wantErr {
					t.Fatalf("MarkHealthy() = %v, want error %v", err, tt.wantErr)
				}
				after := readTestStartupRecoveryState(t, statePath)
				if tt.wantErr {
					if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(recovery.state, before) {
						t.Fatalf("rejected identity changed recovery state: disk=%#v memory=%#v", after, recovery.state)
					}
					raw, err := os.ReadFile(previous)
					if err != nil || string(raw) != "rollback material" {
						t.Fatalf("rollback material = %q, %v", raw, err)
					}
					continue
				}
				if after.PendingUpdate != nil || after.ActiveFailure != nil || after.LastHealthyAt == nil {
					t.Fatalf("state did not resolve: %#v", after)
				}
				if (after.LastRollback != nil) != tt.rollback {
					t.Fatalf("rollback = %#v, requested=%v", after.LastRollback, tt.rollback)
				}
				if after.LastCrashLoop == nil || after.LastRecoveryUpdateAttemptVersion != before.LastRecoveryUpdateAttemptVersion {
					t.Fatalf("recovery history lost: %#v", after)
				}
				if _, err := os.Stat(previous); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rollback file still exists: %v", err)
				}
			}
		})
	}
}

func TestStartupRecoveryDefaultBinaryVerifier(t *testing.T) {
	t.Parallel()
	recovery := newTestStartupRecovery(t, StartupRecoveryConfig{StatePath: filepath.Join(t.TempDir(), "state"), CurrentVersion: "1.0.0", CurrentCommit: testPreviousCommit})
	recovery.state.PendingUpdate = &PendingUpdate{FromVersion: "1.0.0", FromCommit: testPreviousCommit, ToVersion: "1.1.0", ToCommit: testUpdatedCommit, ExecutablePath: filepath.Join(t.TempDir(), "missing")}
	if err := recovery.MarkHealthy(t.Context()); err == nil || !strings.Contains(err.Error(), "read binary identity") {
		t.Fatalf("MarkHealthy() = %v, want installed binary read failure", err)
	}
}
