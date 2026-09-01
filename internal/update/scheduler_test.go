package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSchedulerDetectsFixtureReleaseWithoutApplying(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/releases" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode([]Release{{TagName: "v1.2.4"}}); err != nil {
			t.Errorf("Encode() error = %v", err)
		}
	}))
	t.Cleanup(server.Close)

	executable := filepath.Join(t.TempDir(), "detent")
	service := NewService(Config{
		CurrentVersion: "1.2.3",
		ExecutablePath: executable,
		GOOS:           "linux",
		GOARCH:         "amd64",
		Client: NewGitHubClient(GitHubClientConfig{
			APIBase: server.URL,
		}),
	})
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: time.Hour,
		Updater:       service,
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	status, err := scheduler.CheckNow(context.Background())
	if err != nil {
		t.Fatalf("CheckNow() error = %v", err)
	}
	if !status.UpdateAvailable || status.LatestVersion != "1.2.4" {
		t.Fatalf("CheckNow() = %#v, want available 1.2.4", status)
	}
	if got := scheduler.Status(); got.State != "available" || got.AvailableVersion != "1.2.4" || got.LastCheckAt == nil {
		t.Fatalf("Status() = %#v, want available fixture release", got)
	}
	if _, err := os.Stat(executable); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("automatic notification modified executable: Stat() error = %v", err)
	}
}

func TestSchedulerAutoApplyBehavior(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		autoApply          bool
		restartAccepted    bool
		wantApplyCalls     int
		wantRestartCalls   int
		wantReleaseCalls   int
		wantState          string
		wantAppliedVersion string
		installSource      InstallSource
	}{
		{
			name:          "notification only",
			wantState:     "available",
			installSource: InstallSourceRelease,
		},
		{
			name:               "apply and request restart",
			autoApply:          true,
			restartAccepted:    true,
			wantApplyCalls:     1,
			wantRestartCalls:   1,
			wantState:          "restart_requested",
			wantAppliedVersion: "1.2.4",
			installSource:      InstallSourceRelease,
		},
		{
			name:               "apply while another drain owns restart",
			autoApply:          true,
			wantApplyCalls:     1,
			wantRestartCalls:   1,
			wantReleaseCalls:   1,
			wantState:          "applied_restart_deferred",
			wantAppliedVersion: "1.2.4",
			installSource:      InstallSourceRelease,
		},
		{
			name:          "non-release install never applies automatically",
			autoApply:     true,
			wantState:     "available_manual_apply",
			installSource: InstallSourceGoInstall,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			updater := &schedulerUpdaterStub{
				checkStatus: Status{
					CurrentVersion:  "1.2.3",
					LatestVersion:   "1.2.4",
					UpdateAvailable: true,
					Action:          ActionAvailable,
					InstallSource:   tt.installSource,
				},
				applyStatus: Status{
					CurrentVersion: "1.2.3",
					LatestVersion:  "1.2.4",
					Action:         ActionUpdated,
					Binary:         "/opt/detent/bin/detent",
				},
			}
			restartCalls := 0
			releaseCalls := 0
			scheduler, err := NewScheduler(SchedulerConfig{
				Enabled:          true,
				AutoApplyEnabled: tt.autoApply,
				CheckInterval:    time.Hour,
				Updater:          updater,
				ReserveIdle: func(context.Context) (func(), bool) {
					return func() { releaseCalls++ }, true
				},
				ReserveDrain: schedulerDrainReservation,
				RequestRestart: func(binary string) bool {
					restartCalls++
					if binary != "/opt/detent/bin/detent" {
						t.Errorf("restart binary = %q", binary)
					}
					return tt.restartAccepted
				},
			})
			if err != nil {
				t.Fatalf("NewScheduler() error = %v", err)
			}

			if _, err := scheduler.CheckNow(context.Background()); err != nil {
				t.Fatalf("CheckNow() error = %v", err)
			}
			if updater.applyCalls != tt.wantApplyCalls {
				t.Fatalf("Apply() calls = %d, want %d", updater.applyCalls, tt.wantApplyCalls)
			}
			if restartCalls != tt.wantRestartCalls {
				t.Fatalf("restart calls = %d, want %d", restartCalls, tt.wantRestartCalls)
			}
			if releaseCalls != tt.wantReleaseCalls {
				t.Fatalf("idle reservation releases = %d, want %d", releaseCalls, tt.wantReleaseCalls)
			}
			got := scheduler.Status()
			if got.State != tt.wantState || got.LastAppliedVersion != tt.wantAppliedVersion {
				t.Fatalf("Status() = %#v, want state %q applied %q", got, tt.wantState, tt.wantAppliedVersion)
			}
		})
	}
}

func TestSchedulerPassesUpdateSafetyOptions(t *testing.T) {
	t.Parallel()

	updater := &schedulerUpdaterStub{
		checkStatus: Status{
			CurrentVersion:  "1.2.3",
			LatestVersion:   "1.2.4",
			UpdateAvailable: true,
			Action:          ActionAvailable,
			InstallSource:   InstallSourceRelease,
		},
		applyStatus: Status{
			CurrentVersion: "1.2.3",
			LatestVersion:  "1.2.4",
			Action:         ActionUpdated,
		},
	}
	preflight := func(context.Context, string) error { return nil }
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:          true,
		AutoApplyEnabled: true,
		CheckInterval:    time.Hour,
		Updater:          updater,
		ReserveIdle:      func(context.Context) (func(), bool) { return func() {}, true },
		ReserveDrain:     schedulerDrainReservation,
		RequestRestart:   func(string) bool { return true },
		ApplyOptions: ApplyOptions{
			Preflight:         preflight,
			RecoveryStatePath: "/var/lib/detent/startup-recovery.json",
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	if _, err := scheduler.CheckNow(context.Background()); err != nil {
		t.Fatalf("CheckNow() error = %v", err)
	}

	if len(updater.applyOptions) != 1 {
		t.Fatalf("Apply() options = %d, want 1", len(updater.applyOptions))
	}
	got := updater.applyOptions[0]
	if !got.AssumeYes || got.Preflight == nil || got.RecoveryStatePath != "/var/lib/detent/startup-recovery.json" {
		t.Fatalf("ApplyOptions = %#v, want preflight and durable recovery state", got)
	}
}

func TestSchedulerDefersAutoApplyUntilIdle(t *testing.T) {
	t.Parallel()

	updater := &schedulerUpdaterStub{
		checkStatus: Status{
			CurrentVersion:  "1.2.3",
			LatestVersion:   "1.2.4",
			UpdateAvailable: true,
			Action:          ActionAvailable,
			InstallSource:   InstallSourceRelease,
		},
		applyStatus: Status{
			CurrentVersion: "1.2.3",
			LatestVersion:  "1.2.4",
			Action:         ActionUpdated,
			Binary:         "/opt/detent/bin/detent",
		},
	}
	idle := false
	reservationHeld := false
	restartCalls := 0
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:          true,
		AutoApplyEnabled: true,
		CheckInterval:    time.Hour,
		Updater:          updater,
		ReserveIdle: func(context.Context) (func(), bool) {
			if !idle {
				return nil, false
			}
			reservationHeld = true
			return func() { reservationHeld = false }, true
		},
		ReserveDrain: schedulerDrainReservation,
		RequestRestart: func(string) bool {
			if !reservationHeld {
				t.Error("idle reservation released before restart request")
			}
			restartCalls++
			return true
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	if _, err := scheduler.CheckNow(context.Background()); err != nil {
		t.Fatalf("CheckNow() error = %v", err)
	}
	if updater.applyCalls != 0 || restartCalls != 0 {
		t.Fatalf("busy runtime calls: Apply=%d Restart=%d, want zero", updater.applyCalls, restartCalls)
	}
	if got := scheduler.Status(); got.State != "pending_idle" || got.AvailableVersion != "1.2.4" {
		t.Fatalf("Status() = %#v, want pending 1.2.4", got)
	}

	if _, err := scheduler.applyWhenIdle(context.Background()); err != nil {
		t.Fatalf("applyWhenIdle() busy error = %v", err)
	}
	if updater.applyCalls != 0 {
		t.Fatalf("Apply() calls while busy = %d, want 0", updater.applyCalls)
	}

	idle = true
	if _, err := scheduler.applyWhenIdle(context.Background()); err != nil {
		t.Fatalf("applyWhenIdle() idle error = %v", err)
	}
	if updater.applyCalls != 1 || restartCalls != 1 {
		t.Fatalf("idle runtime calls: Apply=%d Restart=%d, want 1 each", updater.applyCalls, restartCalls)
	}
	if got := scheduler.Status(); got.State != "restart_requested" || got.AvailableVersion != "" {
		t.Fatalf("Status() = %#v, want restart requested", got)
	}
	if !reservationHeld {
		t.Fatal("idle reservation released before shutdown owns dispatch")
	}
}

func TestSchedulerDrainPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name              string
		critical          bool
		advance           time.Duration
		wantPendingBefore bool
		wantIdleCalls     int
	}{
		{name: "busy runtime force drains at cap", advance: 2 * time.Hour, wantPendingBefore: true, wantIdleCalls: 1},
		{name: "critical release drains immediately", critical: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
			updater := &schedulerUpdaterStub{
				checkStatus: Status{
					CurrentVersion:  "1.2.3",
					LatestVersion:   "1.2.4",
					UpdateAvailable: true,
					InstallSource:   InstallSourceRelease,
					Action:          ActionAvailable,
					Critical:        tt.critical,
				},
				applyStatus: Status{
					LatestVersion: "1.2.4",
					Action:        ActionUpdated,
					Binary:        "/opt/detent/bin/detent",
				},
			}
			idleCalls := 0
			drainCalls := 0
			drainReserved := false
			scheduler, err := NewScheduler(SchedulerConfig{
				Enabled:          true,
				AutoApplyEnabled: true,
				CheckInterval:    time.Hour,
				MaxDeferral:      2 * time.Hour,
				Updater:          updater,
				Now:              func() time.Time { return now },
				ReserveIdle: func(context.Context) (func(), bool) {
					idleCalls++
					return nil, false
				},
				ReserveDrain: func(context.Context) (func(), error) {
					drainCalls++
					drainReserved = true
					return func() { drainReserved = false }, nil
				},
				RequestRestart: func(string) bool {
					if !drainReserved {
						t.Error("restart requested before runtime drain reservation")
					}
					return true
				},
			})
			if err != nil {
				t.Fatalf("NewScheduler() error = %v", err)
			}

			if _, err := scheduler.CheckNow(context.Background()); err != nil {
				t.Fatalf("CheckNow() error = %v", err)
			}
			if tt.wantPendingBefore {
				status := scheduler.Status()
				if status.State != "pending_idle" || status.PendingSince == nil || !status.PendingSince.Equal(now) {
					t.Fatalf("Status() before cap = %#v, want pending since %s", status, now)
				}
				now = now.Add(tt.advance)
				if _, err := scheduler.applyWhenIdle(context.Background()); err != nil {
					t.Fatalf("applyWhenIdle() error = %v", err)
				}
			}

			if idleCalls != tt.wantIdleCalls || drainCalls != 1 || updater.applyCalls != 1 {
				t.Fatalf("calls: Idle=%d Drain=%d Apply=%d, want %d/1/1", idleCalls, drainCalls, updater.applyCalls, tt.wantIdleCalls)
			}
			if got := scheduler.Status(); got.State != "restart_requested" || got.PendingSince != nil || got.Critical {
				t.Fatalf("Status() = %#v, want cleared restart request", got)
			}
			if !drainReserved {
				t.Fatal("drain reservation released before restart owns dispatch")
			}
		})
	}
}

func TestSchedulerApplyPendingBypassesIdleWait(t *testing.T) {
	t.Parallel()

	updater := &schedulerUpdaterStub{
		checkStatus: Status{
			CurrentVersion:  "1.2.3",
			LatestVersion:   "1.2.4",
			UpdateAvailable: true,
			Action:          ActionAvailable,
			InstallSource:   InstallSourceRelease,
		},
		applyStatus: Status{
			LatestVersion: "1.2.4",
			Action:        ActionUpdated,
			Binary:        "/opt/detent/bin/detent",
		},
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:          true,
		AutoApplyEnabled: true,
		CheckInterval:    time.Hour,
		Updater:          updater,
		ReserveIdle: func(context.Context) (func(), bool) {
			return nil, false
		},
		ReserveDrain:   schedulerDrainReservation,
		RequestRestart: func(string) bool { return true },
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	if _, err := scheduler.ApplyPending(context.Background()); !errors.Is(err, ErrNoPendingUpdate) {
		t.Fatalf("ApplyPending() before check error = %v, want %v", err, ErrNoPendingUpdate)
	}
	if _, err := scheduler.CheckNow(context.Background()); err != nil {
		t.Fatalf("CheckNow() error = %v", err)
	}
	if _, err := scheduler.ApplyPending(context.Background()); err != nil {
		t.Fatalf("ApplyPending() error = %v", err)
	}
	if updater.applyCalls != 1 {
		t.Fatalf("Apply() calls = %d, want 1", updater.applyCalls)
	}
}

func TestSchedulerReleasesIdleReservationWhenApplyFails(t *testing.T) {
	t.Parallel()

	applyErr := errors.New("fixture apply failed")
	updater := &schedulerUpdaterStub{
		checkStatus: Status{
			LatestVersion:   "1.2.4",
			UpdateAvailable: true,
			Action:          ActionAvailable,
			InstallSource:   InstallSourceRelease,
		},
		applyErr: applyErr,
	}
	releaseCalls := 0
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:          true,
		AutoApplyEnabled: true,
		CheckInterval:    time.Hour,
		Updater:          updater,
		ReserveIdle: func(context.Context) (func(), bool) {
			return func() { releaseCalls++ }, true
		},
		ReserveDrain: schedulerDrainReservation,
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	if _, err := scheduler.CheckNow(context.Background()); !errors.Is(err, applyErr) {
		t.Fatalf("CheckNow() error = %v, want %v", err, applyErr)
	}
	if releaseCalls != 1 {
		t.Fatalf("idle reservation releases = %d, want 1", releaseCalls)
	}
}

func TestSchedulerAutoApplyRequiresIdleReservation(t *testing.T) {
	t.Parallel()

	_, err := NewScheduler(SchedulerConfig{
		Enabled:          true,
		AutoApplyEnabled: true,
		CheckInterval:    time.Hour,
		Updater:          &schedulerUpdaterStub{},
	})
	if err == nil || !strings.Contains(err.Error(), "idle reservation") {
		t.Fatalf("NewScheduler() error = %v, want idle reservation requirement", err)
	}
}

func TestSchedulerAutoApplyRequiresDrainReservation(t *testing.T) {
	t.Parallel()

	_, err := NewScheduler(SchedulerConfig{
		Enabled:          true,
		AutoApplyEnabled: true,
		CheckInterval:    time.Hour,
		Updater:          &schedulerUpdaterStub{},
		ReserveIdle: func(context.Context) (func(), bool) {
			return nil, false
		},
	})
	if err == nil || !strings.Contains(err.Error(), "drain reservation") {
		t.Fatalf("NewScheduler() error = %v, want drain reservation requirement", err)
	}
}

func TestSchedulerRunPollsPendingUpdateUntilIdle(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	updater := &schedulerUpdaterStub{
		checkStatus: Status{
			CurrentVersion:  "1.2.3",
			LatestVersion:   "1.2.4",
			UpdateAvailable: true,
			Action:          ActionAvailable,
			InstallSource:   InstallSourceRelease,
		},
		applyStatus: Status{LatestVersion: "1.2.4", Action: ActionUpdated, Binary: "/opt/detent/bin/detent"},
	}
	idle := false
	var delays []time.Duration
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:          true,
		AutoApplyEnabled: true,
		CheckInterval:    time.Hour,
		IdlePollInterval: 2 * time.Second,
		Updater:          updater,
		ReserveIdle: func(context.Context) (func(), bool) {
			return func() {}, idle
		},
		ReserveDrain:   schedulerDrainReservation,
		RequestRestart: func(string) bool { return true },
		NextDelay:      func(interval time.Duration) time.Duration { return interval },
		Now: func() time.Time {
			return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
		},
		Wait: func(_ context.Context, delay time.Duration) bool {
			delays = append(delays, delay)
			switch len(delays) {
			case 1:
				return true
			case 2:
				idle = true
				return true
			default:
				cancel()
				return false
			}
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	scheduler.Run(ctx)
	if updater.checkCalls != 1 || updater.applyCalls != 1 {
		t.Fatalf("Run() calls: Check=%d Apply=%d, want 1 each", updater.checkCalls, updater.applyCalls)
	}
	if len(delays) != 3 || delays[0] != time.Hour || delays[1] != 2*time.Second || delays[2] != time.Hour {
		t.Fatalf("wait delays = %v, want [1h 2s 1h]", delays)
	}
}

func TestSchedulerRunChecksOnIntervalWhilePendingIdle(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	updater := &schedulerUpdaterStub{
		checkStatus: Status{
			CurrentVersion:  "1.2.3",
			LatestVersion:   "1.2.4",
			UpdateAvailable: true,
			Action:          ActionAvailable,
			InstallSource:   InstallSourceRelease,
		},
	}
	waits := 0
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:          true,
		AutoApplyEnabled: true,
		CheckInterval:    time.Hour,
		IdlePollInterval: 30 * time.Minute,
		Updater:          updater,
		ReserveIdle: func(context.Context) (func(), bool) {
			return nil, false
		},
		ReserveDrain: schedulerDrainReservation,
		Now:          func() time.Time { return now },
		NextDelay:    func(interval time.Duration) time.Duration { return interval },
		Wait: func(_ context.Context, delay time.Duration) bool {
			now = now.Add(delay)
			waits++
			if waits == 4 {
				cancel()
				return false
			}
			return true
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	scheduler.Run(ctx)
	if updater.checkCalls != 2 {
		t.Fatalf("Check() calls = %d, want 2 while runtime remains busy", updater.checkCalls)
	}
}

func TestSchedulerRunUsesPeriodicWaitAndStopsWithContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	updater := &schedulerUpdaterStub{checkStatus: Status{CurrentVersion: "1.2.3", LatestVersion: "1.2.3"}}
	var mu sync.Mutex
	var delays []time.Duration
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: 4 * time.Hour,
		Updater:       updater,
		Now: func() time.Time {
			return time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
		},
		NextDelay: func(interval time.Duration) time.Duration {
			return interval - time.Hour
		},
		Wait: func(_ context.Context, delay time.Duration) bool {
			mu.Lock()
			delays = append(delays, delay)
			calls := len(delays)
			mu.Unlock()
			if calls == 1 {
				return true
			}
			cancel()
			return false
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	scheduler.Run(ctx)
	if updater.checkCalls != 1 {
		t.Fatalf("Check() calls = %d, want 1", updater.checkCalls)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(delays) != 2 || delays[0] != 3*time.Hour || delays[1] != 3*time.Hour {
		t.Fatalf("wait delays = %v, want two 3h delays", delays)
	}
}

func TestSchedulerRestartMidIntervalPreservesRemainingDelay(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "update-state.json")
	lastCheckAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	seedSchedulerLastCheck(t, statePath, lastCheckAt)

	now := lastCheckAt.Add(time.Hour)
	var delays []time.Duration
	updater := &schedulerUpdaterStub{}
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: 4 * time.Hour,
		StatePath:     statePath,
		Updater:       updater,
		Now:           func() time.Time { return now },
		NextDelay:     func(time.Duration) time.Duration { return 3 * time.Hour },
		Wait: func(_ context.Context, delay time.Duration) bool {
			delays = append(delays, delay)
			return false
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	scheduler.Run(context.Background())
	if updater.checkCalls != 0 {
		t.Fatalf("Check() calls = %d, want 0 before persisted interval elapses", updater.checkCalls)
	}
	if len(delays) != 1 || delays[0] != 2*time.Hour {
		t.Fatalf("wait delays = %v, want [2h]", delays)
	}
	status := scheduler.Status()
	if status.LastCheckAt == nil || !status.LastCheckAt.Equal(lastCheckAt) {
		t.Fatalf("LastCheckAt = %v, want %v", status.LastCheckAt, lastCheckAt)
	}
	wantNext := lastCheckAt.Add(3 * time.Hour)
	if status.NextCheckAt == nil || !status.NextCheckAt.Equal(wantNext) {
		t.Fatalf("NextCheckAt = %v, want %v", status.NextCheckAt, wantNext)
	}
}

func TestSchedulerRestartAfterIntervalChecksBeforeWaiting(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "update-state.json")
	lastCheckAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	seedSchedulerLastCheck(t, statePath, lastCheckAt)

	now := lastCheckAt.Add(4 * time.Hour)
	updater := &schedulerUpdaterStub{}
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: 4 * time.Hour,
		StatePath:     statePath,
		Updater:       updater,
		Now:           func() time.Time { return now },
		NextDelay:     func(time.Duration) time.Duration { return 3 * time.Hour },
		Wait: func(_ context.Context, delay time.Duration) bool {
			if updater.checkCalls != 1 {
				t.Fatalf("Check() calls before first wait = %d, want 1", updater.checkCalls)
			}
			if delay != 3*time.Hour {
				t.Fatalf("wait delay after prompt check = %v, want 3h", delay)
			}
			return false
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	scheduler.Run(context.Background())
	if updater.checkCalls != 1 {
		t.Fatalf("Check() calls = %d, want 1", updater.checkCalls)
	}

	restarted, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: 4 * time.Hour,
		StatePath:     statePath,
		Updater:       &schedulerUpdaterStub{},
	})
	if err != nil {
		t.Fatalf("NewScheduler() reload error = %v", err)
	}
	if got := restarted.Status().LastCheckAt; got == nil || !got.Equal(now) {
		t.Fatalf("reloaded LastCheckAt = %v, want %v", got, now)
	}
}

func TestSchedulerRestartPreservesPendingDeferral(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		critical         bool
		initialDrainFail bool
		restartAdvance   time.Duration
		wantIdleCalls    int
	}{
		{name: "busy runtime keeps original deadline", restartAdvance: 2 * time.Hour},
		{name: "critical release retries drain immediately", critical: true, initialDrainFail: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			statePath := filepath.Join(t.TempDir(), "update-state.json")
			pendingSince := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
			now := pendingSince
			initialUpdater := &schedulerUpdaterStub{checkStatus: Status{
				CurrentVersion:  "1.2.3",
				LatestVersion:   "1.2.4",
				UpdateAvailable: true,
				InstallSource:   InstallSourceRelease,
				Critical:        tt.critical,
			}}
			initial, err := NewScheduler(SchedulerConfig{
				Enabled:          true,
				AutoApplyEnabled: true,
				CheckInterval:    time.Hour,
				MaxDeferral:      2 * time.Hour,
				StatePath:        statePath,
				Updater:          initialUpdater,
				Now:              func() time.Time { return now },
				ReserveIdle:      func(context.Context) (func(), bool) { return nil, false },
				ReserveDrain: func(context.Context) (func(), error) {
					if tt.initialDrainFail {
						return nil, errors.New("drain unavailable")
					}
					return schedulerDrainReservation(context.Background())
				},
			})
			if err != nil {
				t.Fatalf("NewScheduler() initial error = %v", err)
			}
			_, checkErr := initial.CheckNow(context.Background())
			if tt.initialDrainFail && checkErr == nil {
				t.Fatal("CheckNow() error = nil, want initial drain failure")
			}
			if !tt.initialDrainFail && checkErr != nil {
				t.Fatalf("CheckNow() error = %v", checkErr)
			}

			now = pendingSince.Add(tt.restartAdvance)
			idleCalls := 0
			drainCalls := 0
			updater := &schedulerUpdaterStub{applyStatus: Status{
				LatestVersion: "1.2.4",
				Action:        ActionUpdated,
			}}
			restarted, err := NewScheduler(SchedulerConfig{
				Enabled:          true,
				AutoApplyEnabled: true,
				CheckInterval:    time.Hour,
				MaxDeferral:      2 * time.Hour,
				StatePath:        statePath,
				Updater:          updater,
				Now:              func() time.Time { return now },
				ReserveIdle: func(context.Context) (func(), bool) {
					idleCalls++
					return nil, false
				},
				ReserveDrain: func(context.Context) (func(), error) {
					drainCalls++
					return func() {}, nil
				},
			})
			if err != nil {
				t.Fatalf("NewScheduler() restart error = %v", err)
			}
			status := restarted.Status()
			if status.State != "pending_idle" || status.AvailableVersion != "1.2.4" || status.PendingSince == nil || !status.PendingSince.Equal(pendingSince) || status.Critical != tt.critical {
				t.Fatalf("Status() = %#v, want restored pending release", status)
			}
			if _, err := restarted.applyWhenIdle(context.Background()); err != nil {
				t.Fatalf("applyWhenIdle() error = %v", err)
			}
			if idleCalls != tt.wantIdleCalls || drainCalls != 1 || updater.applyCalls != 1 {
				t.Fatalf("calls: Idle=%d Drain=%d Apply=%d, want %d/1/1", idleCalls, drainCalls, updater.applyCalls, tt.wantIdleCalls)
			}
		})
	}
}

func TestSchedulerRepeatedRapidRestartsCannotStarveCheck(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "update-state.json")
	lastCheckAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	seedSchedulerLastCheck(t, statePath, lastCheckAt)

	for restart, wantDelay := range []time.Duration{2 * time.Hour, time.Hour} {
		now := lastCheckAt.Add(time.Duration(restart+1) * time.Hour)
		updater := &schedulerUpdaterStub{}
		scheduler, err := NewScheduler(SchedulerConfig{
			Enabled:       true,
			CheckInterval: 4 * time.Hour,
			StatePath:     statePath,
			Updater:       updater,
			Now:           func() time.Time { return now },
			NextDelay:     func(time.Duration) time.Duration { return 3 * time.Hour },
			Wait: func(_ context.Context, delay time.Duration) bool {
				if delay != wantDelay {
					t.Fatalf("restart %d delay = %v, want %v", restart+1, delay, wantDelay)
				}
				return false
			},
		})
		if err != nil {
			t.Fatalf("NewScheduler() restart %d error = %v", restart+1, err)
		}
		scheduler.Run(context.Background())
		if updater.checkCalls != 0 {
			t.Fatalf("restart %d Check() calls = %d, want 0", restart+1, updater.checkCalls)
		}
	}

	now := lastCheckAt.Add(3 * time.Hour)
	updater := &schedulerUpdaterStub{}
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: 4 * time.Hour,
		StatePath:     statePath,
		Updater:       updater,
		Now:           func() time.Time { return now },
		NextDelay:     func(time.Duration) time.Duration { return 3 * time.Hour },
		Wait: func(_ context.Context, delay time.Duration) bool {
			if updater.checkCalls != 1 {
				t.Fatalf("Check() calls before wait = %d, want 1", updater.checkCalls)
			}
			return false
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler() due restart error = %v", err)
	}
	scheduler.Run(context.Background())
	if updater.checkCalls != 1 {
		t.Fatalf("due restart Check() calls = %d, want 1", updater.checkCalls)
	}
}

func TestSchedulerRecordsCheckFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("fixture unavailable")
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: time.Hour,
		Updater:       &schedulerUpdaterStub{checkErr: wantErr},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	if _, err := scheduler.CheckNow(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("CheckNow() error = %v, want %v", err, wantErr)
	}
	if got := scheduler.Status(); got.State != "failed" || got.LastError != wantErr.Error() || got.LastCheckAt == nil {
		t.Fatalf("Status() = %#v, want recorded failure", got)
	}
}

func TestSchedulerCanceledCheckPreservesLastCheck(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "update-state.json")
	lastCheckAt := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	seedSchedulerLastCheck(t, statePath, lastCheckAt)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: 4 * time.Hour,
		StatePath:     statePath,
		Updater:       &schedulerUpdaterStub{checkErr: context.Canceled},
		Now:           func() time.Time { return lastCheckAt.Add(5 * time.Hour) },
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	if _, err := scheduler.CheckNow(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckNow() error = %v, want context cancellation", err)
	}
	if got := scheduler.Status().LastCheckAt; got == nil || !got.Equal(lastCheckAt) {
		t.Fatalf("LastCheckAt = %v, want preserved %v", got, lastCheckAt)
	}

	restarted, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: 4 * time.Hour,
		StatePath:     statePath,
		Updater:       &schedulerUpdaterStub{},
	})
	if err != nil {
		t.Fatalf("NewScheduler() reload error = %v", err)
	}
	if got := restarted.Status().LastCheckAt; got == nil || !got.Equal(lastCheckAt) {
		t.Fatalf("reloaded LastCheckAt = %v, want preserved %v", got, lastCheckAt)
	}
}

func TestSchedulerReportsStatePersistenceFailure(t *testing.T) {
	t.Parallel()

	statePath := filepath.Join(t.TempDir(), "update-state.json")
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: time.Hour,
		StatePath:     statePath,
		Updater:       &schedulerUpdaterStub{},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}

	if _, err := scheduler.CheckNow(context.Background()); err == nil || !strings.Contains(err.Error(), "persist automatic update state") {
		t.Fatalf("CheckNow() error = %v, want persistence failure", err)
	}
	if got := scheduler.Status(); !strings.Contains(got.LastError, "persist automatic update state") {
		t.Fatalf("LastError = %q, want persistence failure", got.LastError)
	}
}

func TestSchedulerDisabledPreservesManualOnlyBehavior(t *testing.T) {
	t.Parallel()

	updater := &schedulerUpdaterStub{}
	scheduler, err := NewScheduler(SchedulerConfig{
		CheckInterval:      time.Hour,
		LastAppliedVersion: "1.2.2",
		Updater:            updater,
		Wait: func(context.Context, time.Duration) bool {
			t.Fatal("disabled scheduler waited for a check")
			return false
		},
	})
	if err != nil {
		t.Fatalf("NewScheduler() error = %v", err)
	}
	scheduler.Run(context.Background())
	if _, err := scheduler.CheckNow(context.Background()); err != nil {
		t.Fatalf("CheckNow() error = %v", err)
	}
	if updater.checkCalls != 0 || updater.applyCalls != 0 {
		t.Fatalf("disabled calls: Check=%d Apply=%d, want zero", updater.checkCalls, updater.applyCalls)
	}
	if got := scheduler.Status(); got.State != "disabled" || got.Enabled || got.LastAppliedVersion != "1.2.2" {
		t.Fatalf("Status() = %#v, want disabled", got)
	}
}

func seedSchedulerLastCheck(t *testing.T, statePath string, checkedAt time.Time) {
	t.Helper()
	scheduler, err := NewScheduler(SchedulerConfig{
		Enabled:       true,
		CheckInterval: 4 * time.Hour,
		StatePath:     statePath,
		Updater:       &schedulerUpdaterStub{},
		Now:           func() time.Time { return checkedAt },
	})
	if err != nil {
		t.Fatalf("NewScheduler() seed error = %v", err)
	}
	if _, err := scheduler.CheckNow(context.Background()); err != nil {
		t.Fatalf("CheckNow() seed error = %v", err)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("Stat() scheduler state error = %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("scheduler state mode = %o, want 600", info.Mode().Perm())
	}
}

type schedulerUpdaterStub struct {
	mu           sync.Mutex
	checkStatus  Status
	checkErr     error
	applyStatus  Status
	applyErr     error
	checkCalls   int
	applyCalls   int
	applyOptions []ApplyOptions
}

func (s *schedulerUpdaterStub) Check(context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkCalls++
	return s.checkStatus, s.checkErr
}

func (s *schedulerUpdaterStub) Apply(_ context.Context, opts ApplyOptions) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyCalls++
	s.applyOptions = append(s.applyOptions, opts)
	return s.applyStatus, s.applyErr
}

func schedulerDrainReservation(context.Context) (func(), error) {
	return func() {}, nil
}
