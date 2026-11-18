//go:build !unix && !windows

package hookproc

import (
	"context"
	"log/slog"
	"time"
)

func ProcessIDs(context.Context, string) ([]int, error) {
	return nil, nil
}

func ReapWorkspace(context.Context, string, *slog.Logger) int {
	return 0
}

func ReapProcesses(context.Context, string, time.Duration) (int, error) {
	return 0, nil
}
