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

type checkpointTransaction = checkpoint.Transaction

const CheckpointRefPrefix = checkpoint.CheckpointRefPrefix
const MaxCheckpointRefBytes = checkpoint.MaxCheckpointRefBytes

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
func checkpointSnapshot(ctx context.Context, git, index checkpointGitRunner) (string, string, []string, error) {
	return checkpoint.Snapshot(ctx, git, index)
}
func checkpointMatches(ctx context.Context, path string, ref *CheckpointRef) bool {
	return checkpoint.Matches(ctx, path, ref)
}
func checkpointUnpublishedHistory(ctx context.Context, git checkpointGitRunner, head string) error {
	return checkpoint.UnpublishedHistory(ctx, git, head)
}
func restoreCheckpointTree(ctx context.Context, path string, ref *CheckpointRef) error {
	return checkpoint.RestoreTree(ctx, path, ref)
}

func captureCheckpointTransaction(ctx context.Context, path, branch, scratch string) (*checkpointTransaction, error) {
	return checkpoint.CaptureTransaction(ctx, path, branch, scratch)
}
func validateCheckpointTransaction(ctx context.Context, git checkpointGitRunner, tx *checkpointTransaction) error {
	return checkpoint.ValidateTransaction(ctx, git, tx)
}

func readCheckpointTransaction(ctx context.Context, git checkpointGitRunner, ref *CheckpointRef) (*checkpointTransaction, error) {
	return checkpoint.ReadTransaction(ctx, git, ref)
}
func checkpointTransactionMatches(ctx context.Context, path string, tx *checkpointTransaction) bool {
	return checkpoint.TransactionMatches(ctx, path, tx)
}
