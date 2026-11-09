package hubserver

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func checkSpriteClaimRecord(ctx context.Context, writer tenantstore.Writer, runnerID string, project tracker.ProjectID, now time.Time) error {
	owner, ok := writer.(postgres.SpriteWriter)
	if !ok {
		return accessstore.ErrTransactionRequired
	}
	if _, err := owner.LockSpritePool(ctx); err != nil && !errors.Is(err, corestore.ErrNotFound) {
		return fmt.Errorf("lock Sprite claim pool: %w", err)
	}
	allowed, err := owner.SpriteRunnerAllowed(ctx, runnerID, project, now)
	if err != nil {
		return fmt.Errorf("check Sprite claim membership: %w", err)
	}
	if !allowed {
		return ErrNoClaimableWork
	}
	return nil
}
