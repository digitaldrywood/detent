package hubserver

import (
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPostgresLogoutRevokesExpiredSessions(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			provider := newHostedSecurityProvider()
			service := &Service{database: &database{}, config: Config{AccessRepository: repository, TenantRepository: repository, now: func() time.Time { return now }, Hosted: &HostedConfig{OrganizationID: string(org), PublicURL: "http://example.test", Provider: provider}}}
			hash := apikey.HashToken("expired-token")
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now}); err != nil {
					return err
				}
				return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: org, Hash: hash, Subject: "member", Email: "member@example.test", ProviderOrganizationID: "provider", SessionID: "session_" + string(org), CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour), IdentityCreatedAt: now.Add(-2 * time.Hour), IdentityExpiresAt: now.Add(-time.Hour)})
			})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/logout", nil)
			request.AddCookie(&http.Cookie{Name: hostedCookie, Value: "expired-token"})
			response := httptest.NewRecorder()
			if err := service.logoutHosted(echo.New().NewContext(request, response)); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusSeeOther || len(provider.revoked) != 1 || provider.revoked[0] != "session_"+string(org) {
				t.Fatalf("logout status=%d revoked=%v", response.Code, provider.revoked)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.AccessSession(t.Context(), hash)
				if err == nil && row.RevokedAt == nil {
					t.Error("local session remains active")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
