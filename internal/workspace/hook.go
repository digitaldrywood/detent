package workspace

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os/exec"

	"github.com/digitaldrywood/detent/internal/procgroup"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func runHookCommand(ctx context.Context, command string, hooks Hooks, info Info, issue Issue, logger *slog.Logger) (_ []byte, resultErr error) {
	ctx, release, err := toolcache.UseFromContext(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	cmd := commandshell.Command(ctx, command, hooks.Shell)
	cmd.Dir = info.Path
	cmd.Env = hookEnv(info, issue, hooks.StripGitHubTokens)
	if toolcache.ManagedBuildFromContext(ctx) != "" {
		scratch, err := PrepareWorkerScratch(ctx, info.Path)
		if err != nil {
			return nil, err
		}
		applyValidationGoBudget(ctx, cmd, logger)
		procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: toolcache.ManagedEnvironment(ctx)})
		procgroup.SetTempDir(cmd, scratch)
	}
	cmd.WaitDelay = workspaceCommandWaitDelay
	procgroup.Configure(ctx, cmd)

	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err = procgroup.RunHook(cmd)
	if errors.Is(err, exec.ErrWaitDelay) && ctx.Err() == nil {
		err = nil
	}
	if ctx.Err() != nil || err != nil {
		err = errors.Join(ctx.Err(), err, procgroup.Cleanup(procgroup.GroupID(cmd)))
	}
	return output.Bytes(), err
}
