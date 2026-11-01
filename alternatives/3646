package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestSchedulerEnrolledReleaseAdvance(t *testing.T) {
	t.Parallel()
	for _, versions := range [][2]string{{"1.0.1", "1.0.2"}, {"2.0.1", "2.0.2"}} {
		for _, owner := range []string{"enrolled", "pending owner"} {
			t.Run(versions[0]+"/"+owner, func(t *testing.T) {
				t.Parallel()
				config, _, publish := enrolledSignedReleaseFixture(t)
				service := config.Updater.(*Service)
				binary := service.cfg.ExecutablePath
				running := config.RunningBuild
				now := running.ObservedAt
				publish(versions[0])
				scheduler, err := NewScheduler(config)
				if err != nil {
					t.Fatal(err)
				}
				observed := scheduler.EnrolledUpdate(t.Context(), running, nil)
				request := runnerauth.UpdateRequest{ID: "follow-" + versions[0], Service: "detent", Release: true, FollowHub: true, Version: versions[0], ExpectedBuildRevision: observed.Revision, RequestedAt: now}
				if owner == "pending owner" {
					scheduler.setEnrolledReceipt(&runnerauth.UpdateReceipt{Request: request, Status: "applying", ObservedAt: now})
					if err := scheduler.persistCurrentState(); err != nil {
						t.Fatal(err)
					}
					publish(versions[1])
					applied, err := scheduler.ApplyRelease(t.Context(), true)
					if err != nil || applied.Action != ActionUpdated || applied.LatestVersion != versions[0] {
						t.Fatalf("apply: %+v %v", applied, err)
					}
				} else {
					scheduler.cfg.ReserveIdle = func(context.Context) (func(), bool) {
						publish(versions[1])
						return func() {}, true
					}
					observed = scheduler.EnrolledUpdate(t.Context(), running, &request)
					if observed == nil || observed.Validate() != nil || observed.Receipt == nil || observed.Receipt.Status != "restart_requested" {
						t.Fatalf("enrolled apply: %+v", observed)
					}
				}
				state, found, err := loadSchedulerState(config.StatePath)
				if err != nil || !found {
					t.Fatalf("successful producer wrote unreadable receipt: %v", err)
				}
				if state.EnrolledReceipt.Request != request || state.EnrolledReceipt.Applied.Version != versions[0] {
					t.Fatalf("apply lost authorized target: %+v", state.EnrolledReceipt)
				}
				installed, err := os.ReadFile(binary)
				if err != nil || string(installed) != "version: v"+versions[0]+"\ncommit: "+testUpdatedCommit+"\n" {
					t.Fatalf("installed wrong release: %q %v", installed, err)
				}
				sum := sha256.Sum256(installed)
				running.Version, running.Commit, running.SHA256 = versions[0], testUpdatedCommit, hex.EncodeToString(sum[:])
				running.VerifiedRelease = state.EnrolledReceipt.Applied.VerifiedRelease
				config.RunningBuild = running
				resumed, err := NewScheduler(config)
				if err != nil {
					t.Fatal(err)
				}
				after := resumed.EnrolledUpdate(t.Context(), running, nil)
				if !after.Supported || after.Validate() != nil || after.Receipt == nil || after.Receipt.Status != "running" || after.Receipt.Request != request {
					t.Fatalf("restart lost receipt: %+v", after)
				}
			})
		}
	}
}

func enrolledSignedReleaseFixture(t *testing.T) (SchedulerConfig, *workerReleaseFixture, func(string) Release) {
	t.Helper()
	artifacts, fixture, publish := workerArtifactFixture(t)
	service := artifacts.service
	root := t.TempDir()
	binary := filepath.Join(root, "detent")
	lock := filepath.Join(root, "install.lock")
	if err := os.WriteFile(binary, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("binary="+binary+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service.cfg.ExecutablePath = binary
	service.cfg.Env = map[string]string{"DETENT_INSTALL_LOCK": lock}
	now := time.Now().UTC()
	running := runnerauth.BuildEvidence{Version: "1.0.0", Commit: testPreviousCommit, Source: "release", SHA256: strings.Repeat("a", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
	config := SchedulerConfig{CheckInterval: time.Hour, StatePath: filepath.Join(root, "scheduler.json"), Updater: service, RunningBuild: running,
		ReserveIdle: func(context.Context) (func(), bool) { return func() {}, true },
		ReserveDrain: func(context.Context) (func(), error) {
			t.Fatal("routine update drained active work")
			return nil, ErrRefused
		},
		RequestRestart: func(string) bool { return true },
	}
	return config, fixture, publish
}

func TestSchedulerEnrolledLegacyReleaseAdvance(t *testing.T) {
	t.Parallel()
	for _, automatic := range []bool{false, true} {
		for _, failure := range []string{"", "signature", "source outage", "checksum", "provenance", "platform", "digest", "commit", "binary identity", "private source", "unverified", "pinned request", "older applied release", "conflicting target", "invalid receipt", "missing verifier", "running drift"} {
			if automatic && failure != "" && failure != "signature" && failure != "source outage" {
				continue
			}
			name := "enrolled/" + failure
			if automatic {
				name = "automatic/" + failure
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				config, fixture, publish := enrolledSignedReleaseFixture(t)
				service := config.Updater.(*Service)
				publish("1.0.1")
				release := publish("1.0.2")
				applied, err := service.Apply(t.Context(), ApplyOptions{AssumeYes: true, FollowHub: true, ExpectedVersion: "1.0.2"})
				if err != nil || applied.Action != ActionUpdated || !applied.VerifiedRelease {
					t.Fatalf("legacy signed install: %+v %v", applied, err)
				}
				now := config.RunningBuild.ObservedAt
				config.Now = func() time.Time { return now }
				config.Enabled = automatic
				request := runnerauth.UpdateRequest{ID: "legacy-follow", Service: "detent", Release: true, FollowHub: true, Version: "1.0.1", ExpectedBuildRevision: strings.Repeat("b", 64), RequestedAt: now}
				evidence := runnerauth.BuildEvidence{Version: "1.0.2", Commit: applied.LatestCommit, Source: "release", SHA256: applied.BinarySHA256, OS: "linux", Architecture: "amd64", VerifiedRelease: true, ObservedAt: now}
				receipt := runnerauth.UpdateReceipt{Request: request, Status: "applied", Applied: &evidence, ObservedAt: now}
				config.RunningBuild = evidence
				config.RunningBuild.Source, config.RunningBuild.VerifiedRelease = "unknown", false
				signature := fixture.downloads[release.Assets[2].BrowserDownloadURL]
				switch failure {
				case "source outage":
					fixture.listErr = errors.New("release source unavailable")
				case "signature":
					fixture.downloads[release.Assets[2].BrowserDownloadURL] = []byte("invalid signature")
				case "checksum":
					fixture.downloads[release.Assets[0].BrowserDownloadURL] = []byte("corrupt archive")
				case "provenance":
					fixture.downloads[release.Assets[3].BrowserDownloadURL] = testReleaseProvenance(t, "v2.0.0", testUpdatedCommit)
				case "platform":
					evidence.Architecture = "arm64"
					config.RunningBuild.Architecture = evidence.Architecture
				case "digest":
					evidence.SHA256 = strings.Repeat("c", 64)
					config.RunningBuild.SHA256 = evidence.SHA256
				case "commit":
					evidence.Commit = strings.Repeat("c", 40)
					config.RunningBuild.Commit = evidence.Commit
				case "binary identity":
					service.cfg.BinaryVerifier = func(context.Context, string) (string, error) {
						return "version: v1.0.1\ncommit: " + testUpdatedCommit + "\n", nil
					}
				case "private source":
					config.RunningBuild.Source = "private_patched_source"
				case "unverified":
					evidence.VerifiedRelease = false
				case "pinned request":
					receipt.Request.FollowHub = false
				case "older applied release":
					receipt.Request.Version = "1.0.3"
				case "conflicting target":
					target := evidence
					target.Version = "1.0.1"
					receipt.VerifiedTarget = &target
				case "invalid receipt":
					receipt.Request.ExpectedBuildRevision = "invalid"
				case "missing verifier":
					config.Updater = &schedulerUpdaterStub{}
				case "running drift":
					config.RunningBuild.SHA256 = strings.Repeat("c", 64)
				}
				raw, err := json.Marshal(schedulerState{Schema: schedulerStateSchema, LastCheckAt: now, EnrolledReceipt: &receipt})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(config.StatePath, raw, 0o600); err != nil {
					t.Fatal(err)
				}
				downloadsBeforeStartup := make(map[string]int)
				for url, count := range fixture.downloadsByURL {
					downloadsBeforeStartup[url] = count
				}
				fixture.list = func() { t.Error("scheduler construction accessed release source") }
				scheduler, err := NewScheduler(config)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(fixture.downloadsByURL, downloadsBeforeStartup) {
					t.Fatal("scheduler construction downloaded legacy release evidence")
				}
				fixture.list = nil
				observed := scheduler.EnrolledUpdate(t.Context(), config.RunningBuild, nil)
				if automatic {
					if observed.Supported || observed.Receipt != nil {
						t.Fatalf("legacy evidence trusted before a release check: %+v", observed)
					}
					_, checkErr := scheduler.CheckNow(t.Context())
					if (checkErr != nil) != (failure != "") {
						t.Fatalf("legacy release check: %v", checkErr)
					}
					observed = scheduler.EnrolledUpdate(t.Context(), config.RunningBuild, nil)
				}
				if failure != "" {
					if observed == nil || observed.Supported || observed.Receipt != nil || observed.Running.VerifiedRelease {
						t.Fatalf("untrusted legacy receipt accepted: %+v", observed)
					}
					scheduler.EnrolledUpdate(t.Context(), config.RunningBuild, &request)
					unchanged, err := os.ReadFile(config.StatePath)
					if err != nil || !bytes.Equal(unchanged, raw) {
						t.Fatalf("untrusted history changed: %s %v", unchanged, err)
					}
					if failure != "signature" && failure != "source outage" {
						return
					}
					downloadsAfterFailure := fixture.downloadsByURL[release.Assets[1].BrowserDownloadURL]
					fixture.downloads[release.Assets[2].BrowserDownloadURL] = signature
					fixture.listErr = nil
					for range 2 {
						if report := scheduler.EnrolledUpdate(t.Context(), config.RunningBuild, nil); report.Supported {
							t.Fatal("receipt retried outside the release check cadence")
						}
						scheduler.EnrolledUpdate(t.Context(), config.RunningBuild, &request)
					}
					if fixture.downloadsByURL[release.Assets[1].BrowserDownloadURL] != downloadsAfterFailure {
						t.Fatal("enrolled observation repeated legacy downloads before the next check")
					}
					now = now.Add(config.CheckInterval)
					if automatic {
						if _, err := scheduler.CheckNow(t.Context()); err != nil {
							t.Fatal(err)
						}
					}
					observed = scheduler.EnrolledUpdate(t.Context(), config.RunningBuild, nil)
				}
				if observed == nil || !observed.Supported || observed.Validate() != nil || !observed.Running.VerifiedRelease || observed.Receipt == nil || observed.Receipt.Status != "running" || observed.Receipt.Request != request || !reflect.DeepEqual(observed.Receipt.Applied, &evidence) {
					t.Fatalf("legacy recovery lost original request or actual artifact: %+v", observed)
				}
				persisted, found, err := loadSchedulerState(config.StatePath)
				if err != nil || !found || persisted.EnrolledReceipt.Request != request || !reflect.DeepEqual(persisted.EnrolledReceipt.Applied, &evidence) {
					t.Fatalf("legacy recovery did not persist truthful history: %+v %v", persisted, err)
				}
				installed, err := os.ReadFile(service.cfg.ExecutablePath)
				if err != nil || string(installed) != "version: v1.0.2\ncommit: "+testUpdatedCommit+"\n" {
					t.Fatalf("recovery changed installed artifact: %q %v", installed, err)
				}
				fixture.list = func() { t.Error("verified legacy receipt scheduled another recovery check") }
				now = now.Add(config.CheckInterval)
				if report := scheduler.EnrolledUpdate(t.Context(), config.RunningBuild, nil); !report.Supported || report.Receipt.Status != "running" {
					t.Fatalf("verified legacy receipt was not retained: %+v", report)
				}
			})
		}
	}
}

func TestSchedulerEnrolledUpdate(t *testing.T) {
	for _, test := range []struct {
		name           string
		pending        bool
		stale          bool
		unavailable    bool
		restart        bool
		applyError     error
		want           string
		calls          int
		follow         bool
		urgent         bool
		version        string
		runningVersion string
		lastApplied    string
		nextVersion    string
		nextWant       string
		logged         bool
		release        bool
		prior          bool
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
		{name: "Hub equal patched version does not drain", follow: true, version: "1.2.3", want: "refused"},
		{name: "operator repairs equal unverified release", release: true, version: "1.2.3", restart: true, want: "restart_requested", calls: 1},
		{name: "Hub repairs equal unverified release", follow: true, release: true, prior: true, version: "1.2.3", restart: true, want: "restart_requested", calls: 1},
		{name: "operator repairs prefixed unverified release", release: true, version: "1.2.3", runningVersion: "v1.2.3", restart: true, want: "restart_requested", calls: 1},
		{name: "Hub repairs prefixed unverified release", follow: true, release: true, prior: true, version: "1.2.3", runningVersion: "v1.2.3", restart: true, want: "restart_requested", calls: 1},
		{name: "prefixed repair refuses stale revision", follow: true, release: true, version: "1.2.3", runningVersion: "v1.2.3", stale: true, want: "refused"},
		{name: "same version repair retains newer installed artifact", follow: true, release: true, version: "1.2.3", lastApplied: "1.2.5", want: "refused"},
		{name: "same version repair refuses stale revision", follow: true, release: true, version: "1.2.3", stale: true, want: "refused"},
		{name: "Hub older version does not drain", follow: true, version: "1.2.2", want: "refused"},
		{name: "Hub follow refuses stale build", follow: true, stale: true, want: "refused"},
		{name: "stale observed build", stale: true, want: "refused"},
		{name: "missing restart owner", unavailable: true, want: "refused"},
		{name: "private refusal redacted", applyError: errors.New("/private/credentials/token=secret"), want: "refused", calls: 1, logged: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			running := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "private_patched_source", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
			if test.runningVersion != "" {
				running.Version = test.runningVersion
			}
			updater := &schedulerUpdaterStub{checkStatus: Status{UpdateAvailable: true, LatestVersion: "1.2.4"}, applyStatus: Status{Action: ActionUpdated, LatestVersion: "1.2.4", LatestCommit: strings.Repeat("c", 40), BinarySHA256: strings.Repeat("d", 64), VerifiedRelease: true}, applyErr: test.applyError}
			if test.release {
				running.Source = "release"
				updater.applyStatus.LatestVersion = test.version
			}
			updater.applyStatus.ReplacementPending = test.pending
			if test.urgent || test.follow {
				updater.checkStatus = Status{}
			}
			if test.applyError != nil {
				updater.applyStatus.Action = ActionRefused
			}
			reservations, restarts := 0, 0
			var logs bytes.Buffer
			config := SchedulerConfig{Logger: slog.New(slog.NewTextHandler(&logs, nil)), CheckInterval: time.Hour, StatePath: filepath.Join(t.TempDir(), "scheduler.json"), Updater: updater, RunningBuild: running,
				LastAppliedVersion: test.lastApplied,
				ReserveIdle:        func(context.Context) (func(), bool) { reservations++; return func() {}, true },
				ReserveDrain: func(context.Context) (func(), error) {
					t.Fatal("routine request drained runner")
					return nil, ErrRefused
				}, RequestRestart: func(string) bool { restarts++; return test.restart }, Now: func() time.Time { return now }}
			if test.unavailable {
				config.RequestRestart = nil
			}
			if test.prior {
				previous := &runnerauth.UpdateReceipt{Request: runnerauth.UpdateRequest{ID: "previous-refused", Service: "detent", Version: test.version, Release: true, FollowHub: true, ExpectedBuildRevision: strings.Repeat("e", 64), RequestedAt: now.Add(-time.Hour)}, Status: "refused", FailureReason: enrolledTargetRefusal, ObservedAt: now.Add(-time.Minute)}
				if err := saveSchedulerState(config.StatePath, schedulerState{LastCheckAt: now, EnrolledReceipt: previous}); err != nil {
					t.Fatal(err)
				}
			}
			scheduler, err := NewScheduler(config)
			if err != nil {
				t.Fatal(err)
			}
			observed := scheduler.EnrolledUpdate(t.Context(), running, nil)
			if observed.Validate() != nil || observed.Running.Source != running.Source || observed.Running.VerifiedRelease {
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
			if test.prior && (observed.Receipt.PreviousFailure == nil || observed.Receipt.PreviousFailure.Request.ID != "previous-refused" || observed.Receipt.PreviousFailure.FailureReason != enrolledTargetRefusal) {
				t.Fatalf("repair lost refusal history: %+v", observed.Receipt)
			}
			repeated := scheduler.EnrolledUpdate(t.Context(), running, &request)
			if repeated.Receipt.Status != test.want || updater.applyCalls != test.calls || reservations != test.calls {
				t.Fatalf("repeat receipt=%+v calls/reservations=%d/%d", repeated, updater.applyCalls, reservations)
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
				if delayed.Receipt.Status != "refused" || delayed.Receipt.FailureReason == "" || updater.applyCalls != 0 || reservations != 0 {
					t.Fatalf("invalid target became retryable: %+v calls/reservations=%d/%d", delayed.Receipt, updater.applyCalls, reservations)
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
					if next == nil || next.Validate() != nil || next.Receipt.Status != test.nextWant || updater.applyCalls != 1 || reservations != 1 || restarts != 1 {
						t.Fatalf("interleaved receipt=%+v calls/reservations/restarts=%d/%d/%d", next, updater.applyCalls, reservations, restarts)
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
			resumedRunning.VerifiedRelease = observed.Receipt.Applied != nil || observed.Receipt.VerifiedTarget != nil
			if resumedRunning.VerifiedRelease {
				resumedRunning.Source = "release"
			}
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
			if test.prior && !reflect.DeepEqual(after.Receipt.PreviousFailure, observed.Receipt.PreviousFailure) {
				t.Fatalf("restart lost refusal history: %+v", after.Receipt)
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
				mismatched.VerifiedRelease = false
				if report := resumed.EnrolledUpdate(t.Context(), mismatched, nil); report.Receipt.Status == "running" || report.Running.VerifiedRelease {
					t.Fatalf("mismatched artifact claimed verified running release: %+v", report)
				}
			}
		})
	}
}

func TestSchedulerEnrolledInterruptedReceipt(t *testing.T) {
	now := time.Now().UTC()
	running := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), SHA256: strings.Repeat("b", 64), Source: "release", OS: "linux", Architecture: "amd64", ObservedAt: now}
	request := runnerauth.UpdateRequest{RequestedAt: now, ID: "interrupted", Service: "detent", ExpectedBuildRevision: strings.Repeat("a", 64), Version: "1.2.4", Release: true}
	sameVersionRequest := request
	sameVersionRequest.Version = running.Version
	forged := running
	forged.VerifiedRelease = true
	path := filepath.Join(t.TempDir(), "scheduler.json")
	updater := &schedulerUpdaterStub{}
	config := SchedulerConfig{CheckInterval: time.Hour, StatePath: path, Updater: updater, RunningBuild: running, Now: func() time.Time { return now }}
	for _, test := range []struct {
		name    string
		receipt runnerauth.UpdateReceipt
		reason  string
		invalid bool
	}{
		{name: "unsigned running receipt cannot verify running build", receipt: runnerauth.UpdateReceipt{Request: sameVersionRequest, Status: "running", Applied: &forged, Running: &forged, ObservedAt: now}, reason: "Update did not restart into the verified build"},
		{name: "unsigned target receipt cannot verify running build", receipt: runnerauth.UpdateReceipt{Request: sameVersionRequest, Status: "uncertain", VerifiedTarget: &forged, ObservedAt: now}, reason: "Update did not restart into the verified build"},
		{name: "unsigned applied receipt cannot verify running build", receipt: runnerauth.UpdateReceipt{Request: sameVersionRequest, Status: "applied", Applied: &forged, ObservedAt: now}, reason: "Update did not restart into the verified build"},
		{name: "interrupted drain", receipt: runnerauth.UpdateReceipt{Request: request, Status: "draining", ObservedAt: now}, reason: "Update was interrupted before completion"},
		{name: "restart returned old build", receipt: runnerauth.UpdateReceipt{Request: request, Status: "applied", Applied: &runnerauth.BuildEvidence{Version: "1.2.4", Commit: strings.Repeat("c", 40), Source: "release", SHA256: strings.Repeat("d", 64), OS: "linux", Architecture: "amd64", VerifiedRelease: true, ObservedAt: now}, ObservedAt: now}, reason: "Update did not restart into the verified build"},
		{name: "detached replacement returned old build", receipt: runnerauth.UpdateReceipt{Request: request, Status: "uncertain", VerifiedTarget: &runnerauth.BuildEvidence{Version: "1.2.4", Commit: strings.Repeat("c", 40), Source: "release", SHA256: strings.Repeat("d", 64), OS: "linux", Architecture: "amd64", VerifiedRelease: true, ObservedAt: now}, ObservedAt: now}, reason: "Update did not restart into the verified build"},
		{name: "success without evidence cannot replace state", receipt: runnerauth.UpdateReceipt{Request: request, Status: "applied", ObservedAt: now}, invalid: true},
		{name: "conflicting target cannot replace state", receipt: runnerauth.UpdateReceipt{Request: request, Status: "applied", Applied: &runnerauth.BuildEvidence{Version: "1.2.5", Commit: strings.Repeat("c", 40), Source: "release", SHA256: strings.Repeat("d", 64), OS: "linux", Architecture: "amd64", VerifiedRelease: true, ObservedAt: now}, ObservedAt: now}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.invalid {
				if err := saveSchedulerState(path, schedulerState{LastCheckAt: now}); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := saveSchedulerState(path, schedulerState{LastCheckAt: now, EnrolledReceipt: &test.receipt}); err == nil {
					t.Fatal("invalid receipt was saved")
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("refused save replaced valid state: %s %v", after, err)
				}
				return
			}
			if err := saveSchedulerState(path, schedulerState{LastCheckAt: now, EnrolledReceipt: &test.receipt}); err != nil {
				t.Fatal(err)
			}
			scheduler, err := NewScheduler(config)
			if err != nil {
				t.Fatal(err)
			}
			report := scheduler.EnrolledUpdate(t.Context(), running, &test.receipt.Request)
			if report == nil || report.Running.VerifiedRelease || report.Receipt.Status != "uncertain" || report.Receipt.FailureReason != test.reason || updater.applyCalls != 0 {
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
			reservations, restarts, waits := 0, 0, 0
			scheduler, err := NewScheduler(SchedulerConfig{Enabled: true, AutoApplyEnabled: true, CheckInterval: 15 * time.Second, RunningBuild: running, StatePath: statePath, Updater: updater,
				ReserveIdle: func(context.Context) (func(), bool) { reservations++; return func() {}, true },
				ReserveDrain: func(context.Context) (func(), error) {
					t.Fatal("routine development update drained runner")
					return nil, ErrRefused
				},
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
			if updater.applyCalls != 1 || reservations != 1 || restarts != 1 || !updater.applyOptions[0].FromRelease {
				t.Fatalf("explicit release owner changed: applies=%d reservations=%d restarts=%d options=%+v", updater.applyCalls, reservations, restarts, updater.applyOptions)
			}
		})
	}
}
