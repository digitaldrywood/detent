package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type workerReleaseFixture struct {
	staticReleaseClient
	listErr        error
	downloadsByURL map[string]int
	list           func()
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
		archiveName := "detent_" + version + "_linux_amd64.tar.gz"
		checksumName := "detent_" + version + "_checksums.txt"
		root := "https://releases.example.test/" + version + "/"
		archive := detentUpdateArchive(t, "version: v"+version+"\ncommit: "+testUpdatedCommit+"\n")
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
	first := publish("1.0.0")
	a1, err := a.Acquire(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
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
	if err := a1.Release(); err != nil {
		t.Fatal(err)
	}
	if current, err := os.ReadFile(a1.Status.Binary); err != nil || string(current) != string(oldBytes) {
		t.Fatalf("live old worker artifact changed: %v", err)
	}
	if err := a2.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a1.Status.Binary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old artifact retained after final owner exit: %v", err)
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
	f.listErr = errors.New("release source unavailable")
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
			if failure == "download" || failure == "checksum" {
				f.downloads[release.Assets[0].BrowserDownloadURL] = archive
				retried, err := a.Acquire(t.Context(), nil)
				if err != nil {
					t.Fatalf("failed download poisoned subsequent verified launch: %v", err)
				}
				if retried.Status.LatestVersion != "1.0.1" {
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
	release := publish("1.0.1")
	const count = 8
	var owners [count]*WorkerArtifact
	var failures [count]error
	var workers sync.WaitGroup
	for i := range count {
		workers.Go(func() {
			owners[i], failures[i] = a.Acquire(t.Context(), nil)
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
