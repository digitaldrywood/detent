package webhooks

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenantcall"
	"github.com/labstack/echo/v4"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSharedEntryGitHubWebhookUnavailable(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, secret, signature string
		status                  int
	}{
		{name: "unconfigured", status: http.StatusServiceUnavailable},
		{name: "unsigned", secret: "example-webhook-secret", status: http.StatusUnauthorized},
		{name: "wrong signature", secret: "example-webhook-secret", signature: "sha256=" + strings.Repeat("0", 64), status: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := New(Deps{Secret: []byte(test.secret)})
			entry := echo.New()
			entry.POST("/api/v1/webhooks/github", service.Webhook)
			request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(`{}`))
			request.Header.Set("X-Hub-Signature-256", test.signature)
			response := httptest.NewRecorder()
			entry.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.status, response.Body)
			}
		})
	}

	for _, test := range []struct {
		name          string
		loaded        bool
		pendingStatus int
	}{
		{name: "blocked first load refuses webhook", pendingStatus: http.StatusServiceUnavailable},
		{name: "blocked refresh serves previous routes", loaded: true, pendingStatus: http.StatusAccepted},
	} {
		t.Run(test.name, func(t *testing.T) {
			secret := []byte("example-webhook-secret")
			blockedCall := int64(1)
			pendingDeliveries := int64(0)
			if test.loaded {
				blockedCall++
				pendingDeliveries++
			}
			blocked := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			var workers sync.WaitGroup
			defer func() {
				releaseOnce.Do(func() { close(release) })
				workers.Wait()
			}()
			var alphaCalls, betaCalls, deliveries atomic.Int64
			transport := func(organization tenant.Organization) (http.RoundTripper, error) {
				return roundTripper(func(request *http.Request) (*http.Response, error) {
					status, raw := http.StatusOK, `["example-org/beta"]`
					switch request.URL.Path {
					case "/internal/v1/github/repositories":
						if organization.ID == "org_alpha" {
							raw = `["example-org/alpha-new"]`
							count := alphaCalls.Add(1)
							switch {
							case count == blockedCall:
								close(blocked)
								select {
								case <-release:
								case <-request.Context().Done():
									return nil, request.Context().Err()
								}
								raw = `["example-org/alpha-old"]`
							case count == 1:
								raw = `["example-org/alpha-old"]`
							case count == blockedCall+2:
								status = http.StatusServiceUnavailable
							case count == blockedCall+3:
								raw = `[`
							case count == blockedCall+4:
								return nil, errors.New("example tenant transport failure")
							}
						} else {
							betaCalls.Add(1)
						}
					case "/internal/v1/github/webhook":
						if organization.ID != "org_beta" || request.URL.Query().Get("delivery_id") != "example-delivery" {
							t.Errorf("webhook target=%s query=%s", organization.ID, request.URL.RawQuery)
						}
						deliveries.Add(1)
						status, raw = http.StatusAccepted, `{"duplicate":false}`
					default:
						return nil, errors.New("unexpected tenant request")
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: request}, nil
				}), nil
			}
			caller := tenantcall.New(tenantcall.Deps{Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), Now: time.Now, Transport: transport})
			service := New(Deps{Secret: secret, Logger: slog.New(slog.DiscardHandler), OrganizationNotFound: errors.New("organization is not registered"), ServiceCall: caller.ServiceCall, Claims: caller.Claims, SignedCall: caller.SignedCall,
				ReadyOrganization: func(_ context.Context, id string) (tenant.Organization, error) {
					return tenant.Organization{ID: id, Generation: 1}, nil
				},
			})
			entry := echo.New()
			entry.POST("/api/v1/webhooks/github", service.Webhook)

			alpha, err := service.deps.ReadyOrganization(t.Context(), "org_alpha")
			if err != nil {
				t.Fatal(err)
			}
			beta, err := service.deps.ReadyOrganization(t.Context(), "org_beta")
			if err != nil {
				t.Fatal(err)
			}
			webhook := func() *httptest.ResponseRecorder {
				t.Helper()
				payload := `{"repository":{"full_name":"example-org/beta"}}`
				mac := hmac.New(sha256.New, secret)
				_, _ = mac.Write([]byte(payload))
				request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", strings.NewReader(payload)).WithContext(t.Context())
				request.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
				request.Header.Set("X-GitHub-Delivery", "example-delivery")
				request.Header.Set("X-GitHub-Event", "issues")
				response := httptest.NewRecorder()
				entry.ServeHTTP(response, request)
				return response
			}
			refresh := func(organization tenant.Organization) <-chan error {
				result := make(chan error, 1)
				workers.Add(1)
				go func() {
					defer workers.Done()
					result <- service.Refresh(t.Context(), organization)
				}()
				return result
			}
			if err := service.Refresh(t.Context(), beta); err != nil {
				t.Fatal(err)
			}
			if test.loaded {
				if err := service.Refresh(t.Context(), alpha); err != nil {
					t.Fatal(err)
				}
			}
			first := refresh(alpha)
			<-blocked
			if !service.githubRoutes.mu.TryLock() {
				t.Fatal("blocked tenant service request holds shared routing lock")
			}
			service.githubRoutes.mu.Unlock()
			if err := service.Refresh(t.Context(), beta); err != nil {
				t.Fatal(err)
			}
			if betaCalls.Load() != 2 {
				t.Fatalf("other tenant refresh calls=%d", betaCalls.Load())
			}
			if response := webhook(); response.Code != test.pendingStatus || deliveries.Load() != pendingDeliveries || !test.loaded && !strings.Contains(response.Body.String(), `"code":"tenant_unavailable"`) {
				t.Fatalf("pending routing status=%d deliveries=%d body=%s", response.Code, deliveries.Load(), response.Body.String())
			}
			if test.loaded {
				service.githubRoutes.mu.Lock()
				priorAvailable := slices.Equal(service.githubRoutes.byOrganization[alpha.ID], []string{"example-org/alpha-old"}) && !service.githubRoutes.unavailable[alpha.ID]
				service.githubRoutes.mu.Unlock()
				if !priorAvailable {
					t.Fatal("in-flight refresh changed previously available routing")
				}
			}
			second := refresh(alpha)
			releaseOnce.Do(func() { close(release) })
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if err := <-second; err != nil {
				t.Fatal(err)
			}
			assertRoutes := func(unavailable bool) {
				t.Helper()
				routes := &service.githubRoutes
				routes.mu.Lock()
				defer routes.mu.Unlock()
				if !slices.Equal(routes.byOrganization[alpha.ID], []string{"example-org/alpha-new"}) || !slices.Equal(routes.byOrganization[beta.ID], []string{"example-org/beta"}) || len(routes.byRepository["example-org/alpha-old"]) != 0 || routes.unavailable[alpha.ID] != unavailable || routes.unavailable[beta.ID] {
					t.Fatalf("routes=%v repositories=%v unavailable=%v", routes.byOrganization, routes.byRepository, routes.unavailable)
				}
			}
			assertRoutes(false)
			if response := webhook(); response.Code != http.StatusAccepted || deliveries.Load() != pendingDeliveries+1 {
				t.Fatalf("available routing status=%d deliveries=%d body=%s", response.Code, deliveries.Load(), response.Body.String())
			}
			for _, failure := range []string{"HTTP refusal", "malformed repositories", "transport failure"} {
				if err := service.Refresh(t.Context(), alpha); err == nil {
					t.Fatalf("%s refresh succeeded", failure)
				}
				assertRoutes(true)
				if response := webhook(); response.Code != http.StatusServiceUnavailable || deliveries.Load() != pendingDeliveries+1 {
					t.Fatalf("%s routing status=%d deliveries=%d", failure, response.Code, deliveries.Load())
				}
			}
			if err := service.Refresh(t.Context(), alpha); err != nil {
				t.Fatal(err)
			}
			assertRoutes(false)
			if response := webhook(); response.Code != http.StatusAccepted || deliveries.Load() != pendingDeliveries+2 || alphaCalls.Load() != blockedCall+5 {
				t.Fatalf("recovered routing status=%d deliveries=%d refreshes=%d", response.Code, deliveries.Load(), alphaCalls.Load())
			}
		})
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
