// Package workspacerunner is the runner's half of a workspace session
// (decisions sections 18.1, 18.2, 18.4 and 18.12): it claims a worktree, binds
// to the hub, heartbeats, holds the relay open, serves the read-only files
// channel, runs the project's actions and serves the git channel for as long
// as the workspace lives.
//
// It is deliberately not a run. A run creates a worktree, produces a
// deliverable and finishes; a workspace session creates or keeps a worktree,
// produces nothing, and ends only when the person closes it, the lease is lost
// or a deadline passes. That is why it does not go through the runner's
// dispatch path at all.
package workspacerunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspaceexec"
	"github.com/digitaldrywood/detent/internal/workspacefiles"
	"github.com/digitaldrywood/detent/internal/workspacegit"
	"github.com/digitaldrywood/detent/internal/workspacesession"
	"github.com/digitaldrywood/detent/internal/workspaceterminal"
)

// Hub is the part of the native client a session needs. It is an interface so
// the session can be tested against a scripted hub rather than a live one.
type Hub interface {
	BindWorkspace(ctx context.Context, workspaceID string, request hubclient.WorkspaceBindRequest) (hubclient.WorkspaceBindResponse, error)
	HeartbeatWorkspace(ctx context.Context, workspaceID string, request hubclient.WorkspaceHeartbeatRequest) (workspacesession.Session, error)
	UnbindWorkspace(ctx context.Context, workspaceID string, request hubclient.WorkspaceUnbindRequest) error
	DialWorkspaceRelay(ctx context.Context, workspaceID string, identity hubclient.WorkspaceIdentity) (*websocket.Conn, error)
}

// Worktree produces and releases the checkout a workspace serves.
//
// Prepare is given the hub's instructions and answers with a path. "retained"
// means the attempt's own worktree should still exist on this runner and the
// implementation should return it unchanged; "fresh" means the retention window
// has passed and head_sha is checked out into a new one. Release is called once
// the workspace has ended, and must not remove a retained worktree that the
// attempt's own lifecycle still owns.
type Worktree interface {
	Prepare(ctx context.Context, checkout hubclient.WorkspaceCheckout) (string, error)
	Release(ctx context.Context, path string, checkout hubclient.WorkspaceCheckout) error
}

// Config builds a session.
type Config struct {
	// WorkspaceID is the workspace this session serves.
	WorkspaceID string
	// Identity is the owner tuple: the lease on the workspace's own work
	// item, not the subject attempt's.
	Identity hubclient.WorkspaceIdentity
	Hub      Hub
	Worktree Worktree
	Logger   *slog.Logger
	// Now is the clock, so a test can drive the heartbeat and the lease
	// window without waiting.
	Now func() time.Time
	// Deny is the project's extra files denylist. The hub sends it on bind;
	// this is the runner's own configured addition, applied on top.
	Deny []string
	// Shell is the shell an action's command runs through, and it is the
	// project's configured one -- the same shell its workspace hooks already
	// use -- rather than a guess made here. An action's command is shell syntax
	// by construction (section 18.12), so a command an author wrote and tested
	// against their project's shell must not be handed to a different one
	// because the runner picked the platform default. An empty value takes
	// shell.Default(), which is the only honest answer when the project never
	// said.
	Shell string
	// Reporter records project action runs with the hub (section 18.12). It is
	// optional: a runner with no reporter still runs the actions, because the
	// command's effect on the worktree is the point and the run row is how a
	// person watches it happen. A nil reporter loses the watching, not the
	// worktree.
	Reporter ActionRunReporter
	// Support is what this runner offers, which today is only whether it serves
	// a terminal at all (section 18.3). The zero value serves none, so a caller
	// that has not thought about handing out a shell does not hand one out.
	Support Support
	// Hostname is the host this runner is on. It is reported on every
	// heartbeat because the header's Open picker needs it: a cursor://file/…
	// link only reaches a worktree on the reader's own machine, so the client
	// compares this against its own host and disables the items that name
	// somewhere else (section 18.13). It is also half the runner's committer
	// identity, so a commit says which machine made it.
	Hostname string
}

// Support is what this runner has been asked and built to serve.
//
// It exists because the terminal is the first surface whose availability is not
// settled by the build alone. A PTY needs a platform that has one (section
// 18.3's contract is written in SIGHUP, process groups and controlling
// terminals, which Windows does not have), and it needs a runner operator
// willing to hand their account's shell to a person. The organization's own
// switch is workspaces.terminal.enabled and lives on the hub, where section
// 18.3 puts it; this is the runner's half of the same answer.
type Support struct {
	// Terminal is whether this runner offers the terminal channel at all. It is
	// ANDed with what the platform can actually do, so a caller cannot turn on
	// a surface the build has no way to serve.
	Terminal bool
}

// DefaultSupport is what the runner process serves unless a caller narrows it.
//
// The terminal follows the platform: where a PTY exists the runner implements
// the channel and reports it, and whether anybody may open one is the hub's
// decision, which section 18.3 makes an organization setting rather than a
// machine setting. A runner that wants no part of it passes a Support of its
// own, and then the capability is never reported and the workspace is never
// claimed for a terminal.
func DefaultSupport() Support {
	return Support{Terminal: workspaceterminal.Supported}
}

// Capabilities is what this runner can serve in principle.
//
// It is the runner-level answer, and it is what the hub's claim gate and the
// bind match a workspace's requires against: a runner reports it once per
// heartbeat, before any worktree exists, so it can only say what channels this
// build implements. The per-workspace answer is narrower and lives on the
// session, because whether the git or terminal channel can actually be served
// is a fact about one worktree rather than about the runner.
//
// The files, exec, git and terminal channels are implemented (section 18.11
// step 2: "files first, then terminal"; section 18.12's project actions;
// section 18.13's git action group). Reporting diff or preview here would make
// the hub hand this runner workspaces it would then have to refuse frame by
// frame, which is worse for the person than never being offered them: an
// unclaimed workspace fails with no_runner and says so, while a claimed one
// that cannot serve its channel looks broken.
func Capabilities(support Support) workspacesession.Capabilities {
	return workspacesession.Capabilities{
		Files: true, Exec: true, Git: true,
		Terminal: support.Terminal && workspaceterminal.Supported,
	}
}

// Session is one workspace held open on this runner.
type Session struct {
	config Config
	logger *slog.Logger
	files  *workspacefiles.Service
	exec   *workspaceexec.Service
	// path is the worktree this session serves. It is kept rather than left
	// local to Run because an action's working directory is it: the reader is
	// looking at this tree and the command they started has to run in the same
	// one.
	path string

	mu sync.Mutex
	// terminal opens PTYs for this workspace, and is nil when this runner
	// serves no terminal: the platform has none, the operator asked for none,
	// or the isolation level asked for is one this runner cannot provide.
	terminal *workspaceterminal.Service
	// terminals is the PTY held on each stream, keyed by stream id. A terminal
	// outlives the frame that asked for it and, for the resume window, the
	// socket it was speaking over, so something has to hold it.
	terminals map[string]*terminalStream
	// socket is the relay connection output is written to right now. A PTY's
	// output has no frame to answer, so it cannot be written to "the socket the
	// request came on": it goes to whichever socket this session holds at the
	// moment the shell produced it, which after a redial is a different one.
	socket *websocket.Conn
	// git serves the git channel, and is nil when this worktree is not a git
	// repository. It is read under the mutex because the relay's read loop asks
	// for it while Run is still opening it.
	git *workspacegit.Service
	// leaseValidUntil is how long this runner may still act on a frame. It is
	// set from every successful heartbeat, so the check before acting is the
	// runner validating its own lease against the tuple rather than asking
	// the hub once per keystroke.
	leaseValidUntil time.Time
	// stale records that the hub has told this runner it no longer owns the
	// workspace. Nothing is executed afterwards.
	stale bool
	// closing records that the hub has asked for the workspace back.
	closing bool
	state   string
	headSHA string
	// runs is the action running on each stream, keyed by stream id. It is
	// what a close, a lost lease, a dead socket or the end of the session
	// reaches for: a run is the one thing on this surface that outlives the
	// frame that asked for it, so something has to hold its cancel.
	runs map[string]*execRun
	// worktreePath is what Worktree.Prepare returned. The heartbeat carries it
	// and the heartbeat has no other way to see it, because Prepare's answer
	// lives in Run's own frame.
	//
	// It holds the same string as `path` above, and the duplication is
	// deliberate rather than an oversight of the merge that introduced it.
	// `path` is written once in Run before any goroutine reads it and is read
	// unsynchronised by the exec channel; this one is read by the heartbeat
	// goroutine while Run is still running, so it has to be under the mutex.
	// Making `path` mutex-guarded instead would put a lock on the exec
	// channel's hot path for a value that never changes, and reading this one
	// without the lock would be a race the detector is right to flag.
	worktreePath string
	// checkout is what the hub told this runner to produce. The git channel
	// consults it on every write, so it is held here rather than passed down:
	// read_only is the whole difference between a person looking at a worktree
	// and a person changing one.
	checkout hubclient.WorkspaceCheckout
}

// execRun is one running action.
type execRun struct {
	cancel context.CancelFunc
}

// New prepares a session. It does not touch the hub.
func New(config Config) (*Session, error) {
	if config.Hub == nil || config.Worktree == nil {
		return nil, errors.New("workspacerunner: a hub and a worktree are required")
	}
	if err := workspacesession.ValidateID(config.WorkspaceID); err != nil {
		return nil, err
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	// The shell is normalized once, here, so every run of every action in this
	// session goes through the same one and a log line naming it is naming what
	// actually ran.
	config.Shell = shell.Normalize(config.Shell)
	return &Session{
		config:    config,
		logger:    config.Logger.With("component", "workspace_session", "workspace_id", config.WorkspaceID),
		state:     workspacesession.StateRequested,
		runs:      map[string]*execRun{},
		terminals: map[string]*terminalStream{},
	}, nil
}

// Run binds, prepares the worktree, serves the relay and unbinds. It returns
// when the workspace ends, the lease is lost or ctx is cancelled; the reason it
// reports to the hub is the one the workspace's failure will carry.
func (s *Session) Run(ctx context.Context) (resultErr error) {
	bound, err := s.config.Hub.BindWorkspace(ctx, s.config.WorkspaceID, hubclient.WorkspaceBindRequest{
		// The bind carries Capabilities(), the build's answer, and not the
		// per-workspace report: at this point nothing is prepared, so the
		// per-workspace report would say git: false for every workspace and
		// the hub's bind check (workspace_worker.go, "the runner does not
		// serve every required surface") would refuse every workspace that
		// asked for git. The runner would then re-claim it forever and the
		// workspace would sit in requested until its request timeout -- which
		// is exactly what happened, and what the preview's readiness test now
		// pins.
		//
		// The ordering cannot be solved by preparing the worktree first,
		// either: the bind is what returns the checkout instructions, so the
		// worktree does not exist until after it.
		//
		// Reporting the build's answer here is also the honest one. Section
		// 18.1 puts the "reports every capability in requires" gate on the
		// claim, against the runner's own heartbeat, which is a build-level
		// report; the bind check is the same brake for the same kind of fact,
		// and it keeps working. What narrows it to this worktree is the ready
		// heartbeat below, which runs immediately after workspacegit.Open and
		// before the workspace leaves starting -- so a worktree that turns out
		// not to be a repository is reported without the capability rather
		// than failed, and the header disables its git group with that reason
		// (section 18.12).
		WorkspaceIdentity: s.config.Identity, Capabilities: Capabilities(s.config.Support),
		// A runner with no container runtime reports user isolation and the
		// terminal card stays disabled for organizations that require a
		// container. This slice serves no terminal at all, so the honest
		// answer is the level the PTY would run at if one existed.
		Isolation: workspacesession.IsolationUser,
	})
	if err != nil {
		return fmt.Errorf("bind workspace: %w", err)
	}
	s.renewLease(bound.Session)
	s.setCheckout(bound.Checkout)
	s.setState(workspacesession.StateStarting)
	s.logger.Info("workspace.bound", "worktree", bound.Checkout.Worktree,
		"read_only", bound.Checkout.ReadOnly, "requires", bound.Checkout.Requires)

	reason := workspacesession.ReasonClosedByActor
	defer func() {
		// The unbind is the runner agreeing with the hub about how this ended,
		// so it runs on a context of its own: a cancelled ctx is exactly when
		// the hub most needs to hear from us.
		unbindCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unbindTimeout)
		defer cancel()
		if err := s.config.Hub.UnbindWorkspace(unbindCtx, s.config.WorkspaceID, hubclient.WorkspaceUnbindRequest{
			WorkspaceIdentity: s.config.Identity, Reason: reason,
		}); err != nil && !errors.Is(err, hubclient.ErrStaleWorkspace) && !errors.Is(err, hubclient.ErrNoWorkspace) {
			resultErr = errors.Join(resultErr, fmt.Errorf("unbind workspace: %w", err))
		}
	}()

	path, err := s.config.Worktree.Prepare(ctx, bound.Checkout)
	if err != nil {
		reason = workspacesession.ReasonCheckoutFailed
		return fmt.Errorf("prepare workspace worktree: %w", err)
	}
	s.setWorktreePath(path)
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), unbindTimeout)
		defer cancel()
		if err := s.config.Worktree.Release(releaseCtx, path, bound.Checkout); err != nil {
			s.logger.Warn("workspace.worktree_not_released", "path", path, "error", err)
		}
	}()

	deny, err := workspacesession.NewDenylist(append(append([]string{}, bound.Checkout.Deny...), s.config.Deny...))
	if err != nil {
		reason = workspacesession.ReasonCheckoutFailed
		return fmt.Errorf("build workspace denylist: %w", err)
	}
	files, err := workspacefiles.Open(path, deny)
	if err != nil {
		// A worktree that is not there is worktree_missing rather than a
		// checkout failure: the checkout succeeded and something removed it.
		reason = workspacesession.ReasonWorktreeMissing
		return fmt.Errorf("open workspace files: %w", err)
	}
	s.files = files
	defer func() {
		if err := files.Close(); err != nil {
			s.logger.Warn("workspace.files_not_closed", "path", path, "error", err)
		}
	}()

	// The exec service is opened from the same path the files service serves,
	// because an action's working directory is the tree the reader is looking
	// at and not one of its own.
	runner, err := workspaceexec.New(path, s.config.Shell, s.logger)
	if err != nil {
		reason = workspacesession.ReasonWorktreeMissing
		return fmt.Errorf("open workspace exec: %w", err)
	}
	s.path, s.exec = path, runner

	// The run-on-worktree-creation set runs before the workspace is reported
	// ready, so a person who opens a fresh worktree finds the project's setup
	// already done rather than racing it.
	s.runCreationActions(ctx, bound.Checkout, bound.Actions)

	// A worktree that is not a git repository is not a failed workspace. The
	// files channel serves it perfectly well, and the header disables the git
	// group with a reason, which is a better answer for the person than losing
	// the whole workspace because this checkout is not a repository. The git
	// service holds no handle, so there is nothing to close on the way out.
	if git, err := workspacegit.Open(ctx, path, deny); err != nil {
		s.logger.Info("workspace.git_unavailable", "path", path,
			"reason", workspacegit.Message(err), "error", err)
	} else {
		s.setGit(git)
	}

	// A runner that cannot open terminals is not a failed workspace either. The
	// files, exec and git channels serve it perfectly well, and section 18.3
	// says in so many words what a runner that cannot provide the level asked
	// for does: it reports the capability it can serve and the card stays
	// disabled with the reason. A read-only workspace opens no service at all,
	// because a terminal is refused there whatever the runner can do (18.1).
	if s.config.Support.Terminal && !bound.Checkout.ReadOnly {
		terminal, err := workspaceterminal.New(path, s.config.Shell, workspacesession.IsolationUser, s.logger)
		if err != nil {
			s.logger.Info("workspace.terminal_unavailable", "path", path, "error", err)
		} else {
			s.setTerminalService(terminal)
		}
	}
	// Every PTY this session opens is killed on the way out, whatever ended it.
	// Section 18.3 lists four endings -- a close, a resume window that ran out,
	// a lost lease and the workspace closing -- and this is the last of them:
	// the worktree is about to be released, and a shell still living in it
	// would hold a directory the runner is trying to remove.
	defer s.closeTerminals(workspacesession.ReasonClosedByActor)

	s.setHeadSHA(bound.Checkout.HeadSHA)
	if err := s.heartbeat(ctx, workspacesession.StateReady, ""); err != nil {
		reason = s.reasonFor(err, workspacesession.ReasonCheckoutFailed)
		return err
	}
	s.setState(workspacesession.StateReady)

	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		defer cancel()
		s.heartbeatLoop(sessionCtx, bound.Checkout)
	}()
	serveErr := s.serve(sessionCtx)
	cancel()
	group.Wait()
	reason = s.endReason(serveErr)
	if serveErr != nil && !errors.Is(serveErr, context.Canceled) {
		return serveErr
	}
	return nil
}

// unbindTimeout bounds the calls that run after the session's own context is
// gone.
const unbindTimeout = 15 * time.Second

// endReason decides what the hub is told about how this ended.
func (s *Session) endReason(err error) string {
	s.mu.Lock()
	stale, closing := s.stale, s.closing
	s.mu.Unlock()
	switch {
	case stale:
		return workspacesession.ReasonLeaseLost
	case closing:
		return workspacesession.ReasonClosedByActor
	case err != nil:
		return workspacesession.ReasonRunnerRestarted
	default:
		return workspacesession.ReasonClosedByActor
	}
}

// reasonFor maps an error onto a workspace reason, defaulting to fallback.
func (s *Session) reasonFor(err error, fallback string) string {
	if errors.Is(err, hubclient.ErrStaleWorkspace) {
		return workspacesession.ReasonLeaseLost
	}
	return fallback
}

// heartbeatLoop renews the lease and carries the runner's report until the
// workspace ends. A heartbeat that fails with stale_execution ends the session
// immediately: continuing to serve frames under a lost lease is precisely what
// the fencing token exists to prevent.
func (s *Session) heartbeatLoop(ctx context.Context, checkout hubclient.WorkspaceCheckout) {
	interval := time.Duration(checkout.HeartbeatSeconds) * time.Second
	if interval <= 0 {
		interval = workspacesession.HeartbeatInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := s.heartbeat(ctx, "", ""); err != nil {
			if errors.Is(err, hubclient.ErrStaleWorkspace) || errors.Is(err, hubclient.ErrNoWorkspace) {
				s.markStale()
				return
			}
			// A transient failure is not a lost lease. The session keeps
			// serving until the lease window itself runs out, which is the
			// same bound the hub applies.
			s.logger.Warn("workspace.heartbeat_failed", "error", err)
			if !s.leaseValid() {
				s.markStale()
				return
			}
			continue
		}
		if s.shouldStop() {
			return
		}
	}
}

// heartbeat sends one report and records what came back.
func (s *Session) heartbeat(ctx context.Context, state, reason string) error {
	s.mu.Lock()
	head, path, capabilities := s.headSHA, s.worktreePath, s.capabilities()
	s.mu.Unlock()
	session, err := s.config.Hub.HeartbeatWorkspace(ctx, s.config.WorkspaceID, hubclient.WorkspaceHeartbeatRequest{
		WorkspaceIdentity: s.config.Identity, State: state, Reason: reason, HeadSHA: head,
		Capabilities: capabilities, Isolation: workspacesession.IsolationUser,
		// The path and the host are reported on every beat rather than at bind
		// alone, so a runner that re-prepared a fresh worktree corrects the
		// resource instead of leaving a stale path behind it.
		WorktreePath: path, MachineHostname: s.config.Hostname,
	})
	if err != nil {
		return err
	}
	s.renewLease(session)
	return nil
}

// capabilities is what this workspace can actually be served with. The caller
// holds the mutex, because every caller needs the worktree path or the head sha
// in the same breath and a second acquisition would let the two disagree.
//
// The distinction from Capabilities() is the whole point. The claim gate and
// the bind both match on what this build implements in principle -- neither can
// do otherwise, since the bind is what returns the checkout instructions and no
// worktree exists until after it. The workspace *resource* has to carry what
// this worktree can do in fact, because the header enables its git group from
// the workspace's capabilities, and a group enabled against a worktree that is
// not a repository is a control that refuses every time it is pressed. The
// first heartbeat is where the two meet: it runs immediately after
// workspacegit.Open and before the workspace leaves starting, so the resource
// is narrowed before any surface has read it.
func (s *Session) capabilities() workspacesession.Capabilities {
	set := Capabilities(s.config.Support)
	set.Git = s.git != nil
	// A terminal this worktree cannot actually open is not a terminal. The
	// service is nil when the platform has none, when the operator asked for
	// none, or when the organization requires an isolation level this runner
	// cannot provide -- and section 18.3 is explicit that the last of those is
	// reported as a missing capability with a reason rather than as a workspace
	// that fails, so the card stays disabled and says why.
	set.Terminal = set.Terminal && s.terminal != nil
	return set
}

// renewLease records that the hub accepted a call, which is what makes the
// lease window ahead of the runner rather than behind it.
func (s *Session) renewLease(session workspacesession.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaseValidUntil = s.config.Now().Add(workspacesession.LeaseTTL)
	if session.State != "" {
		s.state = session.State
		s.closing = session.State == workspacesession.StateClosing || workspacesession.Terminal(session.State)
	}
}

// leaseValid reports whether this runner may still act. It is the check made
// immediately before acting on every frame, so a frame that arrives after lease
// loss is dropped rather than executed.
func (s *Session) leaseValid() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.stale && s.config.Now().Before(s.leaseValidUntil)
}

func (s *Session) markStale() {
	s.mu.Lock()
	s.stale = true
	s.mu.Unlock()
	// A lost lease stops every run under it. That is the whole point of the
	// fencing token: a generation that no longer owns the worktree stops
	// touching it, and a running command is the loudest way to touch it.
	s.cancelRuns(workspacesession.RunReasonLeaseLost)
	// A shell is the same fact held open. Section 18.3 kills the PTY when the
	// lease is lost, for the reason the runs are cancelled: a person typing
	// into a worktree this generation no longer owns is the failure the fencing
	// token exists to prevent.
	s.closeTerminals(workspacesession.RunReasonLeaseLost)
}

func (s *Session) shouldStop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stale || s.closing
}

func (s *Session) setState(state string) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

// State reports the session's own view, for diagnostics.
func (s *Session) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) setHeadSHA(value string) {
	s.mu.Lock()
	s.headSHA = value
	s.mu.Unlock()
}

func (s *Session) setWorktreePath(value string) {
	s.mu.Lock()
	s.worktreePath = value
	s.mu.Unlock()
}

func (s *Session) setCheckout(checkout hubclient.WorkspaceCheckout) {
	s.mu.Lock()
	s.checkout = checkout
	s.mu.Unlock()
}

func (s *Session) setGit(service *workspacegit.Service) {
	s.mu.Lock()
	s.git = service
	s.mu.Unlock()
}

// gitService reports the git service for this workspace, or nil when the
// worktree is not a repository.
func (s *Session) gitService() *workspacegit.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.git
}

// checkoutRules reports what the hub asked this runner to produce, which is
// what decides whether a write is allowed.
func (s *Session) checkoutRules() hubclient.WorkspaceCheckout {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkout
}

// serve holds the relay open and answers frames until the session ends. A
// socket that drops is redialled: losing the relay is not losing the workspace,
// and a person reconnecting within the resume window expects the runner to
// still be there.
func (s *Session) serve(ctx context.Context) error {
	for {
		if s.shouldStop() {
			return nil
		}
		socket, err := s.config.Hub.DialWorkspaceRelay(ctx, s.config.WorkspaceID, s.config.Identity)
		if err != nil {
			if errors.Is(err, hubclient.ErrStaleWorkspace) || errors.Is(err, hubclient.ErrNoWorkspace) {
				s.markStale()
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.logger.Warn("workspace.relay_dial_failed", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(relayRedialDelay):
			}
			continue
		}
		// The socket is published before the read loop starts, because a PTY
		// opened on an earlier one is still producing output and this is where
		// it now has to go. Cancelling the resume windows in the same breath is
		// the redial half of section 18.2: the hub still holds those streams,
		// so the shells behind them are still the ones their readers were
		// looking at.
		s.setSocket(socket)
		s.attachTerminals()
		err = s.readRelay(ctx, socket)
		s.setSocket(nil)
		// A stream belongs to one connection, so a socket that ended took
		// every stream on it with it. Runs on those streams are stopped rather
		// than left working in a worktree whose reader is gone: their output
		// has nowhere to arrive and nobody asked for their writes to continue.
		s.cancelRuns(workspacesession.RunReasonStreamClosed)
		// A terminal is the one thing on this session that a dropped socket
		// does not end. Section 18.2 gives 60 seconds to come back and says the
		// runner keeps a disconnected PTY alive exactly that long, so the
		// shells are timed rather than killed and a redial inside the window
		// finds them still there.
		s.detachTerminals()
		if closeErr := socket.Close(websocket.StatusNormalClosure, "session ended"); closeErr != nil {
			s.logger.Debug("workspace.relay_socket_not_closed", "error", closeErr)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			s.logger.Info("workspace.relay_closed", "error", err)
		}
	}
}

// relayRedialDelay spaces out reconnection attempts so a hub that is refusing
// connections is not hammered by every runner at once.
const relayRedialDelay = 2 * time.Second

// readRelay answers frames on one socket.
func (s *Session) readRelay(ctx context.Context, socket *websocket.Conn) error {
	for {
		kind, data, err := socket.Read(ctx)
		if err != nil {
			return err
		}
		if kind != websocket.MessageText {
			continue
		}
		var frame workspacesession.Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}
		s.handle(ctx, socket, frame)
		if s.shouldStop() {
			return nil
		}
	}
}

// handle answers one frame.
//
// The lease is validated immediately before acting, never once at the start of
// the session: a frame that arrives after lease loss is dropped with
// stale_execution and never executed, which is the rule that makes it safe for
// a worktree to be served by exactly one generation at a time.
func (s *Session) handle(ctx context.Context, socket *websocket.Conn, frame workspacesession.Frame) {
	if frame.Stream == "" {
		return
	}
	switch frame.Type {
	case workspacesession.TypeAck, workspacesession.TypeResumed, workspacesession.TypeClosed:
		return
	case workspacesession.TypeClose:
		// The person ended this stream, and a run on it ends with them: the
		// command was started for a reader who is no longer there, and a
		// process that outlived its stream would keep writing to a worktree
		// nobody is watching (section 18.12).
		s.cancelRun(frame.Stream, workspacesession.RunReasonStreamClosed)
		// A terminal on this stream ends with it, and that covers two different
		// arrivals: the person's own close, and the close the hub's sweep sends
		// once a parked stream has gone unresumed for the window (section 18.2).
		// Neither is distinguishable from here and neither should be: in both
		// the reader is gone for good and the shell has nobody left.
		s.closeTerminal(frame.Stream)
		return
	case workspacesession.TypeError:
		s.handleRelayError(frame)
		return
	}
	switch frame.Channel {
	case workspacesession.ChannelTerminal:
		s.handleTerminal(ctx, socket, frame)
	case workspacesession.ChannelFiles:
		s.handleFiles(ctx, socket, frame)
	case workspacesession.ChannelExec:
		s.handleExec(ctx, socket, frame)
	case workspacesession.ChannelGit:
		s.handleGit(ctx, socket, frame)
	default:
		// Only terminal, files, exec and git are served in this slice. Anything
		// else is answered rather than ignored, so a client that asked for a
		// diff or a preview on a runner that has neither is told why.
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeUnsupported, "This runner serves the terminal, files, exec and git channels only"))
	}
}

// handleFiles answers one frame on the files channel.
func (s *Session) handleFiles(ctx context.Context, socket *websocket.Conn, frame workspacesession.Frame) {
	if !workspacesession.ValidFilesRequest(frame.Type) {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeUnknownFrame, "The files channel does not define "+frame.Type))
		return
	}
	if !s.leaseValid() {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeStaleExecution, "The workspace lease is no longer held by this runner"))
		return
	}
	var request workspacesession.FilesRequest
	if len(frame.Payload) > 0 {
		if err := json.Unmarshal(frame.Payload, &request); err != nil {
			s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
				workspacesession.CodeInvalidFrame, "The request payload could not be read"))
			return
		}
	}
	answer, err := s.serveFiles(ctx, frame, request)
	if err != nil {
		code := workspacefiles.ErrorCode(err)
		if code == "" {
			code = workspacesession.CodeForbidden
		}
		s.logger.Debug("workspace.files_refused", "type", frame.Type, "code", code, "error", err)
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, refusalMessage(code)))
		return
	}
	s.answer(ctx, socket, answer)
}

// handleGit answers one frame on the git channel (section 18.12).
//
// It is the only channel that writes, so it has two gates the files channel
// does not: the checkout's own rules, and an actor the commit is authored as.
// Both are checked here rather than in workspacegit, because both are facts
// about the workspace the hub handed this runner and not about the worktree.
func (s *Session) handleGit(ctx context.Context, socket *websocket.Conn, frame workspacesession.Frame) {
	if !workspacesession.ValidGitRequest(frame.Type) {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeUnknownFrame, "The git channel does not define "+frame.Type))
		return
	}
	git := s.gitService()
	if git == nil {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeUnsupported, "This worktree is not a git repository, so the git channel cannot be served"))
		return
	}
	if !s.leaseValid() {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeStaleExecution, refusalMessage(workspacesession.CodeStaleExecution)))
		return
	}
	var request workspacesession.GitRequest
	if len(frame.Payload) > 0 {
		if err := json.Unmarshal(frame.Payload, &request); err != nil {
			s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
				workspacesession.CodeInvalidFrame, "The request payload could not be read"))
			return
		}
	}
	if err := workspacesession.ValidateGitRequest(frame.Type, request); err != nil {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeInvalidFrame, err.Error()))
		return
	}
	if workspacesession.GitWriteRequest(frame.Type) {
		if refusal, ok := s.refuseWrite(); !ok {
			s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
				workspacesession.CodeReadOnly, refusal))
			return
		}
	}
	author, ok := commitAuthor(frame)
	if frame.Type == workspacesession.TypeGitCommit && !ok {
		// Committing under the runner's own identity would attribute a
		// person's change to the machine, and there is no correcting that
		// afterwards: the commit is in the history. An unnamed actor is the
		// hub's failure rather than the person's, so it is refused with what
		// is missing rather than with git's advice about configuring an
		// identity.
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeForbidden, "The hub did not name the acting person, so this commit has no author"))
		return
	}
	answer, err := s.serveGit(ctx, git, frame, request, author)
	if err != nil {
		if code := workspacegit.ErrorCode(err); code != "" {
			s.logger.Debug("workspace.git_refused", "type", frame.Type, "code", code, "error", err)
			s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, refusalMessage(code)))
			return
		}
		// git ran and said no. That is not a refusal, so the answer carries
		// git's own stderr: a person acting on a rejected push needs what git
		// said rather than a paraphrase of it.
		message := workspacegit.Message(err)
		if message == "" {
			message = refusalMessage(workspacesession.CodeGitFailed)
		}
		s.logger.Info("workspace.git_failed", "type", frame.Type, "error", err)
		s.answer(ctx, socket, workspacesession.GitFailedFrame(frame.Channel, frame.Stream, message, workspacegit.Stderr(err)))
		return
	}
	s.answer(ctx, socket, answer)
}

// refuseWrite reports whether a git write is allowed on this checkout, and the
// sentence that goes with a refusal.
//
// Section 18.1 states two rules, and they are written out separately here even
// though today the first implies the second. A checkout is read-only while its
// attempt runs, and a retained attempt worktree is the model's own working copy
// rather than a copy of it. A future checkout that separates the two -- a
// retained worktree handed over read-write after its attempt finished, say --
// must not silently start allowing a write into a directory the model is
// editing because one condition was left out.
func (s *Session) refuseWrite() (string, bool) {
	checkout := s.checkoutRules()
	if checkout.ReadOnly {
		return "This workspace is read-only, so it cannot be written to", false
	}
	if checkout.Worktree == workspacesession.WorktreeRetained && checkout.ReadOnly {
		return "This workspace holds a running attempt's own worktree, so it cannot be written to", false
	}
	return "", true
}

// commitAuthor reads the person a commit is authored as from the hub's stamp.
//
// The actor is the hub's to write and never the client's (section 18.2), which
// is the only reason it can be trusted as an author at all: a client that could
// name its own author could attribute a commit to anybody.
func commitAuthor(frame workspacesession.Frame) (workspacegit.Identity, bool) {
	if frame.Actor == nil {
		return workspacegit.Identity{}, false
	}
	author := workspacegit.Identity{Name: frame.Actor.Name, Email: frame.Actor.Email}
	if author.Validate() != nil {
		return workspacegit.Identity{}, false
	}
	return author, true
}

// runnerCommitterEmail is the address every commit this runner makes is
// committed under. .invalid is the reserved top-level domain of RFC 2606, so the
// address can never be delivered to and can never collide with a real person's:
// the committer is a machine, and a machine must not be reachable as though it
// were somebody.
const runnerCommitterEmail = "runner@detent.invalid"

// committerIdentity is the runner as git records it. The host is named when the
// runner knows it, because a person reading a history wants to know which
// machine committed as well as who authored.
func (s *Session) committerIdentity() workspacegit.Identity {
	name := "Detent runner"
	if s.config.Hostname != "" {
		name = "Detent runner (" + s.config.Hostname + ")"
	}
	return workspacegit.Identity{Name: name, Email: runnerCommitterEmail}
}

// handleRelayError acts on a control failure the hub reported. superseded and
// revoked mean this connection is over; stale_execution and workspace_closed
// mean the session is.
func (s *Session) handleRelayError(frame workspacesession.Frame) {
	var payload workspacesession.ErrorPayload
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		return
	}
	switch payload.Code {
	case workspacesession.CodeStaleExecution, workspacesession.CodeWorkspaceClosed, workspacesession.CodeSuperseded:
		s.markStale()
	}
}

// serveFiles dispatches one files request.
func (s *Session) serveFiles(ctx context.Context, frame workspacesession.Frame, request workspacesession.FilesRequest) (workspacesession.Frame, error) {
	answer := workspacesession.Frame{Channel: frame.Channel, Stream: frame.Stream}
	switch frame.Type {
	case workspacesession.TypeFilesList:
		listed, err := s.files.List(ctx, request)
		if err != nil {
			return answer, err
		}
		payload, err := workspacesession.Encode(listed)
		if err != nil {
			return answer, err
		}
		answer.Type, answer.Payload = workspacesession.TypeFilesListed, payload
	case workspacesession.TypeFilesStat:
		stat, err := s.files.Stat(ctx, request)
		if err != nil {
			return answer, err
		}
		payload, err := workspacesession.Encode(stat)
		if err != nil {
			return answer, err
		}
		answer.Type, answer.Payload = workspacesession.TypeFilesStat, payload
	case workspacesession.TypeFilesRead:
		content, err := s.files.Read(ctx, request)
		if err != nil {
			return answer, err
		}
		payload, err := workspacesession.Encode(content)
		if err != nil {
			return answer, err
		}
		answer.Type, answer.Payload = workspacesession.TypeFilesContent, payload
	case workspacesession.TypeFilesWatch:
		// Watching is optional in section 18.4: a runner that cannot do it
		// says so and the client refreshes on focus instead. Filesystem
		// notification is not implemented in this slice, and claiming it
		// would leave a reader believing a stale tree is current.
		answer.Type = workspacesession.TypeError
		payload, err := workspacesession.Encode(workspacesession.ErrorPayload{
			Code: workspacesession.CodeUnsupported, Message: "This runner does not watch for changes; refresh on focus",
		})
		if err != nil {
			return answer, err
		}
		answer.Payload = payload
	default:
		return answer, fmt.Errorf("unhandled files frame %q", frame.Type)
	}
	return answer, nil
}

// maxConcurrentRuns bounds how many actions this session runs at once.
//
// It is the relay's own per-workspace stream cap rather than a second number
// invented here. A run occupies one stream for its whole life, and the relay
// already refuses the stream a thirty-third run would need (eight per
// connection, thirty-two per workspace), so a tighter bound here would refuse
// a run whose client had every reason to believe it could start one. The bound
// exists at all because a stream limit the hub enforces is no bound on the
// processes this runner would have started before hearing about it.
const maxConcurrentRuns = workspacesession.MaxStreamsPerWorkspace

// runLeaseInterval is how often a running action's lease is re-checked. It is
// short relative to LeaseTTL on purpose: the window between losing the lease
// and stopping the command is the window in which two generations could be
// writing to one worktree.
const runLeaseInterval = time.Second

// handleExec starts one run on its own stream.
//
// A run is not request/response, which is why nothing is answered here: the
// output spans, the exit and the stream's close are written by serveExec as
// they happen, and this function's whole job is to refuse what should not
// start and to get out of the read loop's way.
func (s *Session) handleExec(ctx context.Context, socket *websocket.Conn, frame workspacesession.Frame) {
	if !workspacesession.ValidExecRequest(frame.Type) {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeUnknownFrame, "The exec channel does not define "+frame.Type))
		return
	}
	var request workspacesession.ExecRun
	if len(frame.Payload) > 0 {
		if err := json.Unmarshal(frame.Payload, &request); err != nil {
			s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
				workspacesession.CodeInvalidFrame, "The request payload could not be read"))
			return
		}
	}
	// The lease is validated immediately before the process starts, the same
	// rule every files frame follows: a run frame that arrives after lease loss
	// is refused and never executed, so a worktree is only ever written to by
	// the generation that owns it.
	if !s.leaseValid() {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream,
			workspacesession.CodeStaleExecution, execRefusalMessage(workspacesession.CodeStaleExecution)))
		return
	}
	runCtx, code, message := s.startRun(ctx, frame.Stream)
	if code != "" {
		s.answer(ctx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, message))
		return
	}
	// The command runs on a goroutine of its own because the read loop is what
	// carries the close that stops it: serving a run inline would mean a
	// session that cannot hear "stop" until the thing being stopped is over.
	go func() {
		defer s.finishRun(frame.Stream)
		s.serveExec(ctx, runCtx, socket, frame, request)
	}()
}

// serveExec streams one run: each output span as its own frame, then the exit,
// then the close that gives the stream back.
func (s *Session) serveExec(
	sessionCtx context.Context,
	runCtx context.Context,
	socket *websocket.Conn,
	frame workspacesession.Frame,
	request workspacesession.ExecRun,
) {
	// Frames are written on the session's context and not the run's. A run that
	// was killed still owes the person an exited and a closed frame, and a
	// write on the cancelled context that killed it would send neither.
	emit := func(span workspacesession.ExecOutput) error {
		payload, err := workspacesession.Encode(span)
		if err != nil {
			return err
		}
		s.answer(sessionCtx, socket, workspacesession.Frame{
			Channel: frame.Channel, Stream: frame.Stream,
			Type: workspacesession.TypeExecOutput, Payload: payload,
		})
		return nil
	}
	// The lease is re-checked while the process runs, not only before it
	// starts: a run outlives the frame that asked for it, so the check that
	// makes a files read safe is not enough on its own here.
	go s.watchRunLease(runCtx, frame.Stream)

	// The command is the one the hub relayed with this run, and the runner has
	// no action store of its own to check it against. What bounds it is who may
	// write an action and where it runs, which is section 18.12's own answer to
	// the same question about the hub.
	result, err := s.exec.Run(runCtx, request.Command, emit)
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// A killed run still ended, and its code and signal are how it ended.
		s.answerExecExited(sessionCtx, socket, frame, result)
	default:
		code := workspaceexec.ErrorCode(err)
		if code == "" {
			code = workspacesession.CodeForbidden
		}
		s.logger.Debug("workspace.action_refused", "stream", frame.Stream, "code", code, "error", err)
		s.answer(sessionCtx, socket, workspacesession.ErrorFrame(frame.Channel, frame.Stream, code, execRefusalMessage(code)))
	}
	// The hub releases a stream's slot on close or closed and never on exited.
	// A runner that stopped at exited would leak one slot per run until the
	// workspace hit its stream cap and started refusing the next one, and the
	// person would see a workspace that runs nothing for no visible reason.
	s.answer(sessionCtx, socket, workspacesession.Frame{
		Channel: frame.Channel, Stream: frame.Stream, Type: workspacesession.TypeClosed,
	})
}

// answerExecExited reports how a run ended.
func (s *Session) answerExecExited(
	ctx context.Context,
	socket *websocket.Conn,
	frame workspacesession.Frame,
	result workspaceexec.Result,
) {
	payload, err := workspacesession.Encode(workspacesession.ExecExited{Code: result.ExitCode, Signal: result.Signal})
	if err != nil {
		s.logger.Warn("workspace.action_exit_not_encoded", "stream", frame.Stream, "error", err)
		return
	}
	s.answer(ctx, socket, workspacesession.Frame{
		Channel: frame.Channel, Stream: frame.Stream, Type: workspacesession.TypeExecExited, Payload: payload,
	})
}

// startRun registers a run for a stream and answers with the context that
// stops it, or with the code and sentence that refuse it.
func (s *Session) startRun(ctx context.Context, stream string) (context.Context, string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs == nil {
		s.runs = map[string]*execRun{}
	}
	if _, running := s.runs[stream]; running {
		// One stream is one run. A second run frame on a stream that is
		// already running one would leave the person with two interleaved
		// output streams and one exited frame to explain both.
		return nil, workspacesession.CodeInvalidFrame, "This stream is already running an action"
	}
	if len(s.runs) >= maxConcurrentRuns {
		return nil, workspacesession.CodeStreamLimit, execRefusalMessage(workspacesession.CodeStreamLimit)
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.runs[stream] = &execRun{cancel: cancel}
	return runCtx, "", ""
}

// finishRun forgets a run that has ended and releases its context.
func (s *Session) finishRun(stream string) {
	s.mu.Lock()
	run, running := s.runs[stream]
	delete(s.runs, stream)
	s.mu.Unlock()
	if running {
		run.cancel()
	}
}

// cancelRun stops the run on one stream, if there is one. The process group
// goes with it: a command that started a build or a server must not outlive
// the reason it was started.
func (s *Session) cancelRun(stream, reason string) {
	s.mu.Lock()
	run, running := s.runs[stream]
	s.mu.Unlock()
	if !running {
		return
	}
	s.logger.Debug("workspace.action_stopped", "stream", stream, "reason", reason)
	run.cancel()
}

// cancelRuns stops every run this session is holding.
func (s *Session) cancelRuns(reason string) {
	s.mu.Lock()
	streams := make([]*execRun, 0, len(s.runs))
	for _, run := range s.runs {
		streams = append(streams, run)
	}
	s.mu.Unlock()
	if len(streams) == 0 {
		return
	}
	s.logger.Debug("workspace.actions_stopped", "runs", len(streams), "reason", reason)
	for _, run := range streams {
		run.cancel()
	}
}

// watchRunLease stops a run whose lease this runner has lost mid-command.
func (s *Session) watchRunLease(ctx context.Context, stream string) {
	ticker := time.NewTicker(runLeaseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !s.leaseValid() {
			s.cancelRun(stream, workspacesession.RunReasonLeaseLost)
			return
		}
	}
}

// execRefusalMessage is the sentence that goes with an exec refusal. It says
// what this runner would not do without repeating the command back, which a
// person may be reading in a shared panel.
func execRefusalMessage(code string) string {
	switch code {
	case workspacesession.CodeStaleExecution:
		return "The workspace lease is no longer held by this runner"
	case workspacesession.CodeInvalidFrame:
		return "The run frame did not carry a command to run"
	case workspacesession.CodeStreamLimit:
		return "This workspace is already running as many actions as it may"
	case workspacesession.CodeTooLarge:
		return "The command is longer than this surface runs"
	case workspacesession.CodeUnsupported:
		return "This runner cannot run the project's shell"
	default:
		return "The action could not be run"
	}
}

// serveGit dispatches one git request.
func (s *Session) serveGit(
	ctx context.Context,
	git *workspacegit.Service,
	frame workspacesession.Frame,
	request workspacesession.GitRequest,
	author workspacegit.Identity,
) (workspacesession.Frame, error) {
	answer := workspacesession.Frame{Channel: frame.Channel, Stream: frame.Stream}
	switch frame.Type {
	case workspacesession.TypeGitStatus:
		status, err := git.Status(ctx)
		if err != nil {
			return answer, err
		}
		payload, err := workspacesession.Encode(status)
		if err != nil {
			return answer, err
		}
		// The status answer reuses the request's name, exactly as the files
		// channel's stat does: the direction disambiguates.
		answer.Type, answer.Payload = workspacesession.TypeGitStatus, payload
	case workspacesession.TypeGitCommit:
		committed, err := git.Commit(ctx, request.Message, author, s.committerIdentity())
		if err != nil {
			return answer, err
		}
		payload, err := workspacesession.Encode(committed)
		if err != nil {
			return answer, err
		}
		answer.Type, answer.Payload = workspacesession.TypeGitCommitted, payload
	case workspacesession.TypeGitPush:
		pushed, err := git.Push(ctx)
		if err != nil {
			return answer, err
		}
		payload, err := workspacesession.Encode(pushed)
		if err != nil {
			return answer, err
		}
		answer.Type, answer.Payload = workspacesession.TypeGitPushed, payload
	default:
		return answer, fmt.Errorf("unhandled git frame %q", frame.Type)
	}
	return answer, nil
}

// answer writes one frame back to the hub. A write that fails ends the socket
// rather than the session: the relay redials.
func (s *Session) answer(ctx context.Context, socket *websocket.Conn, frame workspacesession.Frame) {
	encoded, err := json.Marshal(frame)
	if err != nil {
		s.logger.Warn("workspace.frame_not_encoded", "type", frame.Type, "error", err)
		return
	}
	if len(encoded) > workspacesession.MaxFrameBytes {
		// Chunking a payload the hub would refuse whole is section 18.2's own
		// answer; until the channels that produce oversized payloads exist,
		// refusing is better than sending something the hub will drop.
		s.answerTooLarge(ctx, socket, frame)
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, relayWriteTimeout)
	defer cancel()
	if err := socket.Write(writeCtx, websocket.MessageText, encoded); err != nil {
		s.logger.Debug("workspace.frame_not_written", "type", frame.Type, "error", err)
	}
}

// answerTooLarge replaces an oversized answer with the channel's own refusal.
func (s *Session) answerTooLarge(ctx context.Context, socket *websocket.Conn, frame workspacesession.Frame) {
	refusal := workspacesession.ErrorFrame(frame.Channel, frame.Stream, workspacesession.CodeTooLarge,
		refusalMessage(workspacesession.CodeTooLarge))
	encoded, err := json.Marshal(refusal)
	if err != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, relayWriteTimeout)
	defer cancel()
	if err := socket.Write(writeCtx, websocket.MessageText, encoded); err != nil {
		s.logger.Debug("workspace.refusal_not_written", "stream", frame.Stream, "error", err)
	}
}

// setSocket publishes the relay connection this session is speaking over, so a
// PTY's output can be written to whichever one is current rather than to the
// one its open frame happened to arrive on.
func (s *Session) setSocket(socket *websocket.Conn) {
	s.mu.Lock()
	s.socket = socket
	s.mu.Unlock()
}

// currentSocket reports the relay connection, or nil while this session has
// none.
func (s *Session) currentSocket() *websocket.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.socket
}

// errNoRelaySocket reports that there is nowhere to write a frame right now.
//
// It is returned rather than swallowed so a PTY's output loop can decide what
// to do about it, and what it decides is to stop forwarding and leave the shell
// running: the reader has 60 seconds to come back (section 18.2), and a shell
// killed because one span had nowhere to go would make that window a lie.
var errNoRelaySocket = errors.New("workspacerunner: no relay socket is attached")

// push writes one frame to whatever socket this session currently holds.
//
// It is the answer path for anything the runner produces on its own initiative
// rather than in reply to a frame: a PTY's output, its exit and the close that
// follows. Everything else uses answer, which writes to the socket the request
// arrived on, because for a request/response channel those are the same socket
// by construction.
//
// The context every caller hands it is the session's, not the frame's. That is
// the whole difference: a PTY outlives the frame that opened it, so a write
// bounded by that frame's context would be abandoned the moment the read loop
// moved on to the next one. The session's context is the right lifetime because
// it is exactly what a write should be abandoned for --- the workspace ending,
// the lease going, or the process shutting down.
func (s *Session) push(ctx context.Context, frame workspacesession.Frame) error {
	socket := s.currentSocket()
	if socket == nil {
		return errNoRelaySocket
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("encode %s frame: %w", frame.Type, err)
	}
	if len(encoded) > workspacesession.MaxFrameBytes {
		// Nothing this path produces can reach the cap: an output span is
		// bounded well under it by MaxTerminalOutputFrameBytes, and an exit is
		// two numbers. An oversized frame here is therefore a bug rather than a
		// condition, and dropping it silently would hide the bug.
		return fmt.Errorf("frame %s exceeds the relay cap at %d bytes", frame.Type, len(encoded))
	}
	writeCtx, cancel := context.WithTimeout(ctx, relayWriteTimeout)
	defer cancel()
	if err := socket.Write(writeCtx, websocket.MessageText, encoded); err != nil {
		return fmt.Errorf("write %s frame: %w", frame.Type, err)
	}
	return nil
}

// relayWriteTimeout bounds one answer.
const relayWriteTimeout = 10 * time.Second

// refusalMessage is the sentence that goes with a refusal on either channel. It
// says what happened without saying what is there: the reason a path was
// refused can itself disclose the thing the refusal is protecting.
//
// It stays one table as the channels grow. A second one would drift: the same
// code would reach a person as two different sentences depending on which
// channel refused, and a client showing them would look like two products.
func refusalMessage(code string) string {
	switch code {
	case workspacesession.CodeNotFound:
		return "No such path in the worktree"
	case workspacesession.CodeForbidden:
		return "That path cannot be read through this surface"
	case workspacesession.CodeDenied:
		return "The project's secret patterns refuse this path"
	case workspacesession.CodeTooLarge:
		return "The file is larger than this surface serves"
	case workspacesession.CodeUnsupported:
		return "This runner does not serve that request"
	case workspacesession.CodeStaleExecution:
		return "The workspace lease is no longer held by this runner"
	case workspacesession.CodeUnknownFrame:
		return "That request is not one this channel defines"
	case workspacesession.CodeInvalidFrame:
		return "The request could not be read"
	case workspacesession.CodeReadOnly:
		return "This workspace is read-only, so it cannot be written to"
	case workspacesession.CodeGitFailed:
		return "The git command failed"
	default:
		return "The request was refused"
	}
}
