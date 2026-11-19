package hubfixture

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
)

func (h *ConversationHostedEntry) BrowserThreadCall(t *testing.T, user HostedUser, role, name string, arguments any) *httptest.ResponseRecorder {
	t.Helper()
	params := map[string]any{"_meta": json.RawMessage(MCPProtocolMeta), "name": name, "arguments": arguments}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": params})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, MCPPath(h.Primary), bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Protocol-Version", MCPProtocolVersion)
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", name)
	assertionID, err := cloudassert.NewID()
	if err != nil {
		t.Fatal(err)
	}
	identity, entry, now := user.Identity.Hosted, h.Config.Hosted.SharedEntry, h.Now()
	audience, binding := string(h.Primary), "shared-"+identity.Subject
	csrf := cloudassert.CSRFToken(binding, audience)
	assertion, err := cloudassert.Sign(h.signer, cloudassert.Claims{
		Issuer: entry.Issuer, Audience: audience, Generation: entry.Generation, Kind: cloudassert.KindBrowser,
		Method: request.Method, Path: request.URL.RequestURI(), BodyDigest: cloudassert.BodyDigest(body),
		IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: assertionID,
		Subject: identity.Subject, Email: user.Identity.Email, ProviderOrganization: identity.OrganizationID, ProviderSession: identity.SessionID,
		SessionCreatedAt: identity.CreatedAt, SessionExpiresAt: identity.ExpiresAt, Role: role, AccessExpiresAt: now.Add(5 * time.Minute),
		Binding: cloudassert.AuthorizationBinding(binding, audience, identity.SessionID), CSRF: csrf,
	})
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(cloudassert.Header, assertion)
	request.Header.Set("X-CSRF-Token", csrf)
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, request)
	return response
}
