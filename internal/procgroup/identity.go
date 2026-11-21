package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/digitaldrywood/detent/internal/sessiontimeout"
	"github.com/digitaldrywood/detent/internal/shell"
)

const DefaultTerminationGrace = 250 * time.Millisecond

var (
	ErrProcessNotRunning = errors.New("process is not running")
	ErrRSSUnsupported    = errors.New("process RSS inspection is unsupported on this platform")
)

type Identity struct {
	PID       int
	GroupID   int
	StartedAt time.Time
}

type TerminationOutcome string

type Observation struct {
	GroupObserved bool
	Identity
	Alive        bool
	Stale        bool
	ProcessCount int
	RSSBytes     int64
}

const (
	TerminationOutcomeTerminated    TerminationOutcome = "terminated"
	TerminationOutcomeKilled        TerminationOutcome = "killed_after_timeout"
	TerminationOutcomeAlreadyExited TerminationOutcome = "already_exited"
	TerminationOutcomeStaleIdentity TerminationOutcome = "stale_identity"
)

func ReapedGroupSignalError(state *os.ProcessState, groupID int, err error) error {
	if errors.Is(err, syscall.EPERM) && groupID > 0 && state != nil &&
		state.Pid() == groupID && reapedProcessGroupEmpty(groupID) {
		return nil
	}
	return err
}

func Inspect(cmd *exec.Cmd) (Identity, error) {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return Identity{}, ErrProcessNotRunning
	}
	return inspectProcess(cmd.Process.Pid)
}

func Alive(identity Identity) (bool, error) {
	if identity.PID <= 0 || identity.StartedAt.IsZero() {
		return false, nil
	}
	current, err := inspectProcess(identity.PID)
	if errors.Is(err, ErrProcessNotRunning) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sameIdentity(current, identity), nil
}

func RSS(ctx context.Context, identity Identity) (uint64, error) {
	return processGroupRSS(ctx, identity)
}

func Observe(identities []Identity) ([]Observation, error) {
	if len(identities) == 0 {
		return []Observation{}, nil
	}
	return observeProcesses(identities)
}

func sameIdentity(current Identity, recorded Identity) bool {
	return current.PID == recorded.PID &&
		(recorded.GroupID <= 0 || current.GroupID == recorded.GroupID) &&
		!recorded.StartedAt.IsZero() && current.StartedAt.Equal(recorded.StartedAt)
}

type startAcknowledgementKey struct{}

func WithStartAcknowledgement(ctx context.Context, acknowledge func(Identity) error) context.Context {
	return context.WithValue(ctx, startAcknowledgementKey{}, acknowledge)
}

func StartAuthorized(ctx context.Context, cmd *exec.Cmd) error {
	acknowledge, ok := ctx.Value(startAcknowledgementKey{}).(func(Identity) error)
	if !ok || acknowledge == nil {
		if shell.HasCommandStarter(ctx) {
			return errors.New("native process requires a launch acknowledgement")
		}
		return Start(cmd)
	}
	return startAcknowledged(cmd, func(identity Identity) error {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		if err := acknowledge(identity); err != nil {
			return err
		}
		return context.Cause(ctx)
	})
}

type launchRollbackError struct {
	launch  error
	cleanup error
}

func (e launchRollbackError) Error() string   { return errors.Join(e.launch, e.cleanup).Error() }
func (e launchRollbackError) Unwrap() []error { return []error{e.launch, e.cleanup} }

func StartRollbackCleanup(err error) error {
	var rollback launchRollbackError
	if errors.As(err, &rollback) {
		return rollback.cleanup
	}
	return nil
}

type CommandOwner func(context.Context, *exec.Cmd, Identity) (func(error, error) error, error)

func WithCommandOwner(ctx context.Context, owner CommandOwner, prepareLocalInspection func(*exec.Cmd) bool) context.Context {
	return shell.WithCommandStarter(ctx, func(ctx context.Context, cmd *exec.Cmd) (func() error, error) {
		if owner == nil {
			return nil, errors.New("native command has no process owner")
		}
		if prepareLocalInspection(cmd) {
			Configure(ctx, cmd)
			return func() error { return Wait(cmd) }, Start(cmd)
		}
		return startOwnedCommand(ctx, cmd, owner)
	})
}

func startOwnedCommand(ctx context.Context, cmd *exec.Cmd, owner CommandOwner) (func() error, error) {
	Configure(ctx, cmd)
	var finish func(error, error) error
	var observed Identity
	launch := WithStartAcknowledgement(ctx, func(id Identity) (err error) {
		finish, err = owner(ctx, cmd, id)
		observed = id
		if err == nil && finish == nil {
			return errors.New("native command has no registered session")
		}
		return err
	})
	if err := StartAuthorized(launch, cmd); err != nil {
		if finish != nil {
			var rollback launchRollbackError
			if errors.As(err, &rollback) {
				err = finish(rollback.launch, rollback.cleanup)
			} else {
				err = finish(err, nil)
			}
		}
		return nil, err
	}
	complete := sessiontimeout.ObserveOwnedCommand(ctx, strconv.Itoa(observed.PID)+"/"+strconv.FormatInt(observed.StartedAt.UnixNano(), 10))
	return func() error {
		defer complete()
		return finish(WaitOwned(ctx, cmd, observed))
	}, nil
}
