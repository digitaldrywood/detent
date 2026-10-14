//go:build unix && !darwin && !linux

package workspace

import (
	"context"
	"errors"
)

func scratchEnvironmentProcessIDs(context.Context, string) ([]int, error) {
	return nil, errors.New("worker scratch ownership inspection is unsupported on this platform")
}
