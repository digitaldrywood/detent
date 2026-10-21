//go:build !windows

package cloudentry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/billing"
)

type previewBillingProvider struct{}

func (previewBillingProvider) Reconcile(context.Context, billing.Binding) (billing.Snapshot, error) {
	return billing.Snapshot{Status: "free"}, nil
}

func (previewBillingProvider) Checkout(_ context.Context, request billing.CheckoutRequest) (billing.Session, error) {
	return billing.Session{ID: "cs_test_preview", URL: "https://checkout.stripe.com/c/pay/cs_test_preview", ExpiresAt: request.ExpiresAt}, nil
}

func (previewBillingProvider) Portal(context.Context, billing.Binding, string, string) (billing.Session, error) {
	return billing.Session{ID: "bps_test_preview", URL: "https://billing.stripe.com/p/session/test_preview"}, nil
}

// TestSharedOriginClientPreview serves the shared entry with the real Cloud
// client and fixture WorkOS and Stripe providers on an ephemeral loopback
// origin, with no organization seeded, so a browser can walk the whole
// journey from sign-in. It is skipped unless
// DETENT_SHARED_ORIGIN_CLIENT_PREVIEW names a private directory for the
// fixture file.
func TestSharedOriginClientPreview(t *testing.T) {
	directory := os.Getenv("DETENT_SHARED_ORIGIN_CLIENT_PREVIEW")
	if directory == "" {
		t.Skip("set DETENT_SHARED_ORIGIN_CLIENT_PREVIEW to a private directory to serve the shared-origin client preview")
	}
	if os.Getenv("DETENT_ENTRY_FALLBACK_PREVIEW") == "1" {
		serveEntryFallbackPreview(t, directory)
		return
	}
	p := newSharedOriginPilotWith(t, 3, 3, detent.StaticFS(), previewBillingProvider{})
	stop := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__preview/authorize", p.previewAuthorize)
	mux.HandleFunc("POST /__preview/stop", func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(stop) })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", p.service.Handler())
	var handler http.Handler = mux
	p.handler.handler.Store(&handler)
	p.provider.mu.Lock()
	p.provider.authorizeBase = p.base
	p.provider.mu.Unlock()
	raw, err := json.MarshalIndent(map[string]string{"origin": p.base, "stop": p.base + "/__preview/stop", "tenant_root": p.roots[0]}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "shared-origin-client-preview.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("shared-origin client preview: %s", p.base)
	timer := time.NewTimer(30 * time.Minute)
	defer timer.Stop()
	select {
	case <-stop:
	case <-timer.C:
	case <-t.Context().Done():
	}
}

func serveEntryFallbackPreview(t *testing.T, directory string) {
	t.Helper()
	f := newEntryFixture(t)
	transport, stopTenant := entryTenantTransport(t, f.tenants[testSocketEndpoint("alpha.sock")])
	f.service.config.transport = func(Organization) (http.RoundTripper, error) { return transport, nil }
	stop := make(chan struct{})
	var once sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("POST /__preview/stop-tenant", func(w http.ResponseWriter, _ *http.Request) {
		stopTenant()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__preview/outage", func(w http.ResponseWriter, _ *http.Request) {
		f.provider.mu.Lock()
		f.provider.keysDown = true
		f.provider.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__preview/stop", func(w http.ResponseWriter, _ *http.Request) {
		once.Do(func() { close(stop) })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", f.service.Handler())
	server := httptest.NewUnstartedServer(mux)
	f.service.config.PublicURL = "http://" + server.Listener.Addr().String()
	server.Start()
	defer server.Close()
	raw, err := json.Marshal(map[string]string{"origin": server.URL, "stop": server.URL + "/__preview/stop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "shared-origin-client-preview.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("shared-origin client preview: %s", server.URL)
	select {
	case <-stop:
	case <-t.Context().Done():
	}
}
