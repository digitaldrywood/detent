package hubapi

import (
	"log/slog"
	"net/http"
	"runtime"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func RecoverHostedRequest(logger *slog.Logger, organization tracker.OrganizationID) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (err error) {
			defer func() {
				switch value := recover(); value {
				case nil:
					return
				case http.ErrAbortHandler:
					panic(http.ErrAbortHandler)
				default:
					var pcs [8]uintptr
					frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs[:])])
					frame, _ := frames.Next()
					logger.ErrorContext(c.Request().Context(), "hosted request panicked", "organization_id", organization, "function", frame.Function, "line", frame.Line)
					if c.Response().Committed {
						panic(http.ErrAbortHandler)
					}
					err = echo.ErrInternalServerError
				}
			}()
			return next(c)
		}
	}
}
