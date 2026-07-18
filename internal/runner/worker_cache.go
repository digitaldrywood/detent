package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/workspace"
)

const sharedCacheKeyDigestLength = 12

func configureWorkerCache(request *AgentTurnRequest) error {
	if request == nil {
		return nil
	}

	strategy := strings.ToLower(strings.TrimSpace(request.cacheStrategy))
	if strategy == "" {
		strategy = config.WorkspaceCacheIsolated
	}

	root := request.TempDir
	switch strategy {
	case config.WorkspaceCacheIsolated:
	case config.WorkspaceCacheShared:
		root = sharedWorkerCacheRoot(request.Workspace, request.projectID)
	default:
		return fmt.Errorf("unsupported cache strategy %q", request.cacheStrategy)
	}
	if strings.TrimSpace(root) == "" {
		return errors.New("worker cache root is empty")
	}

	goBuild := filepath.Join(root, "go-build")
	goMod := filepath.Join(root, "go-mod")
	goBin := filepath.Join(root, "go-bin")
	golangCILint := filepath.Join(root, "golangci-lint")
	for _, path := range []string{goBuild, goMod, goBin, golangCILint} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create %s cache: %w", filepath.Base(path), err)
		}
	}

	variables := make(map[string]string, len(request.Environment.Variables)+4)
	for key, value := range request.Environment.Variables {
		variables[key] = value
	}
	variables["GOCACHE"] = goBuild
	variables["GOMODCACHE"] = goMod
	variables["GOBIN"] = goBin
	variables["GOLANGCI_LINT_CACHE"] = golangCILint
	request.Environment.Variables = variables
	request.Environment.PathSuffixes = appendUniquePath(request.Environment.PathSuffixes, goBin)
	if strategy == config.WorkspaceCacheShared {
		request.ExtraWritableRoots = appendUniquePath(request.ExtraWritableRoots, root)
	}
	return nil
}

func sharedWorkerCacheRoot(workspacePath string, projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		projectID = defaultProjectID
	}
	sum := sha256.Sum256([]byte(projectID))
	key := workspace.SafeKey(projectID) + "-" + hex.EncodeToString(sum[:])[:sharedCacheKeyDigestLength]
	return filepath.Join(filepath.Dir(filepath.Clean(workspacePath)), ".detent", "cache", key)
}

func appendUniquePath(paths []string, candidate string) []string {
	for _, path := range paths {
		if filepath.Clean(path) == filepath.Clean(candidate) {
			return paths
		}
	}
	return append(paths, candidate)
}
