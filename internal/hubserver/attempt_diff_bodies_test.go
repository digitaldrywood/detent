package hubserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func TestDiffBodyRoutesRequireTypedOwnerAndServiceScope(t *testing.T) {
	for _, test := range []struct {
		name, endpoint, kind, audience string
		status                         int
	}{
		{"batch", "batch", cloudassert.KindService, "org_example", http.StatusNotFound},
		{"stored", "stored", cloudassert.KindService, "org_example", http.StatusNotFound},
		{"deleted", "deleted", cloudassert.KindService, "org_example", http.StatusNotFound},
		{"vacuum", "vacuum", cloudassert.KindService, "org_example", http.StatusNotFound},
		{"browser", "exists", cloudassert.KindBrowser, "org_example", http.StatusNotFound},
		{"foreign tenant", "exists", cloudassert.KindService, "org_other", http.StatusNotFound},
		{"missing owner", "exists", cloudassert.KindService, "org_example", http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{config: Config{Logger: discardLogger()}}
			server := echo.New()
			server.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
				return func(c echo.Context) error {
					c.Set(hostedSharedClaimsKey, cloudassert.Claims{Kind: test.kind, Audience: test.audience})
					return next(c)
				}
			})
			service.registerAttemptDiffBodyRoutes(server)
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/diff-bodies/"+test.endpoint, strings.NewReader(`{"key":"orgs/org_example/attempt-diffs/project_example/diff_example/0/`+strings.Repeat("a", 64)+`"}`))
			request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("retired endpoint status=%d, want %d", response.Code, test.status)
			}
		})
	}
}
