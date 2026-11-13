package tenantcall

import (
	"bytes"
	"crypto/ed25519"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type sizedTransport struct{ size int }

func (t sizedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), t.size)))}, nil
}

func TestMachineCallRefusesOversizedTenantAnswers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		path        string
		requestBody string
		size        int
		wantErr     bool
	}{
		{name: "at the limit", size: tenantCallResponseLimit},
		{name: "one byte over", size: tenantCallResponseLimit + 1, wantErr: true},
		{name: "export above ordinary transport limit", path: "/internal/v1/keys/call", requestBody: `{"name":"work_bulk_export"}`, size: mcp.MaxHTTPResponseBytes + 1},
		{name: "export at limit", path: "/internal/v1/keys/call", requestBody: `{"name":"work_bulk_export"}`, size: operatortool.WorkBulkExportMaxBytes},
		{name: "export over limit", path: "/internal/v1/keys/call", requestBody: `{"name":"work_bulk_export"}`, size: operatortool.WorkBulkExportMaxBytes + 1, wantErr: true},
		{name: "ordinary tool retains limit", path: "/internal/v1/keys/call", requestBody: `{"name":"work_list"}`, size: mcp.MaxHTTPResponseBytes + 1, wantErr: true},
		{name: "catalog retains limit", path: "/internal/v1/keys/catalog", size: mcp.MaxHTTPResponseBytes + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			seed := make([]byte, ed25519.SeedSize)
			service := New(Deps{Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(seed), Now: time.Now,
				Transport: func(tenant.Organization) (http.RoundTripper, error) { return sizedTransport{size: test.size}, nil }})
			organization := tenant.Organization{ID: "org_limit", ProviderID: "porg_limit", Name: "Limit", Generation: 1}
			path := test.path
			if path == "" {
				path = "/api/v2/organizations/org_limit/entitlements"
			}
			status, body, err := service.MachineCall(t.Context(), organization, http.MethodPost, path, []byte(test.requestBody), "token")
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
