package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/digitaldrywood/detent/internal/workspace/internal/workpath"
)

type pathLeaseRegistry struct {
	mu       sync.Mutex
	holders  map[string]int
	cleaning map[string]chan struct{}
	onWait   func(string)
}

func (r *pathLeaseRegistry) Use(ctx context.Context, path string) (func(), error) {
	clean := filepath.Clean(path)
	for {
		r.mu.Lock()
		ch := r.cleaning[clean]
		if ch == nil {
			if r.holders == nil {
				r.holders = make(map[string]int)
			}
			r.holders[clean]++
			r.mu.Unlock()
			var once sync.Once
			return func() {
				once.Do(func() {
					r.mu.Lock()
					if r.holders[clean] <= 1 {
						delete(r.holders, clean)
					} else {
						r.holders[clean]--
					}
					r.mu.Unlock()
				})
			}, nil
		}
		onWait := r.onWait
		r.mu.Unlock()
		if onWait != nil {
			onWait(clean)
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (r *pathLeaseRegistry) Held(path string) bool {
	clean := filepath.Clean(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.holders[clean] > 0
}

func (r *pathLeaseRegistry) TryCleanup(path string) (func(), bool) {
	clean := filepath.Clean(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.holders[clean] > 0 || r.cleaning[clean] != nil {
		return nil, false
	}
	if r.cleaning == nil {
		r.cleaning = make(map[string]chan struct{})
	}
	ch := make(chan struct{})
	r.cleaning[clean] = ch
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.cleaning, clean)
			close(ch)
			r.mu.Unlock()
		})
	}, true
}

var executionUses pathLeaseRegistry

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

func ensurePrivateDirectories(base, path string) error {
	return workpath.EnsurePrivateDirectories(base, path)
}

func pathExists(path string) (bool, bool, error) { return workpath.PathExists(path) }
func dirIsEmpty(path string) (bool, error)       { return workpath.DirIsEmpty(path) }

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

func (a AttemptWorkspace) AcquireScratch(ctx context.Context, root string) (string, func(), error) {
	key := a.Key()
	if key == "" {
		return "", nil, errors.New("worker scratch requires an organization, project and attempt")
	}
	parent := filepath.Clean(filepath.Join(root, key+"-execution"))
	release, err := executionUses.Use(ctx, parent)
	if err != nil {
		return "", nil, err
	}
	if err := ensurePrivateDirectories(root, parent); err != nil {
		release()
		return "", nil, err
	}
	scratch, err := os.MkdirTemp(parent, "worker-")
	if err != nil {
		release()
		return "", nil, err
	}
	return scratch, release, nil
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
