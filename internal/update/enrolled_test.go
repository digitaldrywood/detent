package update

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestSchedulerEnrolledUpdate(t *testing.T) {
	for _, test := range []struct {
		name        string
		pending     bool
		stale       bool
		unavailable bool
		restart     bool
		applyError  error
		want        string
		calls       int
		urgent      bool
		lastApplied string
	}{
		{name: "applied before restart", want: "applied", calls: 1},
		{name: "detached replacement remains pending", pending: true, want: "uncertain", calls: 1},
		{name: "restart requested", restart: true, want: "restart_requested", calls: 1},
		{name: "urgent release bypasses discovery and automatic opt outs", urgent: true, restart: true, want: "restart_requested", calls: 1},
		{name: "urgent release preserves a newer applied artifact awaiting restart", urgent: true, lastApplied: "1.2.5", want: "refused"},
		{name: "stale observed build", stale: true, want: "refused"},
		{name: "missing restart owner", unavailable: true, want: "refused"},
		{name: "private refusal redacted", applyError: errors.New("/private/credentials/token=secret"), want: "refused", calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			running := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "private_patched_source", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
			updater := &schedulerUpdaterStub{checkStatus: Status{UpdateAvailable: true, LatestVersion: "1.2.4"}, applyStatus: Status{Action: ActionUpdated, LatestVersion: "1.2.4", LatestCommit: strings.Repeat("c", 40), BinarySHA256: strings.Repeat("d", 64), VerifiedRelease: true}, applyErr: test.applyError}
			updater.applyStatus.ReplacementPending = test.pending
			if test.urgent {
				updater.checkStatus = Status{}
			}
			if test.applyError != nil {
				updater.applyStatus.Action = ActionRefused
			}
			drains, restarts := 0, 0
			config := SchedulerConfig{CheckInterval: time.Hour, StatePath: filepath.Join(t.TempDir(), "scheduler.json"), Updater: updater, RunningBuild: running,
				LastAppliedVersion: test.lastApplied,
				ReserveDrain:       func(context.Context) (func(), error) { drains++; return func() {}, nil }, RequestRestart: func(string) bool { restarts++; return test.restart }, Now: func() time.Time { return now }}
			if test.unavailable {
				config.RequestRestart = nil
			}
			scheduler, err := NewScheduler(config)
			if err != nil {
				t.Fatal(err)
			}
			observed := scheduler.EnrolledUpdate(t.Context(), running, nil)
			if observed.Validate() != nil || observed.Running.Source != "private_patched_source" || observed.Running.VerifiedRelease {
				t.Fatalf("initial evidence=%+v", observed)
			}
			request := runnerauth.UpdateRequest{RequestedAt: now, ID: "update-test", Service: "detent", ExpectedBuildRevision: observed.Revision, Version: "1.2.4", Release: true}
			if test.urgent {
				request.Urgent = true
				request.ExpectedBuildRevision = ""
			}
			if test.stale {
				request.ExpectedBuildRevision = strings.Repeat("e", 64)
			}
			observed = scheduler.EnrolledUpdate(t.Context(), running, &request)
			if observed == nil || observed.Validate() != nil || observed.Receipt == nil || observed.Receipt.Status != test.want {
				t.Fatalf("receipt=%+v", observed)
			}
			repeated := scheduler.EnrolledUpdate(t.Context(), running, &request)
			if repeated.Receipt.Status != test.want || updater.applyCalls != test.calls || drains != test.calls {
				t.Fatalf("repeat receipt=%+v calls/drains=%d/%d", repeated, updater.applyCalls, drains)
			}
			if test.calls == 1 && test.applyError == nil && restarts != 1 {
				t.Fatalf("restart owner calls=%d", restarts)
			}
			if test.calls == 1 && (updater.applyOptions[0].Urgent != test.urgent || updater.applyOptions[0].ExpectedVersion != request.Version) {
				t.Fatalf("lost selected release: %+v", updater.applyOptions[0])
			}
			raw, err := json.Marshal(observed)
			if err != nil || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "/private") {
				t.Fatalf("unsafe evidence=%s error=%v", raw, err)
			}
			resumed, err := NewScheduler(config)
			if err != nil {
				t.Fatal(err)
			}
			resumedRunning := running
			if observed.Receipt.VerifiedTarget != nil {
				resumedRunning = *observed.Receipt.VerifiedTarget
			}
			if observed.Receipt.Applied != nil {
				resumedRunning = *observed.Receipt.Applied
			}
			after := resumed.EnrolledUpdate(t.Context(), resumedRunning, &request)
			expected := test.want
			if observed.Receipt.Applied != nil || observed.Receipt.VerifiedTarget != nil {
				expected = "running"
			}
			if after.Receipt.Status != expected || after.Validate() != nil || updater.applyCalls != test.calls {
				t.Fatalf("restart receipt=%+v calls=%d", after, updater.applyCalls)
			}
			if observed.Receipt.Applied != nil || observed.Receipt.VerifiedTarget != nil {
				witnessed := resumed.EnrolledUpdate(t.Context(), resumedRunning, nil)
				if witnessed.Receipt.Running == nil {
					t.Fatal("post-start running receipt was not retained")
				}
				newer := resumedRunning
				newer.Version = "1.2.5"
				newer.SHA256 = strings.Repeat("f", 64)
				drift := resumed.EnrolledUpdate(t.Context(), newer, nil)
				fleet := runnerauth.Runner{Binding: runnerauth.Binding{RunnerID: "runner"}, Health: "online", ConnectionHealth: "online", LastHeartbeatAt: now, Update: drift, Routing: runnerauth.Routing{UpdateRequest: &request}}
				fleet.Update.ReceivedAt = now
				if fleet.UpdateView(now).Status != "drifted" {
					t.Fatalf("later local update retained a pending remote request: %+v", fleet.UpdateView(now))
				}
				patched := resumedRunning
				patched.Source = "private_patched_source"
				patched.VerifiedRelease = false
				if resumed.EnrolledUpdate(t.Context(), patched, nil).Receipt.Status == "running" {
					t.Fatal("patched source claimed verified running release")
				}
			}
		})
	}
}

func TestSchedulerEnrolledInterruptedReceipt(t *testing.T) {
	now := time.Now().UTC()
	running := runnerauth.BuildEvidence{Version: "1.2.3", Commit: "none", Source: "unknown", OS: "linux", Architecture: "amd64", ObservedAt: now}
	request := runnerauth.UpdateRequest{RequestedAt: now, ID: "interrupted", Service: "detent", ExpectedBuildRevision: strings.Repeat("a", 64), Version: "1.2.4", Release: true}
	path := filepath.Join(t.TempDir(), "scheduler.json")
	if err := saveSchedulerState(path, schedulerState{LastCheckAt: now, EnrolledReceipt: &runnerauth.UpdateReceipt{Request: request, Status: "draining", ObservedAt: now}}); err != nil {
		t.Fatal(err)
	}
	updater := &schedulerUpdaterStub{}
	config := SchedulerConfig{CheckInterval: time.Hour, StatePath: path, Updater: updater}
	scheduler, err := NewScheduler(config)
	if err != nil {
		t.Fatal(err)
	}
	report := scheduler.EnrolledUpdate(t.Context(), running, &request)
	if report == nil || report.Receipt.Status != "uncertain" || updater.applyCalls != 0 {
		t.Fatalf("interrupted receipt=%+v", report)
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	scheduler, err = NewScheduler(config)
	if err != nil {
		t.Fatal(err)
	}
	report = scheduler.EnrolledUpdate(t.Context(), running, nil)
	if report == nil || report.Supported {
		t.Fatal("invalid receipt state advertised effect support")
	}
}

func TestSchedulerDevelopmentBuildSkipsDiscovery(t *testing.T) {
	for _, version := range []string{"operator-landed-ec4a9d45ff54", "develop", "dev"} {
		t.Run(version, func(t *testing.T) {
			now := time.Now().UTC()
			running := runnerauth.BuildEvidence{Version: version, Commit: strings.Repeat("a", 40), Source: "unknown", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
			updater := &schedulerUpdaterStub{checkErr: ErrRefused, applyStatus: Status{Action: ActionUpdated, LatestVersion: "1.2.4", LatestCommit: strings.Repeat("c", 40), BinarySHA256: strings.Repeat("d", 64), VerifiedRelease: true}}
			statePath := filepath.Join(t.TempDir(), "scheduler.json")
			drains, restarts, waits := 0, 0, 0
			scheduler, err := NewScheduler(SchedulerConfig{Enabled: true, AutoApplyEnabled: true, CheckInterval: 15 * time.Second, RunningBuild: running, StatePath: statePath, Updater: updater,
				ReserveIdle:    func(context.Context) (func(), bool) { return func() {}, true },
				ReserveDrain:   func(context.Context) (func(), error) { drains++; return func() {}, nil },
				RequestRestart: func(string) bool { restarts++; return true },
				Wait:           func(context.Context, time.Duration) bool { waits++; return false },
			})
			if err != nil {
				t.Fatal(err)
			}
			if status := scheduler.Status(); status.Enabled || status.State != "disabled" {
				t.Fatalf("unsupported build scheduled discovery: %+v", status)
			}
			scheduler.Run(t.Context())
			if _, err := scheduler.CheckNow(t.Context()); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				observed := scheduler.EnrolledUpdate(t.Context(), running, nil)
				if observed == nil || !observed.Supported || observed.Discovery != "unknown" || observed.Validate() != nil {
					t.Fatalf("explicit enrolled owner lost: %+v", observed)
				}
			}
			if status := scheduler.Status(); updater.checkCalls != 0 || waits != 0 || status.LastError != "" || status.LastCheckAt != nil {
				t.Fatalf("unsupported discovery calls=%d waits=%d status=%+v", updater.checkCalls, waits, status)
			}
			if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsupported discovery persisted state: %v", err)
			}
			if _, err := scheduler.ApplyRelease(t.Context(), true); err != nil {
				t.Fatal(err)
			}
			if updater.applyCalls != 1 || drains != 1 || restarts != 1 || !updater.applyOptions[0].FromRelease {
				t.Fatalf("explicit release owner changed: applies=%d drains=%d restarts=%d options=%+v", updater.applyCalls, drains, restarts, updater.applyOptions)
			}
		})
	}
}
