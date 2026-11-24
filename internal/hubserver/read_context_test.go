package hubserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func TestRequestDeadline(t *testing.T) {
	const budget = 35 * time.Second
	for _, tc := range []struct {
		name   string
		path   string
		caller time.Duration
		want   time.Duration
	}{
		{name: "server budget", path: "/runtime", want: budget},
		{name: "longer caller deadline", path: "/runtime", caller: time.Minute, want: time.Minute},
		{name: "shorter caller deadline", path: "/runtime", caller: time.Second, want: time.Second},
		{name: "dispatch long poll budget", path: hubapi.NativeBase + "/claims/wait", want: 40 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tc.caller > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.caller)
					defer cancel()
				}
				s := Service{config: Config{RequestBudget: budget}}
				e := echo.New()
				e.Use(s.requestDeadline)
				var readCtx context.Context
				e.GET(tc.path, func(c echo.Context) error {
					readCtx = c.Request().Context()
					deadline, ok := readCtx.Deadline()
					if !ok || time.Until(deadline) != tc.want {
						t.Errorf("request deadline = %v, present = %t, want %s from now", deadline, ok, tc.want)
					}
					if got := hubapi.ConnectionContext(readCtx); got != ctx {
						t.Error("connection context lost its caller")
					}
					return c.NoContent(http.StatusOK)
				})
				response := httptest.NewRecorder()
				e.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil).WithContext(ctx))
				if response.Code != http.StatusOK || readCtx == nil {
					t.Fatalf("request did not reach handler, status = %d", response.Code)
				}
				if tc.caller == 0 && readCtx.Err() != context.Canceled {
					t.Errorf("server budget context after request = %v, want canceled", readCtx.Err())
				}
				if ctx.Err() != nil {
					t.Errorf("caller context after request = %v, want active", ctx.Err())
				}
			})
		})
	}
}
