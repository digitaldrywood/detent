package access_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresLogoutRevokesExpiredSessions(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	provider := hubfixture.NewHostedProvider()
	cfg := hubfixture.HostedSecurityConfig(provider)
	cfg.Clock = func() time.Time { return now }
	cfg.Hosted.OrganizationID = string(hubfixture.Primary)
	cfg.Hosted.WorkOSOrganizationID = "provider_" + string(hubfixture.Primary)
	h := hubfixture.OpenHosted(t, cfg)
	h.EnsureOrganization(t, hubfixture.Secondary)
	const token = "expired-token"
	hash := apikey.HashToken(token)
	for _, organization := range []tracker.OrganizationID{h.Primary, hubfixture.Secondary} {
		if err := h.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessSession(t.Context(), accessstore.Session{
				OrganizationID: organization, Hash: hash, Subject: "member", Email: "member@example.test",
				ProviderOrganizationID: "provider_" + string(organization), SessionID: "session_" + string(organization),
				CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
				IdentityCreatedAt: now.Add(-2 * time.Hour), IdentityExpiresAt: now.Add(-time.Hour),
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/logout", nil)
	request.AddCookie(&http.Cookie{Name: hubfixture.HostedCookie, Value: token})
	request.Header.Set("X-CSRF-Token", hubfixture.HostedCSRF(token))
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, request)
	hubfixture.RequireStatus(t, response, http.StatusSeeOther)
	sessionID := "session_" + string(h.Primary)
	if len(provider.Revoked) != 1 || provider.Revoked[0] != sessionID {
		t.Fatalf("revoked sessions = %v, want %s", provider.Revoked, sessionID)
	}
	for _, organization := range []tracker.OrganizationID{h.Primary, hubfixture.Secondary} {
		if err := h.Repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.AccessSession(t.Context(), hash)
			if err == nil && (row.RevokedAt != nil) != (organization == h.Primary) {
				t.Errorf("session revocation for %s = %v", organization, row.RevokedAt)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
