//go:build !unix && !windows

package hookproc

import (
	"context"
	"time"
)

func ProcessIDs(context.Context, string) ([]int, error) {
	return nil, nil
}

func ReapProcesses(context.Context, string, time.Duration) (int, error) {
	return 0, nil
}
