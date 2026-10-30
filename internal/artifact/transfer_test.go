package artifact

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"
)

func TestTransfersUseOperationDeadline(t *testing.T) {
	for _, side := range []string{"client", "server"} {
		t.Run(side, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()
				transfer := func(ctx context.Context) error {
					timer := time.NewTimer(31 * time.Second)
					defer timer.Stop()
					select {
					case <-timer.C:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				if side == "client" {
					client := &Client{Origin: "http://localhost", Token: func(context.Context) (string, error) { return "worker", nil }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if err := transfer(r.Context()); err != nil {
							return nil, err
						}
						return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
					})}
					if _, err := client.Reserve(ctx, Reservation{}); err != nil {
						t.Fatal(err)
					}
				} else {
					server := &HTTPServer{}
					c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), httptest.NewRecorder())
					if err := server.boundary(func(c echo.Context) error { return transfer(c.Request().Context()) })(c); err != nil {
						t.Fatal(err)
					}
				}
			})
		})
	}
}
