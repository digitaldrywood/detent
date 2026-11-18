package workspace

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/workspace/hookproc"
)

type workspaceProcessScanner = hookproc.Scanner
type ProcessSource = hookproc.ProcessSource

func scanOwnedWorkspaceProcessIDs(ctx context.Context, path string, scan workspaceProcessScanner) ([]int, error) {
	return hookproc.ScanOwned(ctx, path, scan)
}
func workspaceProcessIDs(ctx context.Context, path string) ([]int, error) {
	return hookproc.ProcessIDs(ctx, path)
}
func ReapProcesses(ctx context.Context, path string, grace time.Duration) (int, error) {
	return hookproc.ReapProcesses(ctx, path, grace)
}
func WithProcessSource(ctx context.Context, source ProcessSource) context.Context {
	return hookproc.WithProcessSource(ctx, source)
}
