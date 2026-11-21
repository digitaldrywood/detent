package runner

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/procstart"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type workerRegistrationStore interface {
	StartSession(context.Context, store.SessionStart) (int64, error)
	FinishSession(context.Context, int64, store.SessionFinish) error
	UpdateSessionWorkerProcess(context.Context, int64, store.WorkerProcessRegistration) error
	MarkSessionWorkerProcessReaped(context.Context, int64, store.WorkerProcessReap) error
}

var ErrWorkerSessionStartUncertain = errors.New("worker session start outcome is uncertain")

type workerSessionStartKey struct{}

func withWorkerSessionStartKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, workerSessionStartKey{}, key)
}

func WorkerSessionStartKey(ctx context.Context) string {
	key, _ := ctx.Value(workerSessionStartKey{}).(string)
	return key
}

type hostValidationProcess struct {
	store         workerRegistrationStore
	sessionID     int64
	start         store.SessionStart
	root          string
	scratch       string
	launchRefused bool

	reconcileStart func(context.Context) (int64, error)
}

func (r *Runner) withValidationProcessOwner(ctx context.Context, issue connector.Issue, attemptID int64) context.Context {
	return withValidationProcessOwner(ctx, r.store, r.projectID, issue, attemptID)
}

func withValidationProcessOwner(ctx context.Context, sessions SessionStore, project string, issue connector.Issue, attemptID int64) context.Context {
	native := workspace.AttemptWorkspaceFromContext(ctx) != (workspace.AttemptWorkspace{})
	owner, ok := sessions.(workerRegistrationStore)
	if !native && (!ok || toolcache.ManagedBuildFromContext(ctx) == "") {
		return ctx
	}
	return workspace.WithValidationProcessOwner(ctx, func(ctx context.Context, info workspace.Info, observedIssue workspace.Issue, scratch string) (workspace.ValidationProcess, error) {
		if !ok {
			return nil, ErrExecutionAuthorityUnavailable
		}
		issueID, identifier := issue.ID, issue.Identifier
		if issueID == "" {
			issueID = observedIssue.ID
		}
		if identifier == "" {
			identifier = observedIssue.Identifier
		}
		start := store.SessionStart{
			ProjectID: project, IssueID: issueID, Identifier: identifier, IssueURL: issue.URL,
			WorkAttemptID: attemptID, StartedAt: time.Now().UTC(), AgentBackendKind: "host", AgentRole: store.WorkerProcessRoleHostValidation,
		}
		return &hostValidationProcess{store: owner, start: start, root: info.Path, scratch: scratch}, nil
	})
}

func (p *hostValidationProcess) Started(ctx context.Context, identity procgroup.Identity) error {
	worker, err := inspectedWorkerProcess(identity)
	if err != nil {
		return err
	}
	p.start.Worker = &store.WorkerProcessRegistration{
		WorkerProcessIdentity: worker,
		CleanupRoot:           p.root, CleanupPath: p.scratch,
	}
	p.sessionID, p.reconcileStart, err = startWorkerSession(ctx, p.store, p.start)
	p.launchRefused = err != nil
	return err
}

func startWorkerSession(ctx context.Context, owner workerRegistrationStore, start store.SessionStart) (int64, func(context.Context) (int64, error), error) {
	key := uuid.NewString()
	commit := func(ctx context.Context) (int64, error) {
		id, err := owner.StartSession(withWorkerSessionStartKey(ctx, key), start)
		if err == nil && id <= 0 {
			err = ErrExecutionAuthorityUnavailable
		}
		return id, err
	}
	id, err := commit(ctx)
	if id <= 0 && errors.Is(err, ErrWorkerSessionStartUncertain) && !errors.Is(err, ErrExecutionAuthorityUnavailable) && !errors.Is(err, ErrOperatorStopped) {
		return 0, commit, err
	}
	return id, nil, err
}

func (p *hostValidationProcess) Finished(ctx context.Context, result gate.CommandResult, cleanupErr error) error {
	if p.reconcileStart != nil {
		var err error
		p.sessionID, err = p.reconcileStart(ctx)
		if p.sessionID <= 0 {
			return err
		}
		p.reconcileStart = nil
	}
	if p.sessionID <= 0 {
		return nil
	}
	now := time.Now().UTC()
	finish := store.SessionFinish{
		CompletedAt: now, FinalState: FinalStateFailed, RuntimeSeconds: int64(time.Duration(result.DurationNS) / time.Second),
	}
	if cleanupErr == nil {
		finish.Reap = &store.WorkerProcessReap{
			ReapedAt: now, Outcome: string(procgroup.TerminationOutcomeAlreadyExited), Reason: store.WorkerProcessRoleHostValidation,
		}
		if !p.launchRefused && result.ExitCode == 0 {
			finish.FinalState = FinalStateCompleted
		}
	}
	return p.store.FinishSession(ctx, p.sessionID, finish)
}

func (r *Runner) reserveSessionWorkerScratch(sessionID int64, root, cleanupPath string) func(context.Context, string) error {
	return func(ctx context.Context, scratch string) error {
		if workspace.AttemptWorkspaceFromContext(ctx).Key() != "" {
			return nil
		}
		owner, ok := r.store.(sessionWorkerProcessStore)
		if !ok || sessionID <= 0 {
			return nil
		}
		path := cleanupPath
		if path == "" {
			path = scratch
		}
		return owner.UpdateSessionWorkerProcess(ctx, sessionID, store.WorkerProcessRegistration{CleanupRoot: root, CleanupPath: path})
	}
}

func inspectedWorkerProcess(identity procgroup.Identity) (store.WorkerProcessIdentity, error) {
	if identity.GroupID <= 0 || identity.StartedAt.IsZero() {
		return store.WorkerProcessIdentity{}, ErrExecutionAuthorityUnavailable
	}
	key, err := procstart.Identity(identity.PID)
	if err != nil {
		return store.WorkerProcessIdentity{}, err
	}
	return store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt, StartKey: key}, nil
}

func (p AgentProcessRequest) OwnProviderLaunch(ctx context.Context) (context.Context, func(error) error) {
	if workspace.AttemptWorkspaceFromContext(ctx).Key() == "" {
		return ctx, func(err error) error { return err }
	}
	owner := workspace.ValidationProcessOwnerFromContext(ctx)
	root := p.Workspace
	if (root == "" || root == p.TempDir) && p.TempDir != "" {
		root = filepath.Dir(p.TempDir)
	}
	var session workspace.ValidationProcess
	var startErr error
	launchCtx := procgroup.WithStartAcknowledgement(ctx, func(identity procgroup.Identity) error {
		if owner == nil || session != nil {
			return ErrExecutionAuthorityUnavailable
		}
		var err error
		session, err = owner(ctx, workspace.Info{Path: root}, workspace.Issue{}, p.TempDir)
		if err != nil {
			return err
		}
		if session == nil {
			return ErrExecutionAuthorityUnavailable
		}
		startErr = session.Started(ctx, identity)
		return startErr
	})
	return launchCtx, func(err error) error {
		if session == nil {
			return err
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		cleanupErr := err
		if startErr != nil {
			cleanupErr = procgroup.StartRollbackCleanup(err)
		}
		return errors.Join(err, session.Finished(cleanup, gate.CommandResult{ExitCode: 0}, cleanupErr))
	}
}

func VerifyRecoveredAttemptWorkerExited(worker tracker.NativeAttemptWorker) error {
	matches, err := procstart.Matches(int(worker.PID), worker.StartKey)
	if err != nil || matches {
		return errors.Join(ErrExecutionAuthorityUnavailable, err)
	}
	observed, err := procgroup.Observe([]procgroup.Identity{{PID: int(worker.PID), GroupID: int(worker.PGID), StartedAt: worker.StartedAt}})
	if err != nil || len(observed) != 1 || !observed[0].GroupObserved || observed[0].Alive || observed[0].Stale {
		return errors.Join(ErrExecutionAuthorityUnavailable, err)
	}
	return nil
}
