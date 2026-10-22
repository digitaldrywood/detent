package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type operatorMembershipProvider struct {
	auth.HostedProvider
	remote auth.HostedProvider
}

func (p operatorMembershipProvider) Memberships(ctx context.Context, user, organization string) ([]auth.Membership, error) {
	return p.remote.Memberships(ctx, user, organization)
}

type operatorMembershipTransport func(*http.Request) (*http.Response, error)

func (f operatorMembershipTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOperatorMembershipProviderAvailability(t *testing.T) {
	for _, test := range []struct {
		name       string
		cached     bool
		outage     bool
		missing    bool
		revokedKey bool
		wantError  error
		wantStatus int
	}{
		{name: "cached membership during outage", cached: true, outage: true, wantStatus: http.StatusOK},
		{name: "uncached outage", outage: true, wantError: operatortool.ErrProviderUnavailable, wantStatus: http.StatusServiceUnavailable},
		{name: "missing membership", missing: true, wantError: operatortool.ErrAccessDenied, wantStatus: http.StatusUnauthorized},
		{name: "revoked key with cached membership", cached: true, outage: true, revokedKey: true, wantError: operatortool.ErrAccessDenied, wantStatus: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			user := f.user(t, "operator", "owner", "operator@example.test", "write", "")
			credential, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
			if err != nil {
				t.Fatal(err)
			}
			expiry := time.Now().Add(time.Hour)
			key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "example-client", Scope: apiScopeAdmin, Issuer: &credential, KeyScope: apikey.ScopeAdmin, ExpiresAt: &expiry, ProjectAccess: hostedProjectsAll})
			if err != nil {
				t.Fatal(err)
			}
			captured := make(chan context.Context, 4)
			const route = "/api/v2/organizations/org_security/membership-authority-test"
			f.service.echo.GET(route, func(c echo.Context) error {
				captured <- c.Request().Context()
				return c.NoContent(http.StatusOK)
			}, f.service.operatorAuthority)
			serve := func(path string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				r.Header.Set("Authorization", "Bearer "+key.Token)
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, r)
				return response
			}
			requireNativeStatus(t, serve(route), http.StatusOK)
			ctx := <-captured
			calls, outage := 0, false
			remote, err := auth.NewHostedProvider("workos", auth.WorkOSConfig{
				ClientID: "client_example", APIKey: "fixture-secret", RedirectURL: "https://app.example.com/callback",
				HTTPClient: &http.Client{Transport: operatorMembershipTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if outage {
						return nil, errors.New("dial: connection reset")
					}
					members, err := f.provider.Memberships(t.Context(), r.URL.Query().Get("user_id"), r.URL.Query().Get("organization_id"))
					if err != nil {
						return nil, err
					}
					if test.missing {
						members = nil
					}
					body, err := json.Marshal(map[string]any{"data": members})
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
				})},
			})
			if err != nil {
				t.Fatal(err)
			}
			f.service.config.Hosted.Provider = operatorMembershipProvider{HostedProvider: f.provider, remote: remote}
			if test.cached {
				if _, err := f.service.hostedMembership(t.Context(), user.identity.Hosted); err != nil {
					t.Fatal(err)
				}
			}
			outage = test.outage
			if test.revokedKey {
				operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE token_hash=?", formatHubTime(time.Now()), apikey.HashToken(key.Token))
			}
			for _, scope := range []apikey.Scope{apikey.ScopeRead, apikey.ScopeWrite, apikey.ScopeAdmin} {
				_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: scope, ProjectID: string(f.project)})
				if !errors.Is(err, test.wantError) {
					t.Fatalf("scope=%s error=%v want=%v", scope, err, test.wantError)
				}
				if test.outage && !test.cached && errors.Is(err, operatortool.ErrAccessDenied) {
					t.Fatal("provider outage became access_denied")
				}
			}
			for _, call := range []operatortool.Call{
				{Name: operatortool.OrganizationSession, Arguments: json.RawMessage(`{}`)},
				{Name: operatortool.WorkList, Arguments: json.RawMessage(`{"project_id":"prj_security"}`)},
			} {
				if _, err := (hostedOperatorExecutor{f.service}).Execute(ctx, call); !errors.Is(err, test.wantError) {
					t.Fatalf("tool=%s err=%v want=%v", call.Name, err, test.wantError)
				}
			}
			for _, path := range []string{route, f.base} {
				response := serve(path)
				requireNativeStatus(t, response, test.wantStatus)
				if errors.Is(test.wantError, operatortool.ErrProviderUnavailable) {
					requireNativeCode(t, response, http.StatusServiceUnavailable, "provider_unavailable")
				}
			}
			if !test.revokedKey {
				browser := f.request(t, user, http.MethodGet, route, nil)
				if errors.Is(test.wantError, operatortool.ErrProviderUnavailable) {
					requireNativeCode(t, browser, http.StatusServiceUnavailable, "provider_unavailable")
				} else if test.cached {
					requireNativeStatus(t, browser, http.StatusOK)
				}
			}
			if test.cached && calls != 1 {
				t.Fatalf("cached requests called WorkOS %d times, want 1", calls)
			}
		})
	}
}
