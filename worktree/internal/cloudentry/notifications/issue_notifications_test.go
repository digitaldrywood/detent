package notifications

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entrytest"
	"github.com/digitaldrywood/detent/internal/cloudentry/origin"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenantcall"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func TestIssueUnsubscribeWithoutSession(t *testing.T) {
	for _, notification := range []string{"issue-notifications", "daily-summary"} {
		t.Run(notification, func(t *testing.T) {
			for _, test := range []struct {
				name, organization, token string
				tenantStatus, want, calls int
			}{
				{"valid", "org_alpha", "unsub_" + strings.Repeat("a", 32), 204, 200, 1},
				{"replay", "org_beta", "unsub_" + strings.Repeat("b", 32), 204, 200, 1},
				{"unknown token", "org_alpha", "unsub_" + strings.Repeat("c", 32), 404, 404, 1},
				{"mail tenant unavailable", "org_alpha", "unsub_" + strings.Repeat("d", 32), 503, 503, 1},
				{"malformed token", "org_alpha", "bad", 204, 404, 0},
				{"unknown organization", "org_missing", "unsub_" + strings.Repeat("a", 32), 204, 404, 0},
			} {
				t.Run(test.name, func(t *testing.T) {
					key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
					calls := 0
					transport := func(organization tenant.Organization) (http.RoundTripper, error) {
						return entrytest.HandlerTransport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls++
							if organization.ID != test.organization || r.URL.Path != "/internal/v1/"+notification+"/unsubscribe" || r.Method != http.MethodPost {
								t.Errorf("incorrect route: %s %s", organization.ID, r.URL.Path)
							}
							var request struct {
								Token string `json:"token"`
							}
							if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
								t.Fatal(err)
							}
							raw, err := json.Marshal(request)
							if err != nil {
								t.Fatal(err)
							}
							verifier := cloudassert.Verifier{PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Issuer: "entry", Audience: organization.ID, Generation: organization.Generation}
							claims, err := verifier.Verify(r.Header.Get(cloudassert.Header), r.Method, r.URL.RequestURI(), raw)
							if err != nil || claims.Kind != cloudassert.KindService || request.Token != test.token {
								t.Errorf("unsubscribe assertion/token: %+v %v", claims, err)
							}
							w.WriteHeader(test.tenantStatus)
						})}, nil
					}
					caller := tenantcall.New(tenantcall.Deps{Issuer: "entry", SigningKey: key, Now: time.Now, Transport: transport})
					service := New(Deps{ReadyOrganization: func(_ context.Context, id string) (tenant.Organization, error) {
						switch id {
						case "org_alpha", "org_beta":
							return tenant.Organization{ID: id, Generation: 1}, nil
						default:
							return tenant.Organization{}, errors.New("organization is not registered")
						}
					}, ServiceRequest: caller.ServiceRequest})
					entry := echo.New()
					entry.Pre(origin.New(origin.Deps{}).Boundary)
					service.Register(entry)
					recorder := httptest.NewRecorder()
					entry.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/organizations/"+test.organization+"/"+notification+"/unsubscribe?token="+test.token, nil))
					if recorder.Code != test.want || calls != test.calls {
						t.Fatalf("status %d, calls %d: %s", recorder.Code, calls, recorder.Body.String())
					}
					if recorder.Header().Get("Location") != "" {
						t.Fatal("unsubscribe required login")
					}
					if recorder.Header().Get("Referrer-Policy") != "no-referrer" || recorder.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("unsubscribe exposed cached capability")
					}
				})
			}
		})
	}
}
