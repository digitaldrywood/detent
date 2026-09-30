package cli

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/connector"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
)

func (r *sshRunner) ReapWorkspace(ctx context.Context, issue connector.Issue) (runnerpkg.WorkspaceReapResult, error) {
	total, localErr := r.Runner.ReapWorkspace(ctx, issue)
	errs := []error{localErr}
	for _, host := range r.SSHHosts() {
		if host == "local" {
			continue
		}
		var remote runnerpkg.WorkspaceReapResult
		request := runnerpkg.RunRequest{ProjectID: r.projectID, Issue: issue, WorkerHost: host}
		if err := r.callSSH(ctx, request, "reap", &remote, nil); err != nil {
			errs = append(errs, err)
			continue
		}
		total.Worktrees += remote.Worktrees
		total.Branches += remote.Branches
		total.Processes += remote.Processes
		if total.Path == "" {
			total.Path = remote.Path
		}
	}
	return total, errors.Join(errs...)
}

func (r *sshRunner) ReconcileWorkspaces(ctx context.Context, active []connector.Issue) (runnerpkg.WorkspaceReconcileResult, error) {
	total, localErr := r.Runner.ReconcileWorkspaces(ctx, active)
	errs := []error{localErr}
	for _, host := range r.SSHHosts() {
		if host == "local" {
			continue
		}
		var remote runnerpkg.WorkspaceReconcileResult
		request := runnerpkg.RunRequest{ProjectID: r.projectID, WorkerHost: host}
		if err := r.callSSH(ctx, request, "reconcile", &remote, active); err != nil {
			errs = append(errs, err)
			continue
		}
		total.Removed += remote.Removed
		total.ActiveSkipped += remote.ActiveSkipped
		total.PreservedSkipped += remote.PreservedSkipped
		total.RegisteredSkipped += remote.RegisteredSkipped
		total.UnownedSkipped += remote.UnownedSkipped
		total.CompletedPaths = append(total.CompletedPaths, remote.CompletedPaths...)
		total.Failures = append(total.Failures, remote.Failures...)
	}
	return total, errors.Join(errs...)
}
