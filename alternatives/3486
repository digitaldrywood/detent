package workspace

import (
	"bytes"
	"context"
	"errors"
	"os/exec"

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
	if err := cmd.Start(); err != nil {
		return output.Bytes(), errors.Join(ctx.Err(), err)
	}
	groupID := procgroup.GroupID(cmd)
	err := cmd.Wait()
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil {
		err = nil
	}
	if ctx.Err() != nil || err != nil {
		err = errors.Join(ctx.Err(), err, procgroup.Cleanup(groupID))
	}
	return output.Bytes(), err
}
