package runner

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/toolcache"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type hostValidationStore interface {
	StartSession(context.Context, store.SessionStart) (int64, error)
	FinishSession(context.Context, int64, store.SessionFinish) error
	UpdateSessionWorkerProcess(context.Context, int64, store.WorkerProcessRegistration) error
	MarkSessionWorkerProcessReaped(context.Context, int64, store.WorkerProcessReap) error
}

type hostValidationProcess struct {
	store     hostValidationStore
	sessionID int64
	root      string
	scratch   string
}

func (r *Runner) withValidationProcessOwner(ctx context.Context, issue connector.Issue, attemptID int64) context.Context {
	if toolcache.ManagedBuildFromContext(ctx) == "" {
		return ctx
	}
	owner, ok := r.store.(hostValidationStore)
	if !ok {
		return ctx
	}
	return workspace.WithValidationProcessOwner(ctx, func(ctx context.Context, info workspace.Info, observedIssue workspace.Issue, scratch string) (workspace.ValidationProcess, error) {
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
		process := &hostValidationProcess{store: owner, sessionID: sessionID, root: info.Path, scratch: scratch}
		if err := owner.UpdateSessionWorkerProcess(ctx, sessionID, store.WorkerProcessRegistration{CleanupRoot: info.Path, CleanupPath: scratch}); err != nil {
			return nil, errors.Join(err, owner.FinishSession(context.WithoutCancel(ctx), sessionID, store.SessionFinish{CompletedAt: time.Now().UTC(), FinalState: FinalStateFailed}))
		}
		return process, nil
	})
}

func (p *hostValidationProcess) Started(ctx context.Context, identity procgroup.Identity) error {
	return p.store.UpdateSessionWorkerProcess(ctx, p.sessionID, store.WorkerProcessRegistration{
		WorkerProcessIdentity: store.WorkerProcessIdentity{PID: identity.PID, GroupID: identity.GroupID, StartedAt: identity.StartedAt},
		CleanupRoot:           p.root, CleanupPath: p.scratch,
	})
}

func (p *hostValidationProcess) Finished(ctx context.Context, result gate.CommandResult, cleanupErr error) error {
	now := time.Now().UTC()
	var reapErr error
	if cleanupErr == nil {
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
