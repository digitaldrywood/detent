package workspace

import (
	"path/filepath"
	"sync"
)

// openSessions counts the workspace sessions holding each worktree path in this
// process. Cleanup and residual reconciliation leave a held worktree alone: a
// session serving files or git starts no process in it, so the process scan
// cannot see that it is in use.
var openSessions = struct {
	sync.Mutex
	paths map[string]int
}{paths: map[string]int{}}

// HoldSession marks path as served by an open workspace session until the
// returned release is called. Release is safe to call more than once.
func HoldSession(path string) func() {
	key := filepath.Clean(path)
	openSessions.Lock()
	openSessions.paths[key]++
	openSessions.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			openSessions.Lock()
			defer openSessions.Unlock()
			if openSessions.paths[key]--; openSessions.paths[key] <= 0 {
				delete(openSessions.paths, key)
			}
		})
	}
}

// sessionHeld reports whether an open workspace session holds path.
func sessionHeld(path string) bool {
	openSessions.Lock()
	defer openSessions.Unlock()
	return openSessions.paths[filepath.Clean(path)] > 0
}
