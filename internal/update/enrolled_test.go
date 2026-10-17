package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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
		follow      bool
		urgent      bool
		version     string
		lastApplied string
		nextVersion string
		nextWant    string
		logged      bool
	}{
		{name: "restart declined", want: "uncertain", calls: 1, logged: true},
		{name: "detached replacement remains pending", pending: true, restart: true, want: "uncertain", calls: 1},
		{name: "restart requested", restart: true, want: "restart_requested", calls: 1},
		{name: "urgent release bypasses discovery and automatic opt outs", urgent: true, restart: true, want: "restart_requested", calls: 1},
		{name: "urgent equal version does not drain", urgent: true, version: "1.2.3", want: "refused"},
		{name: "urgent older version does not drain", urgent: true, version: "1.2.2", want: "refused"},
		{name: "urgent release preserves a newer applied artifact awaiting restart", urgent: true, lastApplied: "1.2.5", want: "refused"},
		{name: "Hub follows without automatic opt ins or latest discovery", follow: true, restart: true, want: "restart_requested", calls: 1},
		{name: "equivalent urgent request retains Hub apply", follow: true, restart: true, want: "restart_requested", calls: 1, nextVersion: "1.2.4", nextWant: "restart_requested"},
		{name: "equivalent urgent request retains detached target", follow: true, pending: true, restart: true, want: "uncertain", calls: 1, nextVersion: "1.2.4", nextWant: "uncertain"},
		{name: "older urgent request retains newer applied artifact", follow: true, restart: true, want: "restart_requested", calls: 1, nextVersion: "1.2.2", nextWant: "refused"},
		{name: "urgent request equal to running retains newer applied artifact", follow: true, restart: true, want: "restart_requested", calls: 1, nextVersion: "1.2.3", nextWant: "refused"},
		{name: "Hub equal version does not drain", follow: true, version: "1.2.3", want: "refused"},
		{name: "Hub older version does not drain", follow: true, version: "1.2.2", want: "refused"},
		{name: "Hub follow refuses stale build", follow: true, stale: true, want: "refused"},
		{name: "stale observed build", stale: true, want: "refused"},
		{name: "missing restart owner", unavailable: true, want: "refused"},
		{name: "private refusal redacted", applyError: errors.New("/private/credentials/token=secret"), want: "refused", calls: 1, logged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			running := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "private_patched_source", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
			updater := &schedulerUpdaterStub{checkStatus: Status{UpdateAvailable: true, LatestVersion: "1.2.4"}, applyStatus: Status{Action: ActionUpdated, LatestVersion: "1.2.4", LatestCommit: strings.Repeat("c", 40), BinarySHA256: strings.Repeat("d", 64), VerifiedRelease: true}, applyErr: test.applyError}
			updater.applyStatus.ReplacementPending = test.pending
			if test.urgent || test.follow {
				updater.checkStatus = Status{}
			}
			if test.applyError != nil {
				updater.applyStatus.Action = ActionRefused
			}
			drains, restarts := 0, 0
			var logs bytes.Buffer
			config := SchedulerConfig{Logger: slog.New(slog.NewTextHandler(&logs, nil)), CheckInterval: time.Hour, StatePath: filepath.Join(t.TempDir(), "scheduler.json"), Updater: updater, RunningBuild: running,
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
			request := runnerauth.UpdateRequest{RequestedAt: now, ID: "update-test", Service: "detent", ExpectedBuildRevision: observed.Revision, Version: "1.2.4", Release: true, FollowHub: test.follow}
			if test.version != "" {
				request.Version = test.version
			}
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
			if got := strings.Count(logs.String(), "enrolled runner update not applied"); got != map[bool]int{true: 1}[test.logged] {
				t.Fatalf("refusal logged %d times: %s", got, logs.String())
			}
			if test.logged && (!strings.Contains(logs.String(), "failure_reason=") || (test.applyError != nil) != strings.Contains(logs.String(), "error=")) {
				t.Fatalf("refusal log lacks failure reason or error: %s", logs.String())
			}
			if test.calls == 0 {
				now = now.Add(config.CheckInterval)
				delayed := scheduler.EnrolledUpdate(t.Context(), running, &request)
				if delayed.Receipt.Status != "refused" || delayed.Receipt.FailureReason == "" || updater.applyCalls != 0 || drains != 0 {
					t.Fatalf("invalid target became retryable: %+v calls/drains=%d/%d", delayed.Receipt, updater.applyCalls, drains)
				}
			}
			if test.calls == 1 && test.applyError == nil && restarts != 1 {
				t.Fatalf("restart owner calls=%d", restarts)
			}
			if test.calls == 1 && (updater.applyOptions[0].FollowHub != test.follow || updater.applyOptions[0].Urgent != test.urgent || updater.applyOptions[0].ExpectedVersion != request.Version) {
				t.Fatalf("lost selected release: %+v", updater.applyOptions[0])
			}
			raw, err := json.Marshal(observed)
			if err != nil || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "/private") {
				t.Fatalf("unsafe evidence=%s error=%v", raw, err)
			}
			var nextRequest *runnerauth.UpdateRequest
			if test.nextVersion != "" {
				before, found, err := loadSchedulerState(config.StatePath)
				if err != nil || !found {
					t.Fatalf("load installer receipt: found=%t error=%v", found, err)
				}
				now = now.Add(time.Minute)
				nextRequest = &runnerauth.UpdateRequest{RequestedAt: now, ID: "update-urgent", Service: "detent", Version: test.nextVersion, Release: true, Urgent: true}
				for range 2 {
					next := scheduler.EnrolledUpdate(t.Context(), running, nextRequest)
					if next == nil || next.Validate() != nil || next.Receipt.Status != test.nextWant || updater.applyCalls != 1 || drains != 1 || restarts != 1 {
						t.Fatalf("interleaved receipt=%+v calls/drains/restarts=%d/%d/%d", next, updater.applyCalls, drains, restarts)
					}
					if test.nextWant == "refused" {
						if next.Receipt.Request != *nextRequest || next.Receipt.FailureReason == "" || next.Receipt.Applied != nil || next.Receipt.VerifiedTarget != nil {
							t.Fatalf("invalid target lacks truthful refusal: %+v", next.Receipt)
						}
					} else if !reflect.DeepEqual(next.Receipt, observed.Receipt) {
						t.Fatalf("equivalent request replaced installer receipt: got %+v want %+v", next.Receipt, observed.Receipt)
					}
				}
				persisted, found, err := loadSchedulerState(config.StatePath)
				if err != nil || !found || !reflect.DeepEqual(persisted.EnrolledReceipt, before.EnrolledReceipt) {
					t.Fatalf("interleaved request lost persisted proof: %+v error=%v", persisted.EnrolledReceipt, err)
				}
			}
			resumedRunning := running
			if observed.Receipt.VerifiedTarget != nil {
				resumedRunning = *observed.Receipt.VerifiedTarget
			}
			if observed.Receipt.Applied != nil {
				resumedRunning = *observed.Receipt.Applied
			}
			resumedRunning.VerifiedRelease = false
			resumedRunning.Source = "unknown"
			config.RunningBuild = resumedRunning
			resumed, err := NewScheduler(config)
			if err != nil {
				t.Fatal(err)
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
				if !witnessed.Supported || !witnessed.Running.VerifiedRelease || witnessed.Running.Source != "release" || witnessed.Receipt.Running == nil || witnessed.Receipt.Request != request || witnessed.Receipt.Applied.Commit != strings.Repeat("c", 40) || witnessed.Receipt.Applied.SHA256 != strings.Repeat("d", 64) {
					t.Fatalf("post-start verified running proof was not retained: %+v", witnessed)
				}
				if nextRequest != nil && nextRequest.Version == request.Version {
					matching := resumed.EnrolledUpdate(t.Context(), resumedRunning, nextRequest)
					if matching.Receipt.Status != "running" || !matching.Running.VerifiedRelease || matching.Receipt.Request != request || updater.applyCalls != 1 {
						t.Fatalf("equivalent post-start request lost verified proof: %+v", matching)
					}
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
				mismatched := resumedRunning
				mismatched.SHA256 = strings.Repeat("f", 64)
				if report := resumed.EnrolledUpdate(t.Context(), mismatched, nil); report.Receipt.Status == "running" || report.Running.VerifiedRelease {
					t.Fatalf("mismatched artifact claimed verified running release: %+v", report)
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
	updater := &schedulerUpdaterStub{}
	config := SchedulerConfig{CheckInterval: time.Hour, StatePath: path, Updater: updater, RunningBuild: running, Now: func() time.Time { return now }}
	for _, test := range []struct {
		name    string
		receipt runnerauth.UpdateReceipt
		reason  string
	}{
		{name: "interrupted drain", receipt: runnerauth.UpdateReceipt{Request: request, Status: "draining", ObservedAt: now}, reason: "Update was interrupted before completion"},
		{name: "restart returned old build", receipt: runnerauth.UpdateReceipt{Request: request, Status: "applied", Applied: &runnerauth.BuildEvidence{Version: "1.2.4", Commit: strings.Repeat("c", 40), Source: "release", SHA256: strings.Repeat("d", 64), OS: "linux", Architecture: "amd64", VerifiedRelease: true, ObservedAt: now}, ObservedAt: now}, reason: "Update did not restart into the verified build"},
		{name: "detached replacement returned old build", receipt: runnerauth.UpdateReceipt{Request: request, Status: "uncertain", VerifiedTarget: &runnerauth.BuildEvidence{Version: "1.2.4", Commit: strings.Repeat("c", 40), Source: "release", SHA256: strings.Repeat("d", 64), OS: "linux", Architecture: "amd64", VerifiedRelease: true, ObservedAt: now}, ObservedAt: now}, reason: "Update did not restart into the verified build"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := saveSchedulerState(path, schedulerState{LastCheckAt: now, EnrolledReceipt: &test.receipt}); err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewScheduler(config)
			if err != nil {
				t.Fatal(err)
			}
			report := scheduler.EnrolledUpdate(t.Context(), running, &request)
			if report == nil || report.Receipt.Status != "uncertain" || report.Receipt.FailureReason != test.reason || updater.applyCalls != 0 {
				t.Fatalf("interrupted receipt=%+v", report)
			}
			persisted, found, err := loadSchedulerState(path)
			if err != nil || !found || persisted.EnrolledReceipt.FailureReason != test.reason {
				t.Fatalf("interrupted receipt was not persisted: %+v %v", persisted, err)
			}
		})
	}
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewScheduler(config)
	if err != nil {
		t.Fatal(err)
	}
	report := scheduler.EnrolledUpdate(t.Context(), running, nil)
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
