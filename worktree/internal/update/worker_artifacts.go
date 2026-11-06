package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

var ErrWorkerProtocolIncompatible = errors.New("execution worker protocol is incompatible with the controller")
var ErrWorkerCriticalUpdate = errors.New("critical security release requires the existing exceptional update drain")

type WorkerArtifacts struct {
	service       *Service
	root          string
	mu            sync.Mutex
	prepareMu     sync.Mutex
	live          map[string]int
	latest        string
	latestArchive string
	prepared      Status
	refusal       error
}

type WorkerArtifact struct {
	binary  string
	Status  Status
	release func() error
	retain  func() (*WorkerArtifact, error)
}

func (a *WorkerArtifact) Command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if a == nil || a.binary == "" {
		return nil, errors.New("execution command requires a verified artifact")
	}
	return ReleaseCommand(ctx, a.binary, args...), nil
}

func (a *WorkerArtifact) Retain() (*WorkerArtifact, error) {
	if a == nil || a.retain == nil {
		return nil, errors.New("execution artifact is no longer owned")
	}
	return a.retain()
}

func NewWorkerArtifacts(service *Service, root string) (*WorkerArtifacts, error) {
	if service == nil || !filepath.IsAbs(root) {
		return nil, errors.New("worker artifacts require a release service and an absolute cache root")
	}
	return &WorkerArtifacts{service: service, root: root, live: make(map[string]int)}, nil
}

func (a *WorkerArtifact) Release() error {
	if a == nil || a.release == nil {
		return nil
	}
	return a.release()
}

func (a *WorkerArtifacts) Acquire(ctx context.Context, preflight BinaryPreflight) (*WorkerArtifact, error) {
	return a.PrepareLaunch(ctx, preflight, nil)
}

func (a *WorkerArtifacts) AcquireLaunch(ctx context.Context, preflight BinaryPreflight, capture func(Status) error) (*WorkerArtifact, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a.refusal != nil {
		return nil, a.refusal
	}
	if !a.prepared.VerifiedRelease {
		return nil, errors.New("no verified execution artifact has been prepared")
	}
	if capture != nil {
		if err := capture(a.prepared); err != nil {
			return nil, fmt.Errorf("capture worker launch: %w", err)
		}
	}
	data, err := os.ReadFile(a.prepared.Binary)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != a.prepared.BinarySHA256 {
		return nil, errors.New("prepared execution artifact failed integrity verification")
	}
	if preflight != nil {
		if err := preflight(ctx, a.prepared.Binary); err != nil {
			return nil, err
		}
	}
	return a.retainLocked(a.prepared, a.latest, a.latestArchive), nil
}

func (a *WorkerArtifacts) PrepareLaunch(ctx context.Context, preflight BinaryPreflight, capture func(Status) error) (*WorkerArtifact, error) {
	a.prepareMu.Lock()
	defer a.prepareMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	releases, err := a.service.cfg.Client.ListReleases(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve worker release: %w", err)
	}
	release, found, err := SelectLatestRelease(a.service.cfg.CurrentVersion, releases)
	if err != nil || !found {
		return nil, fmt.Errorf("resolve worker release: %w", errors.Join(err, errors.New("no eligible worker release")))
	}
	status := Status{CurrentVersion: a.service.cfg.CurrentVersion, LatestVersion: displayVersion(release.TagName), LatestTag: release.TagName, Critical: releaseCritical(release)}
	if capture != nil {
		if err := capture(status); err != nil {
			if errors.Is(err, ErrWorkerCriticalUpdate) {
				a.mu.Lock()
				a.refusal = err
				a.mu.Unlock()
			}
			return nil, fmt.Errorf("capture worker launch: %w", err)
		}
	}
	if err := os.MkdirAll(a.root, 0o700); err != nil {
		return nil, fmt.Errorf("create worker artifact cache: %w", err)
	}
	service := *a.service
	assets, err := SelectReleaseAssets(release, service.cfg.GOOS, service.cfg.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("select worker platform: %w", err)
	}
	cache := &workerArchiveCache{ReleaseClient: service.cfg.Client, root: a.root, assets: assets}
	service.cfg.Client = cache
	status, prepared, err := service.prepareReleaseBinary(ctx, status, release)
	if err != nil {
		return nil, fmt.Errorf("verify worker release: %w", err)
	}
	binary, mode := prepared.binary, prepared.mode
	staging, err := os.MkdirTemp(a.root, ".install-")
	if err != nil {
		return nil, fmt.Errorf("stage worker artifact: %w", err)
	}
	defer removeWorkerStaging(staging)
	name := projectName
	if service.cfg.GOOS == "windows" {
		name = windowsExecutableName
	}
	identity := binaryIdentityVerifier(service.cfg.BinaryVerifier, status.LatestVersion, status.LatestCommit)
	target := filepath.Join(staging, name)
	if err := replaceBinaryNow(ctx, Replacement{Context: ctx, Target: target, Binary: binary, Mode: mode, GOOS: service.cfg.GOOS, Verify: identity, Preflight: candidateIdentityPreflight(identity, preflight), Sign: service.cfg.BinarySigner}); err != nil {
		if errors.Is(err, ErrWorkerProtocolIncompatible) {
			a.mu.Lock()
			a.refusal = err
			a.mu.Unlock()
		}
		return nil, fmt.Errorf("prepare worker executable: %w", err)
	}
	installed, err := os.ReadFile(target)
	if err != nil {
		return nil, fmt.Errorf("read worker executable: %w", err)
	}
	digest := sha256.Sum256(installed)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	status.BinarySHA256 = hex.EncodeToString(digest[:])
	a.mu.Lock()
	defer a.mu.Unlock()
	directory := filepath.Join(a.root, "worker-"+status.BinarySHA256)
	path := filepath.Join(directory, name)
	if existing, err := os.ReadFile(path); err == nil {
		if sha256.Sum256(existing) != digest {
			return nil, errors.New("cached worker executable failed integrity verification")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read cached worker executable: %w", err)
	} else if err := os.Rename(staging, directory); err != nil {
		return nil, fmt.Errorf("install immutable worker artifact: %w", err)
	}
	status.Binary = path
	status.FailureReason = ""
	status.VerifiedRelease = true
	status.Action = ActionUpdated
	a.latest, a.latestArchive = directory, cache.path
	a.prepared, a.refusal = status, nil
	return a.retainLocked(status, directory, cache.path), nil
}

func (a *WorkerArtifacts) retainLocked(status Status, directory, archive string) *WorkerArtifact {
	a.live[directory]++
	a.live[archive]++
	released := false
	return &WorkerArtifact{Status: status, binary: status.Binary, retain: func() (*WorkerArtifact, error) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if released || a.live[directory] == 0 {
			return nil, errors.New("execution artifact is no longer owned")
		}
		data, err := os.ReadFile(status.Binary)
		if err != nil {
			return nil, err
		}
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) != status.BinarySHA256 {
			return nil, errors.New("retained execution artifact failed integrity verification")
		}
		return a.retainLocked(status, directory, archive), nil
	}, release: func() error {
		a.mu.Lock()
		defer a.mu.Unlock()
		if !released {
			released = true
			for _, path := range []string{directory, archive} {
				if a.live[path] > 1 {
					a.live[path]--
				} else {
					delete(a.live, path)
				}
			}
		}
		return a.pruneLocked()
	}}
}

func (a *WorkerArtifacts) pruneLocked() error {
	if !a.prepareMu.TryLock() {
		return nil
	}
	defer a.prepareMu.Unlock()
	entries, err := os.ReadDir(a.root)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		prefix := "archive-"
		if entry.IsDir() {
			prefix = "worker-"
		}
		key := strings.TrimPrefix(entry.Name(), prefix)
		digest, err := hex.DecodeString(key)
		if !strings.HasPrefix(entry.Name(), prefix) || err != nil || len(digest) != sha256.Size {
			continue
		}
		path := filepath.Join(a.root, entry.Name())
		if path != a.latest && path != a.latestArchive && a.live[path] == 0 {
			errs = append(errs, os.RemoveAll(path))
		}
	}
	return errors.Join(errs...)
}

func removeWorkerStaging(path string) {
	removeErr := os.RemoveAll(path)
	discardUpdateError(removeErr)
}

type workerArchiveCache struct {
	ReleaseClient
	root      string
	assets    ReleaseAssets
	checksums []byte
	path      string
}

func (c *workerArchiveCache) Download(ctx context.Context, url string) ([]byte, error) {
	if url != c.assets.Archive.BrowserDownloadURL {
		data, err := c.ReleaseClient.Download(ctx, url)
		if url == c.assets.Checksum.BrowserDownloadURL && err == nil {
			c.checksums = data
		}
		return data, err
	}
	expected, found := expectedChecksum(c.checksums, c.assets.Archive.Name)
	if !found {
		return nil, errors.New("worker archive checksum is unavailable")
	}
	key := sha256.Sum256([]byte(expected))
	path := filepath.Join(c.root, "archive-"+hex.EncodeToString(key[:]))
	c.path = path
	if data, err := os.ReadFile(path); err == nil {
		if VerifyChecksum(c.checksums, c.assets.Archive.Name, data) == nil {
			return data, nil
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove corrupt worker archive: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err := c.ReleaseClient.Download(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := VerifyChecksum(c.checksums, c.assets.Archive.Name, data); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(c.root, ".archive-")
	if err != nil {
		return nil, err
	}
	defer removeFile(file.Name())
	if err := writeBinaryTemp(file, data, 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return nil, err
	}
	return data, nil
}
