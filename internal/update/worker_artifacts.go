package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type WorkerArtifacts struct {
	service *Service
	root    string
	mu      sync.Mutex
	live    map[string]int
	latest  string
}

type WorkerArtifact struct {
	Status  Status
	release func() error
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
	a.mu.Lock()
	defer a.mu.Unlock()
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
	if err := os.MkdirAll(a.root, 0o700); err != nil {
		return nil, fmt.Errorf("create worker artifact cache: %w", err)
	}
	service := *a.service
	assets, err := SelectReleaseAssets(release, service.cfg.GOOS, service.cfg.GOARCH)
	if err != nil {
		return nil, fmt.Errorf("select worker platform: %w", err)
	}
	service.cfg.Client = &workerArchiveCache{ReleaseClient: service.cfg.Client, root: a.root, assets: assets}
	status := Status{CurrentVersion: service.cfg.CurrentVersion, LatestVersion: displayVersion(release.TagName), LatestTag: release.TagName, Critical: releaseCritical(release)}
	status, binary, mode, err := service.prepareReleaseBinary(ctx, status, release)
	if err != nil {
		return nil, fmt.Errorf("verify worker release: %w", err)
	}
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
	if err := ReplaceBinary(ctx, Replacement{Target: target, Binary: binary, Mode: mode, GOOS: service.cfg.GOOS, Verify: identity, Preflight: candidateIdentityPreflight(identity, preflight), Sign: service.cfg.BinarySigner}); err != nil {
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
	a.latest = directory
	a.live[directory]++
	var once sync.Once
	var releaseErr error
	return &WorkerArtifact{Status: status, release: func() error {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.live[directory] > 1 {
				a.live[directory]--
				return
			}
			delete(a.live, directory)
			releaseErr = a.pruneLocked()
		})
		return releaseErr
	}}, nil
}

func (a *WorkerArtifacts) pruneLocked() error {
	entries, err := os.ReadDir(a.root)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		key := strings.TrimPrefix(entry.Name(), "worker-")
		digest, err := hex.DecodeString(key)
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "worker-") || err != nil || len(digest) != sha256.Size {
			continue
		}
		path := filepath.Join(a.root, entry.Name())
		if path != a.latest && a.live[path] == 0 {
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
	if data, err := os.ReadFile(path); err == nil {
		return data, nil
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
