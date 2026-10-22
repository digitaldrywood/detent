package cloudentry

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
)

func TestHostedOriginCompatibility(t *testing.T) {
	t.Parallel()
	for _, environment := range []struct{ name, canonical, legacy string }{
		{"production", "cloud.detent.build", "hub.detent.build"},
		{"staging", "staging.cloud.detent.build", "staging.hub.detent.build"},
	} {
		for _, test := range []struct {
			name, method, path, header, value string
			redirect                          bool
		}{
			{"bookmark", "GET", "/organizations/org_one/work?view=a%2Fb&x=1&y=2", "", "", true},
			{"head", "HEAD", "/?return=%2Forganizations", "", "", true},
			{"invitation", "GET", "/invite?invitation_token=fixture", "Cookie", "__Host-detent_session=existing", true},
			{"new login", "GET", "/auth/oidc/start?return=%2Forganizations", "Cookie", "__Host-detent_session=existing", true},
			{"in-flight callback", "GET", "/auth/oidc/callback?code=fixture&state=fixture", "Cookie", "__Host-detent_login_fixture=transaction", false},
			{"existing session", "GET", "/organizations/org_one/work", "Cookie", "__Host-detent_session=existing", false},
			{"runner read", "GET", "/organizations/org_one/api/v1/work-items/1?cursor=2", "", "", false},
			{"runner mutation", "POST", "/organizations/org_one/api/v1/machines/register", "Authorization", "Bearer fixture", false},
			{"enrollment", "POST", "/organizations/org_one/api/v1/runners/enroll", "", "", false},
			{"bearer stream", "GET", "/organizations/org_one/events", "Authorization", "Bearer fixture", false},
			{"websocket", "GET", "/organizations/org_one/ws", "Upgrade", "websocket", false},
			{"entry API", "GET", "/api/cloud/session", "", "", false},
			{"webhook", "POST", "/webhooks/stripe/live?delivery=fixture", "Stripe-Signature", "t=1,v1=fixture", false},
			{"form mutation", "POST", "/organizations", "Cookie", "__Host-detent_session=existing", false},
		} {
			t.Run(environment.name+"/"+test.name, func(t *testing.T) {
				t.Parallel()
				s := &Service{config: Config{PublicURL: "https://" + environment.canonical}, secure: true}
				r := httptest.NewRequest(test.method, "https://"+environment.legacy+test.path, strings.NewReader("signed payload"))
				if test.header != "" {
					r.Header.Set(test.header, test.value)
				}
				recorder := httptest.NewRecorder()
				handler := s.boundary(func(c echo.Context) error {
					body, err := io.ReadAll(c.Request().Body)
					if err != nil || string(body) != "signed payload" || c.Request().URL.RequestURI() != test.path || c.Request().Host != environment.legacy {
						t.Fatalf("proxy changed request: %q, %v, %s", body, err, c.Request().URL)
					}
					if test.header != "" && c.Request().Header.Get(test.header) != test.value {
						t.Fatal("proxy changed integration header")
					}
					return c.NoContent(http.StatusNoContent)
				})
				if err := handler(echo.New().NewContext(r, recorder)); err != nil {
					t.Fatal(err)
				}
				want := http.StatusNoContent
				if test.redirect {
					want = http.StatusTemporaryRedirect
					if location := recorder.Header().Get("Location"); location != s.config.PublicURL+test.path {
						t.Fatalf("Location = %q", location)
					}
				} else if recorder.Header().Get("Location") != "" {
					t.Fatal("integration request redirected")
				}
				if recorder.Code != want {
					t.Fatalf("status = %d, want %d", recorder.Code, want)
				}
			})
		}
	}
}

func TestHostedOriginEnvironmentBoundaries(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, canonical, host, origin string
		legacy, sameOrigin            bool
	}{
		{"production alias", "https://cloud.detent.build", "hub.detent.build", "https://hub.detent.build", true, true},
		{"staging alias", "https://staging.cloud.detent.build", "staging.hub.detent.build", "https://staging.hub.detent.build", true, true},
		{"canonical", "https://cloud.detent.build", "cloud.detent.build", "https://cloud.detent.build", false, true},
		{"legacy origin on canonical host", "https://cloud.detent.build", "cloud.detent.build", "https://hub.detent.build", false, false},
		{"staging at production", "https://cloud.detent.build", "staging.hub.detent.build", "https://staging.hub.detent.build", false, false},
		{"production at staging", "https://staging.cloud.detent.build", "hub.detent.build", "https://hub.detent.build", false, false},
		{"attacker at alias", "https://cloud.detent.build", "hub.detent.build", "https://attacker.test", true, false},
		{"missing origin", "https://cloud.detent.build", "hub.detent.build", "", true, false},
		{"self-hosted", "https://entry.example.com", "hub.detent.build", "https://hub.detent.build", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			s := &Service{config: Config{PublicURL: test.canonical}, secure: true}
			r := httptest.NewRequest("POST", "https://"+test.host+"/organizations", nil)
			r.Header.Set("Origin", test.origin)
			r.Header.Set("X-Forwarded-Host", "hub.detent.build")
			if got := s.legacyHostedRequest(r); got != test.legacy {
				t.Fatalf("legacy = %t, want %t", got, test.legacy)
			}
			if got := s.sameOrigin(echo.New().NewContext(r, httptest.NewRecorder())); got != test.sameOrigin {
				t.Fatalf("sameOrigin = %t, want %t", got, test.sameOrigin)
			}
		})
	}
}
