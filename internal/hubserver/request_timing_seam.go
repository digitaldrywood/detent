package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/requesttiming"
	"github.com/labstack/echo/v4"
)

type requestTiming = requesttiming.Timing
type timingSpan = requesttiming.Span
type timedJSONSerializer = requesttiming.JSONSerializer

func withRequestTiming(ctx context.Context, timing *requestTiming) context.Context {
	return requesttiming.With(ctx, timing)
}
func requestTimingFrom(ctx context.Context) *requestTiming { return requesttiming.From(ctx) }
func timeSpan(ctx context.Context, name string) func()     { return requesttiming.TimeSpan(ctx, name) }
func (s *Service) serverTiming(next echo.HandlerFunc) echo.HandlerFunc {
	return requesttiming.Server(func(c echo.Context) bool { _, ok := c.Get("hub_api_credential").(apiCredential); return ok })(next)
}
func (s *Service) timeWriterUse(next echo.HandlerFunc) echo.HandlerFunc {
	return requesttiming.New(requesttiming.Deps{Bind: func(ctx context.Context, timing *requestTiming) context.Context {
		return postgres.WithTransactionTiming(ctx, timing.AddSpan, func(at time.Time, query string) { timing.Statement(at, query, nil) })
	}, Logger: s.config.Logger, After: s.logWriterWaits}).Writer(next)
}
