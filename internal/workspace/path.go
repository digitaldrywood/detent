package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"

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
	return workpath.AttemptKey(a.Organization, a.Project, a.Attempt)
}

func workspaceIssueKey(issue Issue) string {
	key := issueKey(issue)
	if attempt := issue.AttemptWorkspace.Key(); attempt != "" {
		return attempt + "--" + key
	}
	return key
}

func (a AttemptWorkspace) permitsCheckout(key string) bool {
	owner, _ := splitCheckoutIssueKey(key)
	return owner == "" || owner == a.Key()
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

func AttemptWorkspaceFromContext(ctx context.Context) AttemptWorkspace {
	if ctx != nil {
		if attempt, ok := ctx.Value(attemptWorkspaceContextKey{}).(AttemptWorkspace); ok {
			return attempt
		}
	}
	return AttemptWorkspace{}
}

func splitCheckoutIssueKey(key string) (string, string) {
	return workpath.SplitCheckoutIssueKey(key)
}
