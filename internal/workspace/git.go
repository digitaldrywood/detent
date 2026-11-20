package workspace

import (
	"context"

	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

type CommandError = gitexec.CommandError

const workspaceCommandWaitDelay = gitexec.WaitDelay
const defaultGitRemote = gitexec.DefaultRemote

var detentHandoffDiffExcludes = gitexec.HandoffDiffExcludes

func runGitAt(ctx context.Context, dir string, args ...string) (string, error) {
	return gitexec.Run(ctx, dir, args...)
}
func runGitAtWithEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	return gitexec.WithEnv(ctx, dir, env, args...)
}
func runGitAtWithInput(ctx context.Context, dir string, env []string, output bool, input string, args ...string) (string, error) {
	return gitexec.WithInput(ctx, dir, env, output, input, args...)
}
func gitPathFor(ctx context.Context, dir, path string) (string, error) {
	return gitexec.PathFor(ctx, dir, path)
}
func commandErrorOutput(err error) string  { return gitexec.ErrorOutput(err) }
func validLandingHead(head string) bool    { return gitexec.ValidHead(head) }
func changeSourceScratch() (string, error) { return gitexec.Scratch() }

func gitDiffArgsWithinLimit(ctx context.Context, path string, env []string, limit int, args ...string) (string, bool, error) {
	return gitexec.WithinLimit(ctx, path, env, limit, args...)
}

func remoteGitArgs(args ...string) []string {
	return gitexec.RemoteArgs(args...)
}

func remoteBranchHead(ctx context.Context, path, remote, branch string) (string, bool, error) {
	return gitexec.RemoteBranchHead(ctx, path, remote, branch)
}
