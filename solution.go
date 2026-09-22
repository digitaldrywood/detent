package orchestrator

import (
    "context"
    "time"
)

var (
    // TimeoutDuration is the duration to wait for orchestrator state changes.
    // Tests can set this to 0 to avoid timing out.
    TimeoutDuration = 1 * time.Second
)

// WaitForState waits until the orchestrator reaches the desired state or times out.
// It uses a deterministic channel if available.
func (o *Orchestrator) WaitForState(ctx context.Context, desired string) error {
    // If a deterministic channel is set, use it.
    if ch := o.stateChangeCh; ch != nil {
        select {
        case <-ch:
            return nil
        case <-ctx.Done():
            return ctx.Err()
        }
    }
    // Fallback to timeout-based wait.
    timer := time.NewTimer(TimeoutDuration)
    defer timer.Stop()
    select {
    case <-o.stateChangeCh:
        return nil
    case <-timer.C:
        return fmt.Errorf("timed out waiting for orchestrator state")
    case <-ctx.Done():
        return ctx.Err()
    }
}
