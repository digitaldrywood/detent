package workspace

import (
	"bytes"
	"context"
	"errors"
	"os/exec"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

func runHookCommand(ctx context.Context, command string, hooks Hooks, info Info, issue Issue) ([]byte, error) {
	cmd := commandshell.Command(ctx, command, hooks.Shell)
	cmd.Dir = info.Path
	cmd.Env = hookEnv(info, issue, hooks.StripGitHubTokens)
	cmd.WaitDelay = workspaceCommandWaitDelay
	procgroup.Configure(ctx, cmd)

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	issue.hookOwner.ConfigureCommand(cmd)
	ctx = WithAttemptWorkspace(ctx, issue.AttemptWorkspace)
	var rollbackErr error
	if AttemptWorkspaceFromContext(ctx).Key() != "" {
		owner := ValidationProcessOwnerFromContext(ctx)
		if owner == nil {
			return nil, errors.New("native hook requires a fenced session owner")
		}
		process, err := owner(ctx, info, issue, "")
		if err != nil {
			return nil, err
		}
		if process == nil {
			return nil, errors.New("native hook has no fenced session")
		}
		issue.hookOwner.OnExit(func(cleanup context.Context) error {
			return process.Finished(cleanup, gate.CommandResult{}, rollbackErr)
		})
		ctx = procgroup.WithStartAcknowledgement(ctx, func(id procgroup.Identity) error { return process.Started(ctx, id) })
	}
	if err := procgroup.StartAuthorized(ctx, cmd); err != nil {
		rollbackErr = procgroup.StartRollbackCleanup(err)
		return output.Bytes(), errors.Join(ctx.Err(), err)
	}
	err := procgroup.Wait(cmd)
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil {
		err = nil
	}
	return output.Bytes(), errors.Join(ctx.Err(), err)
}
