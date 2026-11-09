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
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func checkSpriteClaimRecord(ctx context.Context, writer tenantstore.Writer, runnerID string, project tracker.ProjectID, request tracker.NativeClaim, now time.Time) error {
	owner, ok := writer.(postgres.SpriteWriter)
	if !ok {
		return accessstore.ErrTransactionRequired
	}
	pool, poolErr := owner.LockSpritePool(ctx)
	if poolErr != nil && !errors.Is(poolErr, corestore.ErrNotFound) {
		return fmt.Errorf("lock Sprite claim pool: %w", poolErr)
	}
	allowed, err := owner.SpriteRunnerAllowed(ctx, runnerID, project, now)
	if err != nil {
		return fmt.Errorf("check Sprite claim membership: %w", err)
	}
	if !allowed {
		return ErrNoClaimableWork
	}
	placement := policy.Placement{Mode: pool.PlacementMode, OverflowSlots: int(pool.OverflowSlots), MaxSpriteSlots: int(pool.MaxSpriteSlots)}
	limit := placement.SpriteLimit()
	if poolErr != nil || limit == 0 {
		return nil
	}
	identity, err := writer.RunnerIdentity(ctx, runnerID)
	if err != nil {
		return fmt.Errorf("read Sprite claim runner: %w", err)
	}
	machine, err := writer.Machine(ctx, string(identity.MachineID))
	if err != nil {
		return fmt.Errorf("read Sprite claim machine: %w", err)
	}
	if machine.SpriteName != machine.Hostname || !validSpritesSlug(machine.SpriteName) {
		return nil
	}
	joining := postgres.SpriteSlotGroup{}
	if request.LandingCapacity > 0 {
		joining = postgres.SpriteSlotGroup{MachineID: identity.MachineID, ProjectID: project, SessionID: request.SessionID}
	}
	used, err := owner.SpriteJobSlotCount(ctx, now, int64(limit), joining)
	if err != nil {
		return fmt.Errorf("count Sprite claim slots: %w", err)
	}
	if used >= int64(limit) {
		return ErrNoClaimableWork
	}
	return nil
}
