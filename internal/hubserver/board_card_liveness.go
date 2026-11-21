package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func boardCardPointers(cards []tracker.NativeBoardCard) []*tracker.NativeBoardCard {
	pointers := make([]*tracker.NativeBoardCard, len(cards))
	for i := range cards {
		pointers[i] = &cards[i]
	}
	return pointers
}

func (s *Service) readBoardCardLiveness(ctx context.Context, organization tracker.OrganizationID, now time.Time, cards []*tracker.NativeBoardCard) error {
	runners := map[string]bool{}
	for _, card := range cards {
		if a := card.Attempt; a != nil && a.Status == "running" && now.Before(a.ExpiresAt) && a.RunnerID != "" {
			runners[a.RunnerID] = true
		}
		if e := card.Execution; e != nil && (e.Running || e.Stage != "") && now.Before(e.ExpiresAt) && e.RunnerID != "" {
			runners[e.RunnerID] = true
		}
	}
	var fresh map[string]bool
	if len(runners) != 0 {
		ids := make([]string, 0, len(runners))
		for id := range runners {
			ids = append(ids, id)
		}
		if err := s.config.TenantRepository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(runnerstore.HeartbeatReader)
			if !ok {
				return hubapi.NativeWorkItemStorageUnavailable()
			}
			var err error
			fresh, err = reader.FreshRunnerHeartbeats(ctx, ids, now)
			return err
		}); err != nil {
			return err
		}
	}
	for _, card := range cards {
		if a := card.Attempt; a != nil && a.Status == "running" && (!fresh[a.RunnerID] || !now.Before(a.ExpiresAt)) {
			attempt := *a
			attempt.Status = "interrupted"
			card.Attempt = &attempt
		}
		if e := card.Execution; e != nil && (e.Running || e.Stage != "") && (!fresh[e.RunnerID] || !now.Before(e.ExpiresAt)) {
			execution := *e
			execution.Running, execution.Stage = false, ""
			card.Execution = &execution
		}
		card.StatusLine = nativeBoardStatusLine(*card)
	}
	return nil
}
