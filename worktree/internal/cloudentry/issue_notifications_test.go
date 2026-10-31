package cloudentry

import (
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
					f := newEntryFixture(t)
					calls := 0
					f.service.config.transport = func(organization Organization) (http.RoundTripper, error) {
						return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
							verifier := cloudassert.Verifier{PublicKeys: []ed25519.PublicKey{f.service.config.SigningKey.Public().(ed25519.PublicKey)}, Issuer: "entry", Audience: organization.ID, Generation: organization.Generation}
							claims, err := verifier.Verify(r.Header.Get(cloudassert.Header), r.Method, r.URL.RequestURI(), raw)
							if err != nil || claims.Kind != cloudassert.KindService || request.Token != test.token {
								t.Errorf("unsubscribe assertion/token: %+v %v", claims, err)
							}
							w.WriteHeader(test.tenantStatus)
						})}, nil
					}
					recorder := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/organizations/"+test.organization+"/"+notification+"/unsubscribe?token="+test.token, nil))
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
