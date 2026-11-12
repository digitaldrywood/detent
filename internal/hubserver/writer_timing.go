package hubserver

import (
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
)

const slowRequest = 250 * time.Millisecond

func (s *Service) timeWriterUse(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := time.Now()
		ctx := c.Request().Context()
		request := requestTimingFrom(ctx)
		if request == nil {
			request = &requestTiming{}
			ctx = withRequestTiming(ctx, request)
		}
		ctx = postgres.WithTransactionTiming(ctx, request.span, func(started time.Time, query string) { request.statement(started, query, nil) })
		c.SetRequest(c.Request().WithContext(ctx))
		err := next(c)
		elapsed := time.Since(start)
		if elapsed >= slowRequest {
			attrs := []any{"method", c.Request().Method, "route", c.Path(), "status", c.Response().Status,
				"duration_ms", elapsed.Milliseconds()}
			attrs = append(attrs, request.logAttrs()...)
			s.config.Logger.Info("hub request timing", attrs...)
		}
		s.logWriterWaits()
		return err
	}
}

func (s *Service) logWriterWaits() {
	if s.database == nil || s.database.postgres == nil {
		return
	}
	now := time.Now().UnixNano()
	last := s.writerStatsAt.Load()
	if now-last < int64(time.Minute) || !s.writerStatsAt.CompareAndSwap(last, now) {
		return
	}

	stats := s.database.postgres.Stats()
	waits, waited := stats.EmptyAcquireCount()-s.writerWaits.Swap(stats.EmptyAcquireCount()), stats.EmptyAcquireWaitTime()-time.Duration(s.writerWaited.Swap(int64(stats.EmptyAcquireWaitTime())))
	if last == 0 {
		return
	}
	serving, background := s.database.postgres.WorkloadCapacities()
	s.config.Logger.Info("hub PostgreSQL pool waits", "waits", waits, "wait_ms", waited.Milliseconds(), "in_use", stats.AcquiredConns(), "max_open", stats.MaxConns(), "serving_capacity", serving, "background_capacity", background, "interval_s", (now-last)/int64(time.Second))
}
