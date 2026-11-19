package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/digitaldrywood/detent/internal/workspace/internal/workpath"
)

const workerScratchBaseName = workpath.WorkerScratchBaseName

func WorkerScratchRoot(path string) string              { return workpath.WorkerScratchRoot(path) }
func workerScratchRoots(path string) []string           { return workpath.WorkerScratchRoots(path) }
func workerScratchGroup(path string) string             { return workpath.WorkerScratchGroup(path) }
func workerScratchGroupIn(base, path string) string     { return workpath.WorkerScratchGroupIn(base, path) }
func workerScratchBase() string                         { return workpath.WorkerScratchBase() }
func sharedWorkerScratchBase() string                   { return workpath.SharedWorkerScratchBase() }
func bestEffortCanonicalPath(path string) string        { return workpath.BestEffortCanonicalPath(path) }
func canonicalExistingPath(path string) (string, error) { return workpath.CanonicalExistingPath(path) }
func expandPath(path string) (string, error)            { return workpath.ExpandPath(path) }
func pathWithin(root, path string) bool                 { return workpath.PathWithin(root, path) }

type AttemptWorkspace struct {
	Organization string
	Project      string
	Attempt      string
}

func (a AttemptWorkspace) Key() string {
	id, typed := strings.CutPrefix(a.Attempt, "attempt_")
	if a.Organization == "" || a.Project == "" || !typed || len(id) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ""
	}
	ids, _ := json.Marshal([2]string{a.Organization, a.Project})
	digest := sha256.Sum256(ids)
	return "attempt-" + hex.EncodeToString(digest[:16]) + "-" + strings.ToLower(id)
}

func (a AttemptWorkspace) PrepareScratch(root string) (string, error) {
	key := a.Key()
	if key == "" {
		return "", errors.New("worker scratch requires an organization, project and attempt")
	}
	parent := filepath.Join(root, key+"-execution")
	if err := ensurePrivateDirectories(root, parent); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, "worker-")
}

type attemptWorkspaceContextKey struct{}

func WithAttemptWorkspace(ctx context.Context, attempt AttemptWorkspace) context.Context {
	if attempt.Key() == "" {
		return ctx
	}
	return context.WithValue(ctx, attemptWorkspaceContextKey{}, attempt)
}

func AttemptWorkspaceFromContext(ctx context.Context) AttemptWorkspace {
	attempt, _ := ctx.Value(attemptWorkspaceContextKey{}).(AttemptWorkspace)
	return attempt
}

func checkoutIssueKey(key string) string {
	owner, issue, ok := strings.Cut(key, "--")
	if !ok || issue == "" {
		return key
	}
	owner, ok = strings.CutPrefix(owner, "attempt-")
	if !ok {
		return key
	}
	scope, attempt, ok := strings.Cut(owner, "-")
	if !ok || len(scope) != 32 || len(attempt) != 32 {
		return key
	}
	if _, err := hex.DecodeString(scope + attempt); err != nil {
		return key
	}
	return issue
}
