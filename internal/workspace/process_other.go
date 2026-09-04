//go:build !unix

package workspace

import (
	"context"
	"log/slog"
	"time"
)

func reapWorkspaceProcesses(context.Context, string, *slog.Logger) int {
	return 0
}

func ReapProcesses(context.Context, string, time.Duration) (int, error) {
	return 0, nil
}
