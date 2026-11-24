package hubserver

import (
	"context"
	"errors"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) requestDeadline(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		request := c.Request()
		parent := request.Context()
		if _, ok := parent.Deadline(); ok {
			return next(c)
		}
		budget := s.config.RequestBudget
		if c.Path() == hubapi.NativeBase+"/claims/wait" {
			budget = max(budget, tracker.NativeDispatchWaitRequestBudget)
		}
		ctx, cancel := s.requestTimeout(parent, budget)
		defer cancel()
		c.SetRequest(request.WithContext(hubapi.WithRequestParent(ctx, parent)))
		return next(c)
	}
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
