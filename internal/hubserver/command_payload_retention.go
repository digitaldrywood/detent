package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessreceipts"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func commandResponseExpired() error {
	return hubapi.FromFeatureError(accessreceipts.ResponseExpired())
}

func (s *Service) retainCommandPayloads(ctx context.Context, now time.Time) error {
	return s.retainHostedOperationRecords(ctx, now)
}
