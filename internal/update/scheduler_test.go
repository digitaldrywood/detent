package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		RequestRestart: func(string) bool { return true },
		NextDelay:      func(interval time.Duration) time.Duration { return interval },
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

type schedulerUpdaterStub struct {
	mu          sync.Mutex
	checkStatus Status
	checkErr    error
	applyStatus Status
	applyErr    error
	checkCalls  int
	applyCalls  int
}

func (s *schedulerUpdaterStub) Check(context.Context) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkCalls++
	return s.checkStatus, s.checkErr
}

func (s *schedulerUpdaterStub) Apply(context.Context, ApplyOptions) (Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyCalls++
	return s.applyStatus, s.applyErr
}
