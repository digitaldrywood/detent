package operatorcommands_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestHubMCPOperatorCredentialBoundary(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewDefaultNative(t, hubfixture.Config{})
	worker := hubtest.Credential(t, f.Hub.Repository, f.Organization(), hubtest.CredentialOptions{Scope: "worker", Now: f.Hub.Now()})
	path := hubfixture.MCPPath(f.Organization())
	for _, test := range []struct {
		name, token, target string
		status              int
	}{
		{"operator", f.Token, path, http.StatusOK},
		{"administrator", f.Hub.AdminToken(), path, http.StatusOK},
		{"worker cannot become operator", worker.Token, path, http.StatusForbidden},
		{"no credential", "", path, http.StatusUnauthorized},
		{"foreign organization", f.Token, hubfixture.MCPPath(hubfixture.Secondary), http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.Hub.Request(t, http.MethodPost, test.target, test.token, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}}})
			if response.Code != test.status {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
		})
	}
}
