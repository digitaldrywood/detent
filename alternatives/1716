package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
)

type widgetTokenProvider struct {
	*hostedSecurityProvider
	users []string
	err   error
}

func (p *widgetTokenProvider) WidgetToken(_ context.Context, user string) (string, error) {
	p.users = append(p.users, user)
	return "widget-token", p.err
}

func TestHostedWidgetToken(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		signedIn bool
		support  bool
		bearer   bool
		body     string
		failed   bool
		status   int
		minted   bool
	}{
		{name: "own user", signedIn: true, body: `{}`, status: http.StatusOK, minted: true},
		{name: "unauthenticated", body: `{}`, status: http.StatusForbidden},
		{name: "other user", signedIn: true, body: `{"user_id":"user_other"}`, status: http.StatusUnprocessableEntity},
		{name: "private unknown property", signedIn: true, body: `{"private-token-email-sentinel":"value"}`, status: http.StatusUnprocessableEntity},
		{name: "mixed API key and session", signedIn: true, bearer: true, body: `{}`, status: http.StatusForbidden},
		{name: "API key alone", bearer: true, body: `{}`, status: http.StatusForbidden},
		{name: "impersonation", signedIn: true, support: true, body: `{}`, status: http.StatusForbidden},
		{name: "provider failure", signedIn: true, body: `{}`, failed: true, status: http.StatusServiceUnavailable, minted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var provider *widgetTokenProvider
			f := newHostedSecurityFixture(t, func(c *Config) {
				provider = &widgetTokenProvider{hostedSecurityProvider: c.Hosted.Provider.(*hostedSecurityProvider)}
				c.Hosted.Provider = provider
				c.Hosted.SupportActors = []string{"staff@example.test"}
			})
			var user hostedSecurityUser
			if test.signedIn {
				support := ""
				if test.support {
					support = "staff@example.test"
				}
				user = f.user(t, "account", "member", "account@example.test", "", support)
			}
			if test.failed {
				provider.err = errors.New("private-token-email-sentinel")
			}
			request := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/org_security/widget-token", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			if test.signedIn {
				request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
				request.Header.Set("X-CSRF-Token", hostedCSRF(user.token))
			}
			if test.bearer {
				request.Header.Set("Authorization", "Bearer other-principal-key")
			}
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if test.minted {
				if len(provider.users) != 1 || provider.users[0] != user.identity.Subject {
					t.Fatalf("minted users = %v", provider.users)
				}
			} else if len(provider.users) != 0 {
				t.Fatalf("unexpected mint for %v", provider.users)
			}
			if test.status == http.StatusOK && (response.Body.String() != `{"token":"widget-token"}`+"\n" || response.Header().Get("Cache-Control") != "no-store") {
				t.Fatalf("response = %s, headers = %v", response.Body.String(), response.Header())
			}
			if strings.Contains(response.Body.String(), "private-token-email-sentinel") {
				t.Fatal("provider error leaked")
			}
		})
	}
}

var _ auth.WidgetTokenProvider = (*widgetTokenProvider)(nil)
