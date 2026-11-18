//go:build unix && !darwin && !linux

package hookproc

import (
	"context"
	"errors"
)

func scratchEnvironmentProcessIDs(context.Context, string) ([]int, error) {
	return nil, errors.New("worker scratch ownership inspection is unsupported on this platform")
}

func processEnvironment(int) ([]string, error) { return nil, nil }
