//go:build !windows

package workspacerunner_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// releaseProbeWorktree records whether an action process was still alive when
// the session released its worktree.
type releaseProbeWorktree struct {
	path string
	mu   sync.Mutex
	// aliveAtRelease is set when the pid the action wrote still named a live
	// process at the moment of release.
	aliveAtRelease bool
	released       bool
}

func (w *releaseProbeWorktree) Prepare(context.Context, string, hubclient.WorkspaceCheckout) (string, error) {
	return w.path, nil
}

func (w *releaseProbeWorktree) Release(context.Context, string, hubclient.WorkspaceCheckout) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.released = true
	data, err := os.ReadFile(filepath.Join(w.path, "action.pid"))
	if err != nil {
		return nil
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err == nil && syscall.Kill(pid, 0) == nil {
		w.aliveAtRelease = true
	}
	return nil
}

func (w *releaseProbeWorktree) state() (bool, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.released, w.aliveAtRelease
}

// A command that ignores SIGTERM keeps running for the kill grace after its
// run is cancelled, which is exactly the window a release must not start in.
const stubbornAction = "trap '' TERM; echo $$ > action.pid; while :; do sleep 0.05; done"

func awaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func TestSessionWaitsForActionsBeforeReleasingTheWorktree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		stop func(f *sessionFixture)
	}{
		{name: "the runner shuts down", stop: func(f *sessionFixture) { f.cancel() }},
		{name: "the hub closes the workspace", stop: func(f *sessionFixture) { f.hub.setState(workspacesession.StateClosing) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := worktreeWith(t)
			probe := &releaseProbeWorktree{path: root}
			f := startSessionWith(t, root, nil, func(config *workspacerunner.Config) {
				config.Worktree = probe
				config.Shell = "/bin/sh"
			})
			f.send(t, execRunFrame(t, "conn:1", stubbornAction))
			awaitFile(t, filepath.Join(root, "action.pid"))
			test.stop(f)
			if err := f.wait(t, 20*time.Second); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("session ended with %v", err)
			}
			released, alive := probe.state()
			if !released {
				t.Fatal("the worktree was never released")
			}
			if alive {
				t.Fatal("the worktree was released while its action was still running")
			}
		})
	}
}

func TestSessionStopsASetupActionWhenTheLeaseIsLost(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	t.Parallel()
	root := worktreeWith(t)
	hub := newScriptedHub(t, creationCheckout(workspacesession.WorktreeFresh))
	hub.actions = []workspacesession.Action{{ID: "act_setup", Name: "Setup", Command: stubbornAction}}
	hub.heartbeatErr = fmt.Errorf("%w: gone", hubclient.ErrStaleWorkspace)
	reporter := &recordingReporter{}
	probe := &releaseProbeWorktree{path: root}
	session, err := workspacerunner.New(workspacerunner.Config{
		WorkspaceID: testWorkspaceID,
		Identity:    hubclient.WorkspaceIdentity{LeaseID: tracker.LeaseID("lease-1"), FencingToken: 7},
		Hub:         hub, Worktree: probe, Logger: discardLogger(), Shell: "/bin/sh", Reporter: reporter,
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Run(t.Context()) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("a setup action outlived the lease")
	}
	if unbound, reason := hub.unbindWith(); !unbound || reason != workspacesession.ReasonLeaseLost {
		t.Fatalf("unbind = %v, %q, want lease_lost", unbound, reason)
	}
	if _, alive := probe.state(); alive {
		t.Fatal("the worktree was released while the setup action was still running")
	}
	reports := reporter.snapshot()
	if len(reports) == 0 {
		t.Fatal("the setup action was never reported")
	}
	last := reports[len(reports)-1]
	if last.Status != workspacesession.RunFailed || last.Reason != workspacesession.RunReasonLeaseLost {
		t.Fatalf("setup report = %+v, want failed with lease_lost", last)
	}
}
