package cloudentry

import (
	"bytes"
	"crypto/ed25519"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

type sizedTransport struct{ size int }

func (t sizedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), t.size)))}, nil
}

func TestMachineCallRefusesOversizedTenantAnswers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "at the limit", size: tenantCallResponseLimit},
		{name: "one byte over", size: tenantCallResponseLimit + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			seed := make([]byte, ed25519.SeedSize)
			service, err := Open(t.Context(), Config{Platform: PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"}, PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(seed), Provider: newFakeProvider(),
				StateDir: t.TempDir(), Logger: slog.New(slog.DiscardHandler), clientFS: fstest.MapFS{},
				transport: func(Organization) (http.RoundTripper, error) { return sizedTransport{size: test.size}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })
			organization := Organization{ID: "org_limit", ProviderID: "porg_limit", Name: "Limit", Endpoint: testSocketEndpoint("org_limit.sock"), Generation: 1}
			status, body, err := service.machineCall(t.Context(), organization, http.MethodGet, "/api/v2/organizations/org_limit/entitlements", nil, "token")
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "exceeds") {
					t.Fatalf("machineCall() = %d, %d bytes, %v; want a size error", status, len(body), err)
				}
				return
			}
			if err != nil || status != http.StatusOK || len(body) != test.size {
				t.Fatalf("machineCall() = %d, %d bytes, %v", status, len(body), err)
			}
		})
	}
}
