package runnersession

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubclient/transport"
)

type IdentityWriter interface {
	UpdateSessionIdentity(context.Context, int64, agentidentity.Identity) error
}

func ObserveIdentity(writer IdentityWriter, sessionID int64, acknowledged agentidentity.Identity) func(context.Context, agentidentity.Identity) error {
	return func(ctx context.Context, current agentidentity.Identity) error {
		if writer == nil || sessionID <= 0 || current.IsZero() || acknowledged.MateriallyEqual(current) {
			return nil
		}
		if err := writer.UpdateSessionIdentity(ctx, sessionID, current); err != nil {
			if transport.DeferObservation(ctx, err) {
				return nil
			}
			return fmt.Errorf("update agent session identity: %w", err)
		}
		acknowledged = current
		return nil
	}
}
