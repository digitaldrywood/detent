package runner

import (
	"context"
	"errors"
	"path/filepath"
	"time"

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

type hostValidationProcess struct {
	store      workerRegistrationStore
	sessionID  int64
	root       string
	scratch    string
	registered bool
}

func (r *Runner) withValidationProcessOwner(ctx context.Context, issue connector.Issue, attemptID int64) context.Context {
	native := workspace.AttemptWorkspaceFromContext(ctx).Key() != ""
	owner, ok := r.store.(workerRegistrationStore)
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
		sessionID, err := owner.StartSession(ctx, store.SessionStart{
			ProjectID: r.projectID, IssueID: issueID, Identifier: identifier, IssueURL: issue.URL,
			WorkAttemptID: attemptID, StartedAt: time.Now().UTC(), AgentBackendKind: "host", AgentRole: store.WorkerProcessRoleHostValidation,
		})
		if err != nil {
			return nil, err
		}
		if sessionID <= 0 {
			return nil, ErrExecutionAuthorityUnavailable
		}
		process := &hostValidationProcess{store: owner, sessionID: sessionID, root: info.Path, scratch: scratch}
		if info.Path != "" && scratch != "" {
			if err := owner.UpdateSessionWorkerProcess(ctx, sessionID, store.WorkerProcessRegistration{CleanupRoot: info.Path, CleanupPath: scratch}); err != nil {
				cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
				defer cancel()
				return nil, errors.Join(err, owner.FinishSession(cleanup, sessionID, store.SessionFinish{CompletedAt: time.Now().UTC(), FinalState: FinalStateFailed}))
			}
			process.registered = true
		}
		return process, nil
	})
}

func (p *hostValidationProcess) Started(ctx context.Context, identity procgroup.Identity) error {
	worker, err := inspectedWorkerProcess(identity)
	if err != nil {
		return err
	}
	err = p.store.UpdateSessionWorkerProcess(ctx, p.sessionID, store.WorkerProcessRegistration{
		WorkerProcessIdentity: worker,
		CleanupRoot:           p.root, CleanupPath: p.scratch,
	})
	p.registered = p.registered || err == nil
	return err
}

func (p *hostValidationProcess) Finished(ctx context.Context, result gate.CommandResult, cleanupErr error) error {
	now := time.Now().UTC()
	var reapErr error
	if cleanupErr == nil && p.registered {
		reapErr = p.store.MarkSessionWorkerProcessReaped(ctx, p.sessionID, store.WorkerProcessReap{
			ReapedAt: now, Outcome: string(procgroup.TerminationOutcomeAlreadyExited), Reason: store.WorkerProcessRoleHostValidation,
		})
	}
	state := FinalStateFailed
	if result.ExitCode == 0 && cleanupErr == nil {
		state = FinalStateCompleted
	}
	return errors.Join(reapErr, p.store.FinishSession(ctx, p.sessionID, store.SessionFinish{
		CompletedAt: now, FinalState: state, RuntimeSeconds: int64(time.Duration(result.DurationNS) / time.Second),
	}))
}

func (r *Runner) reserveSessionWorkerScratch(sessionID int64, root, cleanupPath string) func(context.Context, string) error {
	return func(ctx context.Context, scratch string) error {
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
	if root == "" && p.TempDir != "" {
		root = filepath.Dir(p.TempDir)
	}
	var session workspace.ValidationProcess
	launchCtx := procgroup.WithStartAcknowledgement(ctx, func(identity procgroup.Identity) error {
		if owner == nil || session != nil {
			return ErrExecutionAuthorityUnavailable
		}
		var err error
		session, err = owner(ctx, workspace.Info{Path: root}, workspace.Issue{}, p.TempDir)
		if err != nil {
			return err
		}
		return session.Started(ctx, identity)
	})
	return launchCtx, func(err error) error {
		if session == nil {
			return err
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return errors.Join(err, session.Finished(cleanup, gate.CommandResult{ExitCode: 0}, err))
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
