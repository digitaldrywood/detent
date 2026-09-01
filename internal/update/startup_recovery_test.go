package update

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestStartupFailureBackoff(t *testing.T) {
	t.Parallel()

	tests := []struct {
		count int
		want  time.Duration
	}{
		{count: 0},
		{count: 1},
		{count: 2, want: 5 * time.Second},
		{count: 3, want: 30 * time.Second},
		{count: 4, want: 2 * time.Minute},
		{count: 5, want: 10 * time.Minute},
		{count: 6, want: 30 * time.Minute},
		{count: 100, want: 30 * time.Minute},
	}

	for _, tt := range tests {
		if got := startupFailureBackoff(tt.count); got != tt.want {
			t.Errorf("startupFailureBackoff(%d) = %s, want %s", tt.count, got, tt.want)
		}
	}
}

func TestStartupRecoveryPersistsCrashLoopAcrossProcesses(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), startupRecoveryStateName)
	startedAt := time.Date(2026, 8, 28, 17, 50, 0, 0, time.UTC)
	var waits []time.Duration
	var notifications []CrashLoopEvent

	for attempt := 1; attempt <= 4; attempt++ {
		attempt := attempt
		recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
			StatePath:      statePath,
			CurrentVersion: "0.93.0",
			Now: func() time.Time {
				return startedAt.Add(time.Duration(attempt-1) * time.Minute)
			},
			Wait: func(_ context.Context, delay time.Duration) bool {
				waits = append(waits, delay)
				return true
			},
			Notify: func(_ context.Context, event CrashLoopEvent) error {
				notifications = append(notifications, event)
				return nil
			},
		})
		recovery.HandleFailure(context.Background(), errors.New("validate current configuration"))
	}

	state := readTestStartupRecoveryState(t, statePath)
	if state.ActiveFailure == nil {
		t.Fatal("ActiveFailure = nil, want persisted startup failure")
	}
	failure := state.ActiveFailure
	if failure.Count != 4 || !failure.CrashLoop || !failure.Notified {
		t.Fatalf("ActiveFailure = %#v, want count 4 notified crash loop", failure)
	}
	if failure.Backoff != 2*time.Minute {
		t.Fatalf("Backoff = %s, want 2m", failure.Backoff)
	}
	if state.LastCrashLoop == nil || state.LastCrashLoop.Count != 4 {
		t.Fatalf("LastCrashLoop = %#v, want latest count 4", state.LastCrashLoop)
	}
	if !slices.Equal(waits, []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute}) {
		t.Fatalf("waits = %v, want escalating delays", waits)
	}
	if len(notifications) != 1 {
		t.Fatalf("notifications = %d, want 1", len(notifications))
	}
	event := notifications[0]
	if event.Event != "detent_startup_crash_loop" || event.Version != "0.93.0" || event.FailureCount != startupCrashLoopThreshold {
		t.Fatalf("notification = %#v, want third identical failure", event)
	}
	if event.StatePath != statePath || event.NextRetryAt == nil || !event.NextRetryAt.Equal(startedAt.Add(2*time.Minute+30*time.Second)) {
		t.Fatalf("notification retry evidence = %#v, want durable state path and next retry", event)
	}
}

func TestStartupRecoveryAttemptsOneUpdatePerVersion(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), startupRecoveryStateName)
	updater := &startupRecoveryUpdater{
		checkStatus: Status{
			UpdateAvailable: true,
			InstallSource:   InstallSourceRelease,
		},
		applyStatus: Status{Action: ActionUpToDate, Message: "already current"},
	}
	for range 2 {
		recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
			StatePath:      statePath,
			CurrentVersion: "0.93.0",
			AutoUpdate:     true,
			Updater:        updater,
		})
		recovery.HandleFailure(context.Background(), errors.New("startup failed"))
	}

	if updater.applyCalls != 1 {
		t.Fatalf("Apply() calls = %d, want 1", updater.applyCalls)
	}
	if updater.checkCalls != 1 {
		t.Fatalf("Check() calls = %d, want 1", updater.checkCalls)
	}
	state := readTestStartupRecoveryState(t, statePath)
	if state.LastRecoveryUpdateAttemptVersion != "0.93.0" {
		t.Fatalf("LastRecoveryUpdateAttemptVersion = %q, want 0.93.0", state.LastRecoveryUpdateAttemptVersion)
	}
	if state.ActiveFailure == nil || state.ActiveFailure.RecoveryAction != "update_unavailable" {
		t.Fatalf("ActiveFailure = %#v, want persisted unavailable update", state.ActiveFailure)
	}
}

func TestStartupRecoveryDoesNotMutateUnsafeInstallSource(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), startupRecoveryStateName)
	updater := &startupRecoveryUpdater{
		checkStatus: Status{
			UpdateAvailable: true,
			InstallSource:   InstallSourceGoInstall,
		},
	}
	recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
		StatePath:      statePath,
		CurrentVersion: "0.93.0",
		AutoUpdate:     true,
		Updater:        updater,
	})
	recovery.HandleFailure(context.Background(), errors.New("startup failed"))

	if updater.checkCalls != 1 || updater.applyCalls != 0 {
		t.Fatalf("updater calls = check %d apply %d, want check 1 apply 0", updater.checkCalls, updater.applyCalls)
	}
	state := readTestStartupRecoveryState(t, statePath)
	if state.ActiveFailure == nil || state.ActiveFailure.RecoveryAction != "update_requires_manual_install" {
		t.Fatalf("ActiveFailure = %#v, want manual install action", state.ActiveFailure)
	}
}

func TestStartupRecoveryRollsBackRepeatedlyFailingUpdate(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), startupRecoveryStateName)
	pending := PendingUpdate{
		FromVersion:        "0.93.0",
		ToVersion:          "0.94.0",
		InstallSource:      InstallSourceRelease,
		ExecutablePath:     "/opt/detent/bin/detent",
		PreviousBinaryPath: "/opt/detent/bin/detent.previous",
		AppliedAt:          time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC),
	}
	if err := recordPendingUpdate(statePath, pending); err != nil {
		t.Fatalf("recordPendingUpdate() error = %v", err)
	}
	rollbackCalls := 0
	for range startupCrashLoopThreshold {
		recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
			StatePath:      statePath,
			CurrentVersion: pending.ToVersion,
			GOOS:           "linux",
			Rollback: func(_ context.Context, got PendingUpdate) error {
				rollbackCalls++
				if got != pending {
					t.Fatalf("rollback pending = %#v, want %#v", got, pending)
				}
				return nil
			},
		})
		recovery.HandleFailure(context.Background(), errors.New("candidate cannot start"))
	}

	if rollbackCalls != 1 {
		t.Fatalf("rollback calls = %d, want 1", rollbackCalls)
	}
	state := readTestStartupRecoveryState(t, statePath)
	if state.PendingUpdate != nil {
		t.Fatalf("PendingUpdate = %#v, want cleared after rollback", state.PendingUpdate)
	}
	if state.LastRollback == nil || state.LastRollback.FromVersion != pending.ToVersion || state.LastRollback.ToVersion != pending.FromVersion {
		t.Fatalf("LastRollback = %#v, want 0.94.0 to 0.93.0", state.LastRollback)
	}
	if state.ActiveFailure == nil || state.ActiveFailure.RecoveryAction != "rolled_back_update" {
		t.Fatalf("ActiveFailure = %#v, want rolled back action", state.ActiveFailure)
	}
}

func TestStartupRecoveryRestoresInstallLockAfterRollback(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		previousLock string
		wantLock     string
		wantLockFile bool
	}{
		{name: "removes release lock added over go install"},
		{name: "restores prior lock contents", previousLock: "binary=/opt/other/detent\nversion=0.90.0\n", wantLock: "binary=/opt/other/detent\nversion=0.90.0\n", wantLockFile: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			goBin := filepath.Join(dir, "go", "bin")
			if err := os.MkdirAll(goBin, 0o755); err != nil {
				t.Fatalf("MkdirAll(go bin) error = %v", err)
			}
			executable := filepath.Join(goBin, "detent")
			previous := PreviousBinaryPath(executable)
			lockPath := filepath.Join(dir, "state", "install.lock")
			if err := os.WriteFile(executable, []byte("candidate"), 0o755); err != nil {
				t.Fatalf("WriteFile(executable) error = %v", err)
			}
			if err := os.WriteFile(previous, []byte("previous"), 0o755); err != nil {
				t.Fatalf("WriteFile(previous) error = %v", err)
			}
			if err := writeInstallLock(lockPath, executable, "0.94.0", time.Now()); err != nil {
				t.Fatalf("writeInstallLock() error = %v", err)
			}

			statePath := filepath.Join(dir, startupRecoveryStateName)
			pending := PendingUpdate{
				FromVersion:              "0.93.0",
				ToVersion:                "0.94.0",
				InstallSource:            InstallSourceGoInstall,
				ExecutablePath:           executable,
				PreviousBinaryPath:       previous,
				InstallLockPath:          lockPath,
				PreviousInstallLock:      tt.previousLock,
				PreviousInstallLockFound: tt.wantLockFile,
				AppliedAt:                time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC),
			}
			if err := recordPendingUpdate(statePath, pending); err != nil {
				t.Fatalf("recordPendingUpdate() error = %v", err)
			}
			for range startupCrashLoopThreshold {
				recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
					StatePath:      statePath,
					CurrentVersion: pending.ToVersion,
					ExecutablePath: executable,
					GOOS:           "linux",
					HomeDir:        dir,
					Env: map[string]string{
						"GOBIN":               goBin,
						"DETENT_INSTALL_LOCK": lockPath,
					},
				})
				recovery.HandleFailure(context.Background(), errors.New("candidate cannot serve"))
			}

			raw, err := os.ReadFile(executable)
			if err != nil {
				t.Fatalf("ReadFile(executable) error = %v", err)
			}
			if string(raw) != "previous" {
				t.Fatalf("executable = %q, want previous", raw)
			}
			lockRaw, err := os.ReadFile(lockPath)
			if tt.wantLockFile {
				if err != nil {
					t.Fatalf("ReadFile(lock) error = %v", err)
				}
				if string(lockRaw) != tt.wantLock {
					t.Fatalf("install lock = %q, want %q", lockRaw, tt.wantLock)
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ReadFile(lock) error = %v, want not exist", err)
			}
			if got := DetectInstallSource(DetectionOptions{
				CurrentVersion: "0.93.0",
				ExecutablePath: executable,
				GOOS:           "linux",
				HomeDir:        dir,
				Env: map[string]string{
					"GOBIN":               goBin,
					"DETENT_INSTALL_LOCK": lockPath,
				},
			}).Source; got != InstallSourceGoInstall {
				t.Fatalf("install source after rollback = %q, want %q", got, InstallSourceGoInstall)
			}
		})
	}
}

func TestStartupRecoveryMarkHealthyCommitsPendingUpdate(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	statePath := filepath.Join(dir, startupRecoveryStateName)
	previousPath := filepath.Join(dir, "detent.previous")
	if err := os.WriteFile(previousPath, []byte("previous"), 0o700); err != nil {
		t.Fatalf("WriteFile(previous) error = %v", err)
	}
	pending := PendingUpdate{
		FromVersion:        "0.93.0",
		ToVersion:          "0.94.0",
		InstallSource:      InstallSourceRelease,
		ExecutablePath:     filepath.Join(dir, "detent"),
		PreviousBinaryPath: previousPath,
		AppliedAt:          time.Date(2026, 8, 29, 1, 0, 0, 0, time.UTC),
	}
	if err := recordPendingUpdate(statePath, pending); err != nil {
		t.Fatalf("recordPendingUpdate() error = %v", err)
	}

	healthyAt := pending.AppliedAt.Add(time.Minute)
	recovery := newTestStartupRecovery(t, StartupRecoveryConfig{
		StatePath:      statePath,
		CurrentVersion: pending.ToVersion,
		Now:            func() time.Time { return healthyAt },
	})
	if err := recovery.MarkHealthy(context.Background()); err != nil {
		t.Fatalf("MarkHealthy() error = %v", err)
	}

	if _, err := os.Stat(previousPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(previous) error = %v, want not exist", err)
	}
	state := readTestStartupRecoveryState(t, statePath)
	if state.PendingUpdate != nil || state.ActiveFailure != nil {
		t.Fatalf("state = %#v, want healthy state without pending update or active failure", state)
	}
	if state.LastHealthyAt == nil || !state.LastHealthyAt.Equal(healthyAt) {
		t.Fatalf("LastHealthyAt = %v, want %v", state.LastHealthyAt, healthyAt)
	}
}

func newTestStartupRecovery(t *testing.T, cfg StartupRecoveryConfig) *StartupRecovery {
	t.Helper()
	if cfg.ExecutablePath == "" {
		cfg.ExecutablePath = "/opt/detent/bin/detent"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Date(2026, 8, 28, 17, 50, 0, 0, time.UTC) }
	}
	if cfg.Wait == nil {
		cfg.Wait = func(context.Context, time.Duration) bool { return true }
	}
	recovery, err := NewStartupRecovery(cfg)
	if err != nil {
		t.Fatalf("NewStartupRecovery() error = %v", err)
	}
	return recovery
}

func readTestStartupRecoveryState(t *testing.T, path string) startupRecoveryState {
	t.Helper()
	state, found, err := loadStartupRecoveryState(path)
	if err != nil {
		t.Fatalf("loadStartupRecoveryState() error = %v", err)
	}
	if !found {
		t.Fatal("loadStartupRecoveryState() found = false, want true")
	}
	return state
}

type startupRecoveryUpdater struct {
	checkStatus Status
	checkErr    error
	checkCalls  int
	applyStatus Status
	applyErr    error
	applyCalls  int
}

func (u *startupRecoveryUpdater) Check(context.Context) (Status, error) {
	u.checkCalls++
	return u.checkStatus, u.checkErr
}

func (u *startupRecoveryUpdater) Apply(context.Context, ApplyOptions) (Status, error) {
	u.applyCalls++
	return u.applyStatus, u.applyErr
}
