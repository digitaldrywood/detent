package hubserver

import (
	"context"
	"net/http"
	"time"
)

func commandResponseExpired() error {
	return &nativeError{Code: "idempotency_expired", Message: "The command completed; its response replay horizon has passed. The command will not be repeated", status: http.StatusGone, publicMessage: true}
}

func (s *Service) retainCommandPayloads(ctx context.Context, now time.Time) error {
	return s.retainHostedOperationRecords(ctx, now)
}

func commandCompletionContent(ctx context.Context) context.Context {
	if content, ok := ctx.Value(changeContentKey{}).(*changeContent); ok {
		content.mu.Lock()
		content.completion = true
		content.mu.Unlock()
	}
	return ctx
}
