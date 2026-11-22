package workspace

import (
	"context"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/workspace/checkpoint"
)

var ErrCheckpointUnsafe = checkpoint.ErrUnsafe

type CheckpointSelection = checkpoint.Selection
type CheckpointPlan = checkpoint.Plan
type CheckpointRecord = checkpoint.Record
type CheckpointRef = checkpoint.Ref
type checkpointGitRunner = checkpoint.GitRunner

const CheckpointRefPrefix = checkpoint.CheckpointRefPrefix

func WriteCheckpointRecord(plan CheckpointPlan, record CheckpointRecord) error {
	return checkpoint.WriteRecord(plan, record)
}
func checkpointEnvironment(env procgroup.Environment) []string { return checkpoint.Environment(env) }
func checkpointGit(path string, env procgroup.Environment) checkpointGitRunner {
	return checkpoint.Git(path, env)
}
func checkpointRefGit(path string, env ...string) checkpointGitRunner {
	return checkpoint.RefGit(path, env...)
}
func checkpointGuard(ctx context.Context, git checkpointGitRunner, plan CheckpointPlan, head string, validate func(context.Context) error) error {
	return checkpoint.Guard(ctx, git, plan, head, validate)
}
func checkpointRemoteHead(ctx context.Context, git checkpointGitRunner, plan CheckpointPlan) (string, error) {
	return checkpoint.RemoteHead(ctx, git, plan)
}
func checkpointHistory(ctx context.Context, git checkpointGitRunner, base, head string, paths []string) error {
	return checkpoint.History(ctx, git, base, head, paths)
}
func checkpointBlob(ctx context.Context, git checkpointGitRunner, prefix, path string) error {
	return checkpoint.Blob(ctx, git, prefix, path)
}
func checkpointPathAllowed(path string) bool { return checkpoint.PathAllowed(path) }
func checkpointMatches(ctx context.Context, path string, ref *CheckpointRef) bool {
	return checkpoint.Matches(ctx, path, ref)
}
func restoreCheckpointTree(ctx context.Context, path string, ref *CheckpointRef) error {
	return checkpoint.RestoreTree(ctx, path, ref)
}
