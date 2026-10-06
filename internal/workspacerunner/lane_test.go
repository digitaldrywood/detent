package workspacerunner_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// scriptedClaimer hands the lane a fixed sequence of claims and records what it
// released. It is the whole scheduling side, because what the lane has to get
// right is what it does with a claim rather than how it got one.
type scriptedClaimer struct {
	mu sync.Mutex
	// claims is consumed in order; an exhausted claimer reports no work, the
	// steady state of an idle runner.
	claims   []claimResult
	sessions []workspacesession.Session
	resolve  error
	release  error
	released []string
	calls    int
}

type claimResult struct {
	lease tracker.NativeLease
	err   error
}

func (c *scriptedClaimer) ClaimWorkspace(context.Context) (tracker.NativeLease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if len(c.claims) == 0 {
		return tracker.NativeLease{}, hubclient.ErrNoClaimableWork
	}
	next := c.claims[0]
	c.claims = c.claims[1:]
	return next.lease, next.err
}

func (c *scriptedClaimer) WorkspaceForWorkItem(context.Context, tracker.NativeWorkItemID, hubclient.WorkspaceIdentity) (workspacesession.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolve != nil {
		return workspacesession.Session{}, c.resolve
	}
	if len(c.sessions) == 0 {
		return workspacesession.Session{}, errors.New("no session scripted")
	}
	next := c.sessions[0]
	c.sessions = c.sessions[1:]
	return next, nil
}

func (c *scriptedClaimer) ReleaseWorkspaceLease(_ context.Context, lease tracker.NativeLease, reason string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = append(c.released, string(lease.ID)+":"+reason)
	return c.release
}

func (c *scriptedClaimer) releases() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.released...)
}

func lease(id string) tracker.NativeLease {
	return tracker.NativeLease{ID: tracker.LeaseID(id), WorkItemID: "wi_1", FencingToken: 3}
}

// runLane starts a lane and stops it once the condition holds.
func runLane(t *testing.T, claimer *scriptedClaimer, hub workspacerunner.Hub, until func() bool) {
	t.Helper()
	runLaneLogging(t, claimer, hub, discardLogger(), until)
}

// runLaneLogging is runLane with the logger the test wants to read.
func runLaneLogging(t *testing.T, claimer *scriptedClaimer, hub workspacerunner.Hub, logger *slog.Logger, until func() bool) {
	t.Helper()
	lane, err := workspacerunner.NewLane(workspacerunner.LaneConfig{
		Claimer: claimer, Hub: hub, Worktree: &fixedWorktree{path: t.TempDir()},
		Logger: logger, Poll: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = lane.Run(ctx)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !until() {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("the lane did not reach the expected state")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the lane did not stop when its context was cancelled")
	}
}

func TestLaneReleasesAClaimItCannotResolve(t *testing.T) {
	t.Parallel()
	claimer := &scriptedClaimer{
		claims:  []claimResult{{lease: lease("lease-unresolvable")}},
		resolve: errors.New("not a workspace"),
	}
	hub := newScriptedHub(t, hubclient.WorkspaceCheckout{WorkItemID: "wi_1", HeartbeatSeconds: 1})
	// Holding a claim it cannot act on would park the runner on work it
	// cannot do, so the lease goes straight back.
	runLane(t, claimer, hub, func() bool { return len(claimer.releases()) == 1 })
	if got := claimer.releases()[0]; got != "lease-unresolvable:work_item_identity_missing" {
		t.Fatalf("release = %q", got)
	}
}

func TestLaneReleasesAWorkspaceThatAlreadyEnded(t *testing.T) {
	t.Parallel()
	claimer := &scriptedClaimer{
		claims:   []claimResult{{lease: lease("lease-ended")}},
		sessions: []workspacesession.Session{{ID: testWorkspaceID, State: workspacesession.StateClosed}},
	}
	hub := newScriptedHub(t, hubclient.WorkspaceCheckout{WorkItemID: "wi_1", HeartbeatSeconds: 1})
	// A workspace closed between the request and the claim is not a failure;
	// the lane gives the slot back and asks again.
	runLane(t, claimer, hub, func() bool { return len(claimer.releases()) == 1 })
	if got := claimer.releases()[0]; got != "lease-ended:completed" {
		t.Fatalf("release = %q", got)
	}
}

// recordedLog collects a lane's log lines for a test to read. The buffer is
// written from the lane's goroutines, so it is guarded.
type recordedLog struct {
	mu    sync.Mutex
	lines strings.Builder
}

func (r *recordedLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lines.Write(p)
}

func (r *recordedLog) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lines.String()
}

func (r *recordedLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(r, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestLaneReportsOnlyAReleaseThatReallyFailed is the tail of the September 12
// fifth dogfood defect. The runner logged `workspace.lease_not_released ...
// 409 (stale_fencing_token)` whenever a session ended on a lease the hub no
// longer held -- which, once the heartbeat renews and a terminal workspace
// releases, is the ordinary end of a session rather than a failure. The lane
// warns only when the release is something it could still have got right.
func TestLaneReportsOnlyAReleaseThatReallyFailed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		release error
		want    string
		absent  string
	}{
		{
			name:    "a lease the hub already released is agreement",
			release: fmt.Errorf("%w: %s", hubclient.ErrStaleWorkspace, "409 (stale_fencing_token)"),
			want:    "workspace.lease_already_released", absent: "workspace.lease_not_released",
		},
		{
			name:    "a lease the hub no longer has is agreement",
			release: fmt.Errorf("%w: %s", hubclient.ErrNoWorkspace, "404"),
			want:    "workspace.lease_already_released", absent: "workspace.lease_not_released",
		},
		{
			name:    "a release the lane could have got right is still reported",
			release: errors.New("hub unreachable"),
			want:    "workspace.lease_not_released", absent: "workspace.lease_already_released",
		},
		{
			name:   "a release that succeeded says nothing",
			absent: "workspace.lease_",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			claimer := &scriptedClaimer{
				claims:  []claimResult{{lease: lease("lease-released")}},
				resolve: errors.New("not a workspace"),
				release: test.release,
			}
			hub := newScriptedHub(t, hubclient.WorkspaceCheckout{WorkItemID: "wi_1", HeartbeatSeconds: 1})
			recorded := &recordedLog{}
			runLaneLogging(t, claimer, hub, recorded.logger(), func() bool { return len(claimer.releases()) == 1 })
			logged := recorded.String()
			if test.want != "" && !strings.Contains(logged, test.want) {
				t.Fatalf("log = %q, want it to carry %q", logged, test.want)
			}
			if strings.Contains(logged, test.absent) {
				t.Fatalf("log = %q, want no %q", logged, test.absent)
			}
		})
	}
}

func TestLaneHoldsAWorkspaceUntilItEnds(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	t.Parallel()
	claimer := &scriptedClaimer{
		claims:   []claimResult{{lease: lease("lease-open")}},
		sessions: []workspacesession.Session{{ID: testWorkspaceID, State: workspacesession.StateRequested}},
	}
	hub := newScriptedHub(t, hubclient.WorkspaceCheckout{
		WorkItemID: "wi_1", Worktree: workspacesession.WorktreeFresh, HeartbeatSeconds: 1,
	})
	lane, err := workspacerunner.NewLane(workspacerunner.LaneConfig{
		Claimer: claimer, Hub: hub, Worktree: &fixedWorktree{path: worktreeWith(t)},
		Logger: discardLogger(), Poll: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = lane.Run(ctx)
	}()
	// The session binds and opens its relay, and the lane keeps holding it:
	// a workspace does not end when a turn ends.
	hub.waitForSocket()
	deadline := time.Now().Add(10 * time.Second)
	for lane.Open() != 1 {
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("the lane holds %d workspaces, want one", lane.Open())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(claimer.releases()) != 0 {
		t.Fatalf("the lane released %v while the workspace was still open", claimer.releases())
	}
	// Cancelling waits for the session to unbind rather than abandoning it: a
	// runner that exited without unbinding would leave the workspace to time
	// out on a missed heartbeat, and a person would watch a ready panel go
	// unreachable for no reason anyone could see.
	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the lane did not drain its sessions")
	}
	if unbound, _ := hub.unbindWith(); !unbound {
		t.Fatal("the lane must unbind before it exits")
	}
	if len(claimer.releases()) != 1 {
		t.Fatalf("releases = %v, want the one claim back", claimer.releases())
	}
}

func TestLaneRequiresItsCollaborators(t *testing.T) {
	t.Parallel()
	if _, err := workspacerunner.NewLane(workspacerunner.LaneConfig{}); err == nil {
		t.Fatal("a lane with no claimer, hub or worktree must be refused rather than fail later")
	}
}

type countingClaimer struct {
	err   error
	mu    sync.Mutex
	calls int
}

func (c *countingClaimer) ClaimWorkspace(context.Context) (tracker.NativeLease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return tracker.NativeLease{}, c.err
}

func (c *countingClaimer) WorkspaceForWorkItem(context.Context, tracker.NativeWorkItemID, hubclient.WorkspaceIdentity) (workspacesession.Session, error) {
	return workspacesession.Session{}, errors.New("no session scripted")
}

func (c *countingClaimer) ReleaseWorkspaceLease(context.Context, tracker.NativeLease, string) error {
	return nil
}

func (c *countingClaimer) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestLaneIdlesOnAHubWithoutWorkspaceSessions(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	t.Parallel()
	tests := []struct {
		name     string
		err      error
		wantMany bool
	}{
		{name: "a hub that serves workspaces is polled", err: hubclient.ErrNoClaimableWork, wantMany: true},
		{name: "a hub without workspace sessions waits the idle interval", err: hubclient.ErrWorkspacesNotServed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			claimer := &countingClaimer{err: test.err}
			lane, err := workspacerunner.NewLane(workspacerunner.LaneConfig{
				Claimer: claimer, Hub: newScriptedHub(t, defaultCheckout()), Worktree: &fixedWorktree{path: t.TempDir()},
				Logger: discardLogger(), Poll: 10 * time.Millisecond, IdlePoll: time.Hour,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = lane.Run(ctx)
			}()
			time.Sleep(300 * time.Millisecond)
			cancel()
			<-done
			calls := claimer.count()
			if test.wantMany && calls < 3 {
				t.Fatalf("the lane asked %d times, want it to keep polling", calls)
			}
			if !test.wantMany && calls != 1 {
				t.Fatalf("the lane asked %d times, want once before the idle interval", calls)
			}
		})
	}
}
