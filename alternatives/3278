package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type workerReleaseFixture struct {
	staticReleaseClient
	listErr        error
	downloadsByURL map[string]int
	list           func()
}

func TestWorkerArtifactsRoutineUpdatePaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"startup", "authenticated startup", "startup critical", "startup incompatible", "automatic", "explicit", "enrolled", "enrolled advanced target", "pending", "download failure", "protocol incompatible", "critical", "windows"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			artifacts, fixture, publish := workerArtifactFixture(t)
			if path == "windows" {
				artifacts.service.cfg.GOOS = "windows"
			}
			publish("1.0.0")
			active, err := artifacts.Acquire(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := active.Release(); err != nil {
					t.Error(err)
				}
			})
			published := publish("1.0.1")
			if path == "authenticated startup" {
				clientConfig := GitHubClientConfig{HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
					recorder := httptest.NewRecorder()
					if request.URL.Host == "api.github.com" {
						if request.Header.Get("Authorization") != "Bearer configured-token" {
							recorder.WriteHeader(http.StatusForbidden)
						} else if err := json.NewEncoder(recorder).Encode(fixture.releases); err != nil {
							return nil, err
						}
					} else {
						data, err := fixture.Download(request.Context(), request.URL.String())
						if err != nil {
							return nil, err
						}
						if _, err := recorder.Write(data); err != nil {
							return nil, err
						}
					}
					return recorder.Result(), nil
				})}}
				artifacts.service.cfg.Client = NewGitHubClient(clientConfig)
				if _, err := artifacts.PrepareLaunch(t.Context(), nil, nil); err == nil || !strings.Contains(err.Error(), "403 Forbidden") {
					t.Fatalf("anonymous discovery = %v, want 403 Forbidden", err)
				}
				clientConfig.Token = "configured-token"
				artifacts.service.cfg.Client = NewGitHubClient(clientConfig)
			}
			updater := &schedulerUpdaterStub{checkStatus: Status{UpdateAvailable: true, LatestVersion: "1.0.1", InstallSource: InstallSourceRelease}, applyStatus: Status{Action: ActionUpdated, LatestVersion: "1.0.1", LatestCommit: testUpdatedCommit, Binary: "verified-controller", BinarySHA256: strings.Repeat("c", 64), VerifiedRelease: true}}
			var drains, restarts int
			preflight := BinaryPreflight(nil)
			if path == "download failure" {
				delete(fixture.downloads, published.Assets[0].BrowserDownloadURL)
			}
			if path == "protocol incompatible" || path == "startup incompatible" {
				preflight = func(context.Context, string) error { return ErrWorkerProtocolIncompatible }
			}
			if path == "critical" || path == "startup critical" {
				fixture.releases[len(fixture.releases)-1].Body = "[detent-critical]"
			}
			now := time.Now().UTC()
			scheduler, err := NewScheduler(SchedulerConfig{WorkerArtifacts: artifacts, WorkerPreflight: preflight, Now: func() time.Time { return now },
				Updater: updater, Enabled: path != "enrolled" && path != "enrolled advanced target", AutoApplyEnabled: true, CheckInterval: time.Hour,
				StatePath: filepath.Join(t.TempDir(), "update.json"), RunningBuild: runnerauth.BuildEvidence{Version: "1.0.0", Commit: testPreviousCommit, Source: "release", SHA256: strings.Repeat("a", 64), OS: "linux", Architecture: "amd64", ObservedAt: now},
				ReserveIdle:    func(context.Context) (func(), bool) { return nil, false },
				ReserveDrain:   func(context.Context) (func(), error) { drains++; return func() {}, nil },
				RequestRestart: func(string) bool { restarts++; return true },
			})
			if err != nil {
				t.Fatal(err)
			}
			switch path {
			case "startup", "authenticated startup", "startup critical", "startup incompatible":
				err = scheduler.PrepareWorkers(t.Context())
			case "automatic":
				_, err = scheduler.CheckNow(t.Context())
			case "pending":
				scheduler.updateStatus(func(status *AutoStatus) { status.State, status.AvailableVersion = "pending_idle", "1.0.1" })
				_, err = scheduler.ApplyPending(t.Context())
			case "enrolled", "enrolled advanced target":
				observed := scheduler.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), nil)
				request := &runnerauth.UpdateRequest{ID: "update-1", Service: "detent", Release: true, FollowHub: true, Version: "1.0.1", ExpectedBuildRevision: observed.Revision, RequestedAt: time.Now().UTC()}
				if path == "enrolled advanced target" {
					publish("1.0.2")
					observed = scheduler.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), request)
					if observed == nil || observed.Validate() != nil || observed.Receipt == nil || observed.Receipt.Status != "refused" || observed.Receipt.Applied != nil {
						t.Fatalf("stale target installed an unauthorized release: %+v", observed)
					}
					resumed, err := NewScheduler(scheduler.cfg)
					if err != nil {
						t.Fatal(err)
					}
					after := resumed.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), nil)
					if !after.Supported || after.Validate() != nil || after.Receipt == nil || after.Receipt.Status != "refused" || after.Receipt.Request != *request {
						t.Fatalf("stale target refusal did not survive restart: %+v", after)
					}
					break
				}
				observed = scheduler.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), request)
				if observed.Receipt == nil || !observed.Receipt.ExecutionReady() || observed.Running.Version != "1.0.0" || observed.Receipt.Applied == nil || observed.Receipt.Applied.Version != "1.0.1" || observed.Receipt.Applied.SHA256 != updater.applyStatus.BinarySHA256 {
					t.Fatalf("untruthful mixed-version update receipt: %+v", observed)
				}
				resumed, resumeErr := NewScheduler(scheduler.cfg)
				if resumeErr != nil {
					t.Fatal(resumeErr)
				}
				if receipt := resumed.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), nil).Receipt; receipt == nil || !receipt.ExecutionReady() {
					t.Fatalf("execution preparation required controller restart after reload: %+v", receipt)
				}
				publish("1.0.2")
				now = now.Add(time.Hour)
				updater.checkStatus.LatestVersion = "1.0.2"
				updater.applyStatus.LatestVersion = "1.0.2"
				observed = scheduler.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), nil)
				if observed.AvailableVersion != "1.0.2" || observed.Discovery != "available" {
					t.Fatalf("later release discovery stalled: %+v", observed)
				}
				request.FollowHub = false
				request.ID, request.Version, request.ExpectedBuildRevision = "update-2", "1.0.2", observed.Revision
				observed = scheduler.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), request)
				if observed.Receipt == nil || !observed.Receipt.ExecutionReady() || observed.Receipt.Applied.Version != "1.0.2" || observed.Running.Version != "1.0.0" || observed.Validate() != nil {
					t.Fatalf("consecutive execution preparation failed: %+v", observed)
				}
			default:
				_, err = scheduler.ApplyRelease(t.Context(), true)
			}
			wantControllerInstalls := 1
			wantState := "applied"
			switch path {
			case "startup", "authenticated startup":
				wantControllerInstalls, wantState = 0, "scheduled"
			case "enrolled advanced target":
				wantControllerInstalls, wantState = 0, "failed"
			case "enrolled":
				wantControllerInstalls = 2
			}
			if path == "download failure" {
				if err == nil || drains != 0 || restarts != 0 {
					t.Fatalf("artifact failure drained or appeared successful: %v, drains=%d restarts=%d", err, drains, restarts)
				}
			} else if err != nil {
				t.Fatal(err)
			} else if path == "protocol incompatible" || path == "critical" || path == "startup critical" || path == "startup incompatible" {
				if drains != 1 || restarts != 1 || updater.applyCalls != 1 {
					t.Fatalf("exceptional update did not use existing replacement: drains=%d restarts=%d applies=%d", drains, restarts, updater.applyCalls)
				}
			} else if drains != 0 || restarts != 0 || updater.applyCalls != wantControllerInstalls || scheduler.Status().State != wantState {
				t.Fatalf("busy routine update drained, restarted or skipped the controller binary: drains=%d restarts=%d applies=%d want %d state=%q want %q", drains, restarts, updater.applyCalls, wantControllerInstalls, scheduler.Status().State, wantState)
			}
			if path == "critical" || path == "protocol incompatible" || path == "startup critical" || path == "startup incompatible" {
				fixture.listErr = errors.New("discovery unavailable")
				if _, err := artifacts.Acquire(t.Context(), nil); err == nil {
					t.Fatal("discovery unexpectedly succeeded")
				}
				if _, err := artifacts.AcquireLaunch(t.Context(), nil, nil); err == nil {
					t.Fatal("known exceptional update allowed new launch")
				}
			} else if path != "download failure" {
				launch, err := artifacts.AcquireLaunch(t.Context(), nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				wantVersion := "1.0.1"
				if path == "enrolled advanced target" {
					wantVersion = "1.0.0"
				}
				if launch.Status.LatestVersion != wantVersion && path != "enrolled" {
					t.Fatal("prepared selection lost")
				}
				if err := launch.Release(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(active.Status.Binary); err != nil {
				t.Fatalf("update removed active worker artifact: %v", err)
			}
		})
	}
}

func TestSchedulerRestartsControllerBehindAppliedRelease(t *testing.T) {
	t.Parallel()
	type heartbeat struct {
		advance time.Duration
		idle    bool
		accept  bool
	}
	for _, test := range []struct {
		name         string
		applyErr     error
		heartbeats   []heartbeat
		wantRestarts []string
		restore      bool
		wantState    string
		wantReserved int
	}{
		{name: "busy controller keeps serving without a deferral drain", heartbeats: []heartbeat{{advance: 7 * time.Hour, accept: true}, {advance: 7 * time.Hour, accept: true}}, wantState: "applied"},
		{name: "idle controller restarts into installed binary", heartbeats: []heartbeat{{advance: time.Minute}, {advance: time.Minute, idle: true, accept: true}, {advance: time.Minute, idle: true, accept: true}}, wantRestarts: []string{"verified-controller"}, wantState: "restart_requested", wantReserved: 1},
		{name: "old launcher recovers applied release at startup", restore: true, heartbeats: []heartbeat{{accept: true}, {idle: true, accept: true}}, wantRestarts: []string{"verified-controller"}, wantState: "restart_requested", wantReserved: 1},
		{name: "declined idle restart retries", heartbeats: []heartbeat{{idle: true}, {idle: true, accept: true}}, wantRestarts: []string{"verified-controller", "verified-controller"}, wantState: "restart_requested", wantReserved: 1},
		{name: "controller install refused never restarts", applyErr: ErrRefused, heartbeats: []heartbeat{{idle: true, accept: true}, {advance: 7 * time.Hour, accept: true}}, wantState: "applied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			artifacts, _, publish := workerArtifactFixture(t)
			publish("1.0.0")
			publish("1.0.1")
			updater := &schedulerUpdaterStub{checkStatus: Status{UpdateAvailable: true, LatestVersion: "1.0.1", InstallSource: InstallSourceRelease}, applyErr: test.applyErr,
				applyStatus: Status{Action: ActionUpdated, LatestVersion: "1.0.1", LatestCommit: testUpdatedCommit, Binary: "verified-controller", BinarySHA256: strings.Repeat("c", 64), VerifiedRelease: true}}
			if test.applyErr != nil {
				updater.applyStatus = Status{Action: ActionRefused}
			}
			now := time.Now().UTC()
			current := heartbeat{accept: true}
			var restarts []string
			reserved := 0
			scheduler, err := NewScheduler(SchedulerConfig{WorkerArtifacts: artifacts, Now: func() time.Time { return now }, Updater: updater, AutoApplyEnabled: true, CheckInterval: time.Hour, MaxDeferral: 6 * time.Hour,
				StatePath: filepath.Join(t.TempDir(), "update.json"), RunningBuild: runnerauth.BuildEvidence{Version: "1.0.0", Commit: testPreviousCommit, Source: "release", SHA256: strings.Repeat("a", 64), OS: "linux", Architecture: "amd64", ObservedAt: now},
				ReserveIdle: func(context.Context) (func(), bool) {
					reserved++
					return func() { reserved-- }, current.idle
				},
				ReserveDrain: func(context.Context) (func(), error) { return nil, errors.New("routine update must not drain") },
				RequestRestart: func(binary string) bool {
					restarts = append(restarts, binary)
					return current.accept
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			observed := scheduler.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), nil)
			request := &runnerauth.UpdateRequest{ID: "update-1", Service: "detent", Release: true, FollowHub: true, Version: "1.0.1", ExpectedBuildRevision: observed.Revision, RequestedAt: now}
			observed = scheduler.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), request)
			if observed.Receipt == nil || observed.Receipt.Applied == nil || !observed.Receipt.Applied.VerifiedRelease || !observed.Receipt.ExecutionArtifact {
				t.Fatalf("routine update lost execution readiness: %+v restarts=%v", observed.Receipt, restarts)
			}
			if len(restarts) != 0 || reserved != 0 {
				t.Fatalf("busy controller requested restart = %v reserved = %d, want no restart while attempts run", restarts, reserved)
			}
			wantInstalls := 1
			if test.restore {
				scheduler, err = NewScheduler(scheduler.cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := scheduler.PrepareWorkers(t.Context()); err != nil {
					t.Fatal(err)
				}
				wantInstalls++
			}
			for _, beat := range test.heartbeats {
				current = beat
				now = now.Add(beat.advance)
				scheduler.EnrolledUpdate(t.Context(), scheduler.RunningBuild(), nil)
			}
			if !slices.Equal(restarts, test.wantRestarts) || scheduler.Status().State != test.wantState {
				t.Fatalf("restarts = %v state = %q, want %v %q", restarts, scheduler.Status().State, test.wantRestarts, test.wantState)
			}
			if updater.applyCalls != wantInstalls {
				t.Fatalf("controller binary installs = %d, want %d", updater.applyCalls, wantInstalls)
			}
			if reserved != test.wantReserved {
				t.Fatalf("held idle reservations = %d, want %d", reserved, test.wantReserved)
			}
		})
	}
}

func (f *workerReleaseFixture) ListReleases(ctx context.Context) ([]Release, error) {
	releases := append([]Release(nil), f.releases...)
	if f.list != nil {
		f.list()
	}
	return releases, f.listErr
}

func (f *workerReleaseFixture) Download(ctx context.Context, url string) ([]byte, error) {
	f.downloadsByURL[url]++
	return f.staticReleaseClient.Download(ctx, url)
}

func workerArtifactFixture(t *testing.T) (*WorkerArtifacts, *workerReleaseFixture, func(string) Release) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	keyID := []byte("12345678")
	publicKey := testMinisignPublicKey(public, keyID)
	f := &workerReleaseFixture{staticReleaseClient: staticReleaseClient{downloads: make(map[string][]byte)}, downloadsByURL: make(map[string]int)}
	service := NewService(Config{CurrentVersion: "1.0.0", GOOS: "linux", GOARCH: "amd64", Client: f,
		ChecksumSignatureVerifier: func(_ context.Context, message, signature []byte) error {
			return VerifyMinisignSignature(publicKey, message, signature)
		},
		BinaryVerifier: func(_ context.Context, path string) (string, error) {
			data, err := os.ReadFile(path)
			return string(data), err
		},
	})
	a, err := NewWorkerArtifacts(service, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publish := func(version string) Release {
		platform := a.service.cfg.GOOS
		archiveName := "detent_" + version + "_" + platform + "_amd64.tar.gz"
		if platform == "windows" {
			archiveName = "detent_" + version + "_windows_amd64.zip"
		}
		checksumName := "detent_" + version + "_checksums.txt"
		root := "https://releases.example.test/" + version + "/"
		content := "version: v" + version + "\ncommit: " + testUpdatedCommit + "\n"
		archive := detentUpdateArchive(t, content)
		if platform == "windows" {
			archive = detentWindowsUpdateArchive(t, content)
		}
		manifest := testReleaseProvenance(t, "v"+version, testUpdatedCommit)
		checksums := fmt.Appendf(nil, "%x  %s\n%x  %s\n", sha256.Sum256(archive), archiveName, sha256.Sum256(manifest), provenanceAssetName)
		f.downloads[root+archiveName] = archive
		f.downloads[root+provenanceAssetName] = manifest
		f.downloads[root+checksumName] = checksums
		f.downloads[root+checksumName+".minisig"] = testMinisignSignature(t, private, keyID, checksums, "worker release")
		release := Release{TagName: "v" + version, Assets: []Asset{
			{Name: archiveName, BrowserDownloadURL: root + archiveName},
			{Name: checksumName, BrowserDownloadURL: root + checksumName},
			{Name: checksumName + ".minisig", BrowserDownloadURL: root + checksumName + ".minisig"},
			{Name: provenanceAssetName, BrowserDownloadURL: root + provenanceAssetName},
		}}
		f.releases = append(f.releases, release)
		return release
	}
	return a, f, publish
}

func TestWorkerArtifactsSelectionAndRetention(t *testing.T) {
	t.Parallel()
	a, f, publish := workerArtifactFixture(t)
	if _, err := a.AcquireLaunch(t.Context(), nil, nil); err == nil {
		t.Fatal("cold launch accepted an unprepared artifact")
	}
	first := publish("1.0.0")
	a1, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	oldArchive := a.latestArchive
	oldBytes, err := os.ReadFile(a1.Status.Binary)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if a1.Status.Binary != a2.Status.Binary || f.downloadsByURL[first.Assets[0].BrowserDownloadURL] != 1 {
		t.Fatal("matching verified artifact was not reused")
	}
	publish("1.0.1")
	b, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	intermediateArchive := a.latestArchive
	publish("1.0.2")
	c, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status.LatestVersion != "1.0.1" || c.Status.LatestVersion != "1.0.2" || !c.Status.VerifiedRelease || c.Status.LatestCommit != testUpdatedCommit || c.Status.BinarySHA256 == "" {
		t.Fatalf("new worker selection did not advance independently of controller: %+v, %+v", b.Status, c.Status)
	}
	if err := b.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b.Status.Binary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired worker artifact remains: %v", err)
	}
	if _, err := os.Stat(intermediateArchive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retired archive remains: %v", err)
	}
	pinned, err := a1.Retain()
	if err != nil {
		t.Fatal(err)
	}
	if err := a1.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := a1.Retain(); err == nil {
		t.Fatal("released handle could acquire a new artifact owner")
	}
	if current, err := os.ReadFile(a1.Status.Binary); err != nil || string(current) != string(oldBytes) {
		t.Fatalf("live old worker artifact changed: %v", err)
	}
	if err := a2.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pinned.Status.Binary); err != nil {
		t.Fatalf("captured continuation lost its artifact: %v", err)
	}
	if _, err := os.Stat(oldArchive); err != nil {
		t.Fatalf("live old archive removed: %v", err)
	}
	if err := pinned.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a1.Status.Binary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old artifact retained after final owner exit: %v", err)
	}
	if _, err := os.Stat(oldArchive); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old archive retained after final owner: %v", err)
	}
	if err := a2.Release(); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Status.Binary); err != nil {
		t.Fatalf("latest verified artifact unavailable: %v", err)
	}
	archives, err := filepath.Glob(filepath.Join(a.root, "archive-*"))
	if err != nil || len(archives) != 1 || archives[0] != a.latestArchive {
		t.Fatalf("latest archive unavailable or obsolete archives retained: %v %v", archives, err)
	}
	f.listErr = errors.New("release source unavailable")
	f.list = func() { t.Error("launch performed release discovery") }
	launch, err := a.AcquireLaunch(t.Context(), nil, nil)
	if err != nil {
		t.Fatalf("prepared launch during discovery outage: %v", err)
	}
	if launch.Status.Binary != c.Status.Binary {
		t.Fatal("launch lost the latest prepared artifact")
	}
	if err := launch.Release(); err != nil {
		t.Fatal(err)
	}
	f.list = nil
	if _, err := a.Acquire(t.Context(), nil); err == nil {
		t.Fatal("unavailable release selection silently reused an obsolete artifact")
	}
}

func TestWorkerArtifactsRejectInvalidLaunch(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"download", "signature", "provenance", "checksum", "identity", "protocol", "cached executable", "cached archive", "platform"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			a, f, publish := workerArtifactFixture(t)
			publish("1.0.0")
			active, err := a.Acquire(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			activeBytes, err := os.ReadFile(active.Status.Binary)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := active.Release(); err != nil {
					t.Error(err)
				}
			}()
			release := publish("1.0.1")
			archive := f.downloads[release.Assets[0].BrowserDownloadURL]
			var preflight BinaryPreflight
			switch failure {
			case "download":
				delete(f.downloads, release.Assets[0].BrowserDownloadURL)
			case "signature":
				f.downloads[release.Assets[2].BrowserDownloadURL] = []byte("invalid signature")
			case "provenance":
				f.downloads[release.Assets[3].BrowserDownloadURL] = testReleaseProvenance(t, "v2.0.0", testUpdatedCommit)
			case "checksum":
				f.downloads[release.Assets[0].BrowserDownloadURL] = []byte("invalid archive")
			case "identity":
				a.service.cfg.BinaryVerifier = func(context.Context, string) (string, error) {
					return "version: v1.0.0\ncommit: " + testPreviousCommit + "\n", nil
				}
			case "protocol":
				preflight = func(context.Context, string) error { return errors.New("incompatible execution protocol") }
			case "cached executable":
				f.releases = f.releases[:1]
				if err := os.WriteFile(active.Status.Binary, []byte("corrupt"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "cached archive":
				f.releases = f.releases[:1]
				release = f.releases[0]
				archive = f.downloads[release.Assets[0].BrowserDownloadURL]
				f.downloads[release.Assets[0].BrowserDownloadURL] = []byte("corrupt source")
				entries, err := os.ReadDir(a.root)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), "archive-") {
						if err := os.WriteFile(filepath.Join(a.root, entry.Name()), []byte("corrupt"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "platform":
				a.service.cfg.GOARCH = "unsupported"
			}
			if _, err := a.Acquire(t.Context(), preflight); err == nil {
				t.Fatal("invalid selected launch accepted")
			}
			if _, err := os.Stat(active.Status.Binary); err != nil {
				t.Fatalf("failed launch removed live old artifact: %v", err)
			}
			if failure != "cached executable" {
				current, err := os.ReadFile(active.Status.Binary)
				if err != nil || string(current) != string(activeBytes) {
					t.Fatalf("failed launch modified live old artifact: %v", err)
				}
			}
			if failure == "download" || failure == "checksum" || failure == "cached archive" {
				f.downloads[release.Assets[0].BrowserDownloadURL] = archive
				retried, err := a.Acquire(t.Context(), nil)
				if err != nil {
					t.Fatalf("failed download poisoned subsequent verified launch: %v", err)
				}
				if retried.Status.LatestVersion != strings.TrimPrefix(release.TagName, "v") {
					t.Fatalf("replacement selected obsolete release %s", retried.Status.LatestVersion)
				}
				if err := retried.Release(); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(a.root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".") {
					t.Fatalf("failed launch leaked staging artifact %s", entry.Name())
				}
			}
		})
	}
}

func TestWorkerArtifactsConcurrentOwners(t *testing.T) {
	t.Parallel()
	a, f, publish := workerArtifactFixture(t)
	publish("1.0.0")
	old, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	discovering, resume := make(chan struct{}), make(chan struct{})
	f.list = func() { close(discovering); <-resume }
	preparedResult := make(chan error, 1)
	go func() {
		prepared, err := a.Acquire(t.Context(), nil)
		if err == nil {
			err = prepared.Release()
		}
		preparedResult <- err
	}()
	<-discovering
	launchResult := make(chan error, 1)
	go func() {
		launch, err := a.AcquireLaunch(t.Context(), nil, nil)
		if err == nil {
			err = launch.Release()
		}
		launchResult <- err
	}()
	select {
	case err := <-launchResult:
		if err != nil {
			t.Error(err)
		}
	case <-t.Context().Done():
		t.Error("prepared launch blocked on discovery")
	}
	close(resume)
	if err := <-preparedResult; err != nil {
		t.Fatal(err)
	}
	f.list = nil
	release := publish("1.0.1")
	prepared, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Release(); err != nil {
		t.Fatal(err)
	}
	f.listErr = errors.New("GitHub returned 403 Forbidden")
	f.list = func() { t.Error("concurrent launch performed discovery") }
	const count = 8
	var owners [count]*WorkerArtifact
	var failures [count]error
	var workers sync.WaitGroup
	for i := range count {
		workers.Go(func() {
			owners[i], failures[i] = a.AcquireLaunch(t.Context(), nil, nil)
		})
	}
	workers.Wait()
	for i, failure := range failures {
		if failure != nil {
			t.Fatalf("acquire %d: %v", i, failure)
		}
		if owners[i].Status.LatestVersion != "1.0.1" || owners[i].Status.Binary != owners[0].Status.Binary {
			t.Fatalf("concurrent owner %d has wrong artifact: %+v", i, owners[i].Status)
		}
	}
	if f.downloadsByURL[release.Assets[0].BrowserDownloadURL] != 1 {
		t.Fatal("concurrent owners downloaded the verified archive repeatedly")
	}
	for range count {
		workers.Go(func() {
			if err := owners[0].Release(); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	f.listErr, f.list = nil, nil
	publish("1.0.2")
	latest, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owners[1].Status.Binary); err != nil {
		t.Fatalf("repeated release deleted another owner's artifact: %v", err)
	}
	for i := 1; i < count; i++ {
		workers.Go(func() {
			if err := owners[i].Release(); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	if _, err := os.Stat(owners[0].Status.Binary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("concurrent final releases did not remove retired artifact: %v", err)
	}
	if _, err := os.Stat(old.Status.Binary); err != nil {
		t.Fatalf("concurrent releases removed unrelated live artifact: %v", err)
	}
	if err := old.Release(); err != nil {
		t.Fatal(err)
	}
	if err := latest.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerArtifactsReleaseSelectionBoundary(t *testing.T) {
	t.Parallel()
	a, f, publish := workerArtifactFixture(t)
	publish("1.0.0")
	f.list = func() { publish("1.0.1"); f.list = nil }
	first, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status.LatestVersion != "1.0.0" || second.Status.LatestVersion != "1.0.1" {
		t.Fatalf("publication after release snapshot changed captured selection: %s, %s", first.Status.LatestVersion, second.Status.LatestVersion)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}
