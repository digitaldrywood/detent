//go:build !unix

package workspace

import (
	"context"
	"log/slog"
)

func reapWorkspaceProcesses(context.Context, string, *slog.Logger) int {
	return 0
}
