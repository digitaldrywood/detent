//go:build !unix

package workspace

import (
	"context"
	"log/slog"
	"time"
)

func workspaceProcessIDs(context.Context, string) ([]int, error) {
	return nil, nil
}

func reapWorkspaceProcesses(context.Context, string, *slog.Logger) int {
	return 0
}

func ReapProcesses(context.Context, string, time.Duration) (int, error) {
	return 0, nil
}
