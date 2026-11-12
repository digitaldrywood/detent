package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessreceipts"
)

func commandResponseExpired() error {
	return fromFeatureError(accessreceipts.ResponseExpired())
}

func (s *Service) retainCommandPayloads(ctx context.Context, now time.Time) error {
	return s.retainHostedOperationRecords(ctx, now)
}
