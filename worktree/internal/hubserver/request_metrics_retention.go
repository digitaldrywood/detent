package hubserver

import (
	"context"
	"time"
)

const (
	requestMinuteRetention = 14 * 24 * time.Hour
	requestDayRetention    = 400 * 24 * time.Hour
	requestRetentionBatch  = commandReceiptPruneBatch
)

func (s *Service) expireRequestMetrics(ctx context.Context, now time.Time) error {
	m := s.requestMetrics
	if m == nil {
		return nil
	}

	return m.repository.RetainRequestMetrics(ctx, m.organization, now)
}
