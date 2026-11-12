package dailysummary

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type summaryEmailSender func(context.Context, auth.EmailMessage) error

func (f summaryEmailSender) SendEmail(ctx context.Context, message auth.EmailMessage) error {
	return f(ctx, message)
}

func summaryHTTPFixture(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, now time.Time) (*hubtest.HTTPHarness, *Service) {
	t.Helper()
	hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Now: now})
	service := New(Deps{Repository: repository, AccessRepository: repository, Now: func() time.Time { return now }, Organization: func() tracker.OrganizationID { return organization }})
	harness := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repository, Organization: organization, Now: func() time.Time { return now }, Hosted: &hubauth.Hosted{}}, func(e *echo.Group, deps hubtest.HTTPDeps) {
		options := HTTPDeps{Service: func() *Service { return service }, Auth: func() *hubauth.Authenticator { return deps.Auth }, Error: deps.Error, HostedError: deps.Error, InvalidRequest: deps.Error,
			JSONError: func(c echo.Context, status int, message string) error {
				return c.JSON(status, map[string]string{"code": "forbidden", "message": message})
			},
			ValidToken: func(token string) bool {
				value, ok := strings.CutPrefix(token, "unsub_")
				if !ok || len(value) != 32 {
					return false
				}
				_, err := hex.DecodeString(value)
				return err == nil
			},
			Session: func(next echo.HandlerFunc) echo.HandlerFunc { return next },
		}
		RegisterPreferenceRoutes(e, options)
		RegisterUnsubscribeRoute(e, options)
		RegisterSharedUnsubscribeRoute(e, options)
	})
	return harness, service
}

func summaryRequest(f *hubtest.HTTPHarness, t *testing.T, actor hubtest.HTTPActor, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return f.Request(t, actor, method, target, body).Recorder
}

func requireNativeStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
	}
}

func decodeSummaryResponse(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}
