package workspacerunner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// The workspace lane: the runner's loop that claims detent:workspace items and
// holds them open (decisions section 18.1).
//
// It runs beside dispatch rather than through it, and that is a design choice
// rather than a convenience. The orchestrator's dispatcher is built around a
// turn: it acquires a slot, starts a run, waits for it to finish and releases
// everything. A workspace "does not end when a turn ends, and it stays claimed
// until the workspace closes", so putting it through that machinery would mean
// teaching every brake in it about a run that never finishes.
//
// Capacity is still accounted for, without touching the dispatcher: a workspace
// claim takes an ordinary lease on the workspace's own work item, and the hub's
// machineClaimCapacity counts unreleased leases. That is exactly section 18.1's
// "a workspace reserves one slot of the runner's capacity at claim", and it is
// enforced by the same code that enforces it for a run.

// Claimer is the part of the scheduling client the lane needs.
type Claimer interface {
	// ClaimWorkspace asks for the next claimable workspace item. It reports
	// ErrNoWorkspaceWork, or hubclient.ErrNoClaimableWork, when there is
	// none: the common case, and not a failure.
	ClaimWorkspace(ctx context.Context) (tracker.NativeLease, error)
	// WorkspaceForWorkItem resolves the workspace a claimed item dispatches.
	WorkspaceForWorkItem(ctx context.Context, item tracker.NativeWorkItemID, identity hubclient.WorkspaceIdentity) (workspacesession.Session, error)
	// ReleaseWorkspaceLease gives the claim back when the session ends.
	ReleaseWorkspaceLease(ctx context.Context, lease tracker.NativeLease, reason string) error
}

// ErrNoWorkspaceWork reports that no workspace item was claimable. It is the
// steady state of an idle runner, so the lane treats it as "wait", not "retry".
var ErrNoWorkspaceWork = errors.New("no claimable workspace work")

// noWorkspaceWork reports the idle case: nothing was claimable. It is not a
// failure and is never logged, because an idle runner asking every few seconds
// would otherwise fill the log with the absence of work.
func noWorkspaceWork(err error) bool {
	return errors.Is(err, ErrNoWorkspaceWork) || errors.Is(err, hubclient.ErrNoClaimableWork)
}

// LaneConfig builds the lane.
type LaneConfig struct {
	Claimer  Claimer
	Hub      Hub
	Worktree Worktree
	Logger   *slog.Logger
	Now      func() time.Time
	// Poll is how often the lane asks for work when it found none. It is
	// deliberately unhurried: a person waiting for a workspace waits for this,
	// and workspaces.request_timeout gives them five minutes.
	Poll time.Duration
	// MaxOpen bounds how many workspaces this runner holds at once. It is the
	// runner's own brake; the hub's capacity accounting is the other one, and
	// neither is sufficient alone because they bound different things.
	MaxOpen int
	// Deny is the runner's configured addition to the files denylist.
	Deny []string
	// Shell is the project's configured shell, carried through to every
	// session so an action's command runs through the shell its author wrote
	// it for (section 18.12). An empty value takes the platform default.
	Shell string
	// Reporter records project action runs with the hub. It is optional for
	// the same reason it is optional on a session: a runner without one still
	// runs the project's actions.
	Reporter ActionRunReporter
	// Hostname is the host this runner is on, reported on every workspace
	// heartbeat so the header's Open picker can tell a worktree on the
	// reader's own machine from one somewhere else (section 18.13).
	Hostname string
	// Support is what this runner offers beyond the channels every build
	// serves: today, whether it hands out a terminal at all (section 18.3). It
	// is carried on the lane rather than decided per session so the capability
	// this runner reports on its heartbeat and the one its sessions bind with
	// are the same answer -- a lane whose sessions served a surface the
	// heartbeat never reported would be claimed for work it then refused.
	Support Support
}

const (
	// defaultLanePoll is how often an idle lane asks for work.
	defaultLanePoll = 5 * time.Second
	// defaultLaneMaxOpen is how many workspaces one runner holds by default.
	// It matches the per-person limit of section 18.1: one runner serving more
	// open worktrees than one person may open is not a case the contract
	// contemplates.
	defaultLaneMaxOpen = 3
)

func (c LaneConfig) normalized() LaneConfig {
	if c.Poll <= 0 {
		c.Poll = defaultLanePoll
	}
	if c.MaxOpen <= 0 {
		c.MaxOpen = defaultLaneMaxOpen
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Lane claims and holds workspace sessions.
type Lane struct {
	config LaneConfig
	logger *slog.Logger

	mu   sync.Mutex
	open map[string]struct{}
	wait sync.WaitGroup
}

// NewLane prepares the lane. It does not talk to the hub.
func NewLane(config LaneConfig) (*Lane, error) {
	if config.Claimer == nil || config.Hub == nil || config.Worktree == nil {
		return nil, errors.New("workspacerunner: a claimer, a hub and a worktree are required")
	}
	config = config.normalized()
	return &Lane{
		config: config,
		logger: config.Logger.With("component", "workspace_lane"),
		open:   map[string]struct{}{},
	}, nil
}

// Run claims workspace items until ctx is cancelled, then waits for every
// session it started to finish unbinding. The wait matters: a runner that
// exited without unbinding would leave every workspace it held to time out on
// a missed heartbeat, and a person would watch a ready panel go unreachable for
// no reason anyone could see.
func (l *Lane) Run(ctx context.Context) error {
	ticker := time.NewTicker(l.config.Poll)
	defer ticker.Stop()
	for {
		if l.openCount() < l.config.MaxOpen {
			if claimed, err := l.claimOnce(ctx); err != nil {
				if ctx.Err() != nil {
					break
				}
				if !noWorkspaceWork(err) {
					l.logger.Warn("workspace.claim_failed", "error", err)
				}
			} else if claimed {
				// Something was claimed, so ask again straight away: a person
				// who opened two panels should not wait a poll interval for
				// the second.
				continue
			}
		}
		select {
		case <-ctx.Done():
			return l.drain()
		case <-ticker.C:
		}
	}
	return l.drain()
}

// drain waits for every session to finish.
func (l *Lane) drain() error {
	l.wait.Wait()
	return nil
}

// Open reports how many workspaces this runner is holding, for diagnostics.
func (l *Lane) Open() int { return l.openCount() }

func (l *Lane) openCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.open)
}

// claimOnce takes at most one workspace item and starts a session for it.
func (l *Lane) claimOnce(ctx context.Context) (bool, error) {
	lease, err := l.config.Claimer.ClaimWorkspace(ctx)
	if err != nil {
		return false, err
	}
	identity := hubclient.WorkspaceIdentity{LeaseID: lease.ID, FencingToken: lease.FencingToken}
	session, err := l.config.Claimer.WorkspaceForWorkItem(ctx, lease.WorkItemID, identity)
	if err != nil {
		// The claim named an item that is not a workspace, or one whose
		// workspace is gone. Releasing it immediately is the only correct
		// answer: holding it would park this runner on work it cannot do.
		l.release(ctx, lease, "work_item_identity_missing")
		return false, fmt.Errorf("resolve workspace for %s: %w", lease.WorkItemID, err)
	}
	if workspacesession.Terminal(session.State) {
		l.release(ctx, lease, "completed")
		return false, nil
	}
	worker, err := New(Config{
		WorkspaceID: session.ID, Identity: identity, Hub: l.config.Hub,
		Worktree: l.config.Worktree, Logger: l.config.Logger, Now: l.config.Now, Deny: l.config.Deny,
		Shell: l.config.Shell, Reporter: l.config.Reporter,
		Hostname: l.config.Hostname, Support: l.config.Support,
	})
	if err != nil {
		l.release(ctx, lease, "failed")
		return false, err
	}
	l.mu.Lock()
	l.open[session.ID] = struct{}{}
	l.mu.Unlock()
	l.wait.Add(1)
	go func() {
		defer l.wait.Done()
		defer func() {
			l.mu.Lock()
			delete(l.open, session.ID)
			l.mu.Unlock()
		}()
		reason := "completed"
		if err := worker.Run(ctx); err != nil && ctx.Err() == nil {
			l.logger.Warn("workspace.session_failed", "workspace_id", session.ID, "error", err)
			reason = "failed"
		}
		// The lease is released on a context of its own, for the same reason
		// the unbind is: a cancelled run is exactly when the hub most needs to
		// be told the slot is free.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unbindTimeout)
		defer cancel()
		l.release(releaseCtx, lease, reason)
	}()
	l.logger.Info("workspace.claimed", "workspace_id", session.ID, "work_item_id", lease.WorkItemID)
	return true, nil
}

// release gives a claim back, logging rather than failing: a lease the hub
// never hears about expires on its own, so a failed release costs a timeout
// and not correctness.
//
// A lease the hub has already let go of is not a failure at all. The hub
// releases the workspace lease itself when a heartbeat ends the workspace, and
// an expired lease is gone by definition; in both cases the release is refused
// with stale_execution and in both cases the lane and the hub already agree
// that the slot is free. It is the same rule the unbind follows, and warning
// about it would put a line in the log for the ordinary end of a session.
func (l *Lane) release(ctx context.Context, lease tracker.NativeLease, reason string) {
	err := l.config.Claimer.ReleaseWorkspaceLease(ctx, lease, reason)
	switch {
	case err == nil:
	case errors.Is(err, hubclient.ErrStaleWorkspace), errors.Is(err, hubclient.ErrNoWorkspace):
		l.logger.Debug("workspace.lease_already_released", "lease_id", lease.ID, "reason", reason, "error", err)
	default:
		l.logger.Warn("workspace.lease_not_released", "lease_id", lease.ID, "reason", reason, "error", err)
	}
}
