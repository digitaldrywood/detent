package hubserver

import (
	"context"
	"errors"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type accessCommandSessionProvider struct {
	*browserHostedProvider
	calls, failAt int
	err           error
}

func (p *accessCommandSessionProvider) CurrentSession(ctx context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	p.calls++
	if p.calls == p.failAt {
		return auth.HostedIdentity{}, p.err
	}
	return p.browserHostedProvider.CurrentSession(ctx, identity)
}

func TestPostgresAccessTokenMutationIdentityOutage(t *testing.T) {
	f := newSharedRouteFixture(t, func(cfg *Config) { cfg.Hosted.SharedEntry = nil })
	for org, service := range f.services {
		provider := &accessCommandSessionProvider{browserHostedProvider: &browserHostedProvider{members: map[string]auth.Membership{}, sessions: map[string]auth.HostedIdentity{}}}
		identity := provider.identity(org+"_owner", org+"_owner@example.test", "provider_"+org, "")
		member := auth.Membership{ID: org + "_owner", UserID: org + "_owner", OrganizationID: "provider_" + org, Status: "active"}
		member.Role.Slug = "owner"
		provider.members[member.ID] = member
		service.config.Hosted.Provider = provider
		token, _, err := service.hostedSessions.CreateIdentitySession(t.Context(), identity)
		if err != nil {
			t.Fatal(err)
		}
		for _, fault := range []struct {
			name string
			err  error
			want int
		}{
			{"storage", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
			{"unavailable", &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable}, http.StatusServiceUnavailable},
			{"outage before identity denial", errors.Join(tenantauth.ErrUnavailable, auth.ErrHostedIdentity), http.StatusServiceUnavailable},
			{"missing identity", auth.ErrHostedIdentity, http.StatusForbidden},
		} {
			t.Run(org+"/"+fault.name, func(t *testing.T) {
				provider.calls, provider.failAt, provider.err = 0, 4, fault.err
				request := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+org+"/api-keys", strings.NewReader(`{"name":"Example key","scope":"admin","never_expires":true,"project_access":"all"}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-CSRF-Token", hostedCSRF(token))
				request.AddCookie(&http.Cookie{Name: hostedCookie, Value: token})
				response := httptest.NewRecorder()
				service.Handler().ServeHTTP(response, request)
				requireEntryAuthResponse(t, response.Code, response.Body.String(), fault.want)
				if provider.calls != provider.failAt {
					t.Fatalf("session calls=%d want=%d", provider.calls, provider.failAt)
				}
			})
		}
	}
}
