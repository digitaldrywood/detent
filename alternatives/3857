package requesttiming

import (
	"context"
	"log/slog"
	"time"

	"github.com/labstack/echo/v4"
)

const slowRequest = 250 * time.Millisecond

type Deps struct {
	Bind   func(context.Context, *Timing) context.Context
	Logger *slog.Logger
	After  func()
}
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }
func (s *Service) Writer(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := time.Now()
		ctx := c.Request().Context()
		request := From(ctx)
		if request == nil {
			request = &Timing{}
			ctx = With(ctx, request)
		}
		ctx = s.deps.Bind(ctx, request)
		c.SetRequest(c.Request().WithContext(ctx))
		err := next(c)
		elapsed := time.Since(start)
		if elapsed >= slowRequest {
			attrs := []any{"method", c.Request().Method, "route", c.Path(), "status", c.Response().Status,
				"duration_ms", elapsed.Milliseconds()}
			attrs = append(attrs, request.LogAttrs()...)
			s.deps.Logger.Info("hub request timing", attrs...)
		}
		s.deps.After()
		return err
	}
}
