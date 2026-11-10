package hubserver

import (
	"context"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
)

func commandResponseExpired() error {
	return &nativeError{Code: "idempotency_expired", Message: receiptstore.ErrExpired.Error(), status: http.StatusGone, publicMessage: true}
}

func (s *Service) retainCommandPayloads(ctx context.Context, now time.Time) error {
	return s.retainHostedOperationRecords(ctx, now)
}
