package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) requestDeadline(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		budget := s.config.RequestBudget
		switch c.Path() {
		case hubapi.NativeBase + "/claims/wait", hubapi.NativeBase + "/conversations/:conversation/controls":
			budget = max(budget, tracker.NativeLongPollRequestBudget)
		}
		request := c.Request()
		ctx, cancel := s.requestContext(request.Context(), budget)
		defer cancel()
		c.SetRequest(request.WithContext(ctx))
		return next(c)
	}
}

func (s *Service) requestContext(parent context.Context, budget time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := parent.Deadline(); ok {
		return parent, func() {}
	}
	ctx, cancel := s.requestTimeout(parent, budget)
	return hubapi.WithRequestParent(ctx, parent), cancel
}

func (d *database) readTimeoutAttributes(err error) []any {
	if d == nil || d.postgres == nil || !errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	stats := d.postgres.Stats()
	return []any{"read_timeout_cause", "read_deadline", "pool_full", stats.AcquiredConns() == stats.MaxConns(),
		"pool_in_use", stats.AcquiredConns(), "pool_max", stats.MaxConns(),
		"pool_wait_count", stats.EmptyAcquireCount(), "pool_wait_ms", stats.EmptyAcquireWaitTime().Milliseconds(),
		"pool_canceled_acquires", stats.CanceledAcquireCount()}
}
