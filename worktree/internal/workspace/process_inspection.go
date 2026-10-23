package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func scratchProcessInspectionError(ctx context.Context, pid int, operation string, inspectionErr error, alive func(context.Context) (bool, error)) error {
	ctx, cancel := context.WithTimeoutCause(ctx, 100*time.Millisecond, errors.New("process exit not confirmed within 100ms"))
	defer cancel()
	for {
		if err := context.Cause(ctx); err != nil {
			inspectionErr = errors.Join(inspectionErr, err)
			break
		}
		running, err := alive(ctx)
		if err != nil {
			inspectionErr = errors.Join(inspectionErr, err)
			break
		}
		if !running {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	return fmt.Errorf("inspect worker %s for process %d: %w", operation, pid, inspectionErr)
}
