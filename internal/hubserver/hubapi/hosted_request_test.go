package hubapi_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func TestHostedRequestPanic(t *testing.T) {
	for _, test := range []struct {
		name      string
		committed bool
	}{
		{"before headers", false},
		{"after headers", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var log bytes.Buffer
			handler := hubapi.RecoverHostedRequest(slog.New(slog.NewTextHandler(&log, nil)), "org_example")(func(c echo.Context) error {
				if test.committed {
					c.Response().WriteHeader(http.StatusAccepted)
				}
				panic([]string{"private fixture content"})
			})
			ctx := echo.New().NewContext(httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil), httptest.NewRecorder())
			var failure error
			aborted := func() (recovered any) {
				defer func() { recovered = recover() }()
				failure = handler(ctx)
				return nil
			}()
			if test.committed && aborted != http.ErrAbortHandler || !test.committed && (aborted != nil || !errors.Is(failure, echo.ErrInternalServerError)) {
				t.Fatalf("unexpected recovery: abort=%v error=%v", aborted, failure)
			}
			if output := log.String(); !strings.Contains(output, "organization_id=org_example") || !strings.Contains(output, "function=") || strings.Contains(output, "private fixture content") {
				t.Fatal("panic diagnostic lost scope or exposed customer content")
			}
		})
	}
}
