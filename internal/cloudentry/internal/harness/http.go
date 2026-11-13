package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func AttachmentMCPClient(t *testing.T, browser *Browser, token, protocol string) func(string, any) (json.RawMessage, bool) {
	t.Helper()
	return MCPClient(t, browser, token, protocol, "/organizations/org_alpha/mcp")
}

func MCPClient(t *testing.T, browser *Browser, token, protocol, path string) func(string, any) (json.RawMessage, bool) {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token, "Mcp-Protocol-Version": protocol}
	if protocol != "2026-07-28" {
		initialized := Request(t, browser, http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+protocol+`","capabilities":{},"clientInfo":{"name":"attachment-operations","version":"1"}}}`), headers)
		if initialized.Code != http.StatusOK {
			t.Fatalf("initialize=%d %s", initialized.Code, initialized.Body.String())
		}
		headers["Mcp-Session-Id"] = initialized.Header().Get("Mcp-Session-Id")
		Request(t, browser, http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), headers)
	}

	return func(name string, arguments any) (json.RawMessage, bool) {
		t.Helper()
		method, params := "tools/call", map[string]any{"name": name, "arguments": arguments}
		if name == "tools/list" {
			method, params = name, map[string]any{}
		}
		if protocol == "2026-07-28" {
			params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": protocol, "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]string{"name": "attachment-operations", "version": "1"}}
			headers["Mcp-Method"] = method
			headers["Mcp-Name"] = name
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		response := Request(t, browser, http.MethodPost, path, bytes.NewReader(raw), headers)
		var frame struct {
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &frame) != nil || len(frame.Error) != 0 {
			return response.Body.Bytes(), true
		}
		if method == "tools/list" {
			return frame.Result, false
		}
		var result struct {
			ResultType string                     `json:"resultType"`
			Meta       map[string]json.RawMessage `json:"_meta"`
			IsError    bool                       `json:"isError"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured json.RawMessage `json:"structuredContent"`
		}
		if json.Unmarshal(frame.Result, &result) != nil || len(result.Content) != 1 {
			t.Fatalf("tool response=%s", response.Body.String())
		}
		if protocol == mcp.ProtocolVersion && (result.ResultType != "complete" || len(result.Meta["io.modelcontextprotocol/serverInfo"]) == 0) {
			t.Fatalf("modern tool result missing protocol envelope=%s", frame.Result)
		}
		if protocol != mcp.ProtocolVersion && (result.ResultType != "" || len(result.Meta) != 0) {
			t.Fatalf("legacy tool result returned modern protocol envelope=%s", frame.Result)
		}
		if protocol == "2024-11-05" && len(result.Structured) != 0 {
			t.Fatal("legacy MCP returned structured content")
		}
		if len(result.Content[0].Text) > operatortool.MaxResultBytes {
			t.Fatal("tool result exceeded its bound")
		}
		if strings.Contains(result.Content[0].Text, "authorized_principal") || strings.Contains(result.Content[0].Text, "spaces.test") || strings.Contains(result.Content[0].Text, "private/") {
			t.Fatal("tool result exposed internal attachment authority or storage")
		}
		if len(result.Structured) != 0 {
			var structured, textual any
			if json.Unmarshal(result.Structured, &structured) != nil || json.Unmarshal([]byte(result.Content[0].Text), &textual) != nil || !reflect.DeepEqual(structured, textual) {
				t.Fatal("structured result differs from text")
			}
		}
		return json.RawMessage(result.Content[0].Text), result.IsError
	}
}

func Project(t *testing.T, browser *Browser, org string) string {
	t.Helper()
	response, body := browser.Get("/api/v2/organizations/" + org + "/projects")
	var result []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil || response.StatusCode != http.StatusOK || len(result) == 0 {
		t.Fatalf("projects=%d %s", response.StatusCode, body)
	}
	return result[0].ID
}

func Request(t *testing.T, b *Browser, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, PublicURL+target, body)
	if method != http.MethodGet {
		r.Header.Set("Origin", PublicURL)
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	base, _ := url.Parse(PublicURL)
	for _, cookie := range b.Jar.Cookies(base) {
		r.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	b.Entry.ServeHTTP(recorder, r)
	return recorder
}

func TenantTransport(t *testing.T, handler http.Handler) (http.RoundTripper, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	address := server.Listener.Addr().String()
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, network, address)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	t.Cleanup(server.Close)
	return transport, server.Close
}

type SwappableHandler struct{ Handler atomic.Pointer[http.Handler] }

func (h *SwappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*h.Handler.Load()).ServeHTTP(w, r)
}

type KeyOrganization struct {
	apikey.ProjectContext
	OrganizationID string `json:"organization_id"`
}

func CreateAccessKey(t *testing.T, b *Browser, context string, organizations any, serviceOrg string, key any, permissions ...string) {
	t.Helper()
	path := "/api/cloud/account/api-keys"
	if serviceOrg != "" {
		path = "/api/cloud/organizations/" + serviceOrg + "/service-keys"
	}
	permission := "write"
	if len(permissions) > 0 {
		permission = permissions[0]
	}
	input := map[string]any{"name": "Multi-org test", "permission": permission, "access_context": context, "organizations": organizations, "expires_days": 7}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	response := Request(t, b, http.MethodPost, path, bytes.NewReader(raw), map[string]string{"Content-Type": "application/json", "Origin": PublicURL})
	if response.Code != http.StatusCreated {
		t.Fatalf("create key=%d %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), key); err != nil {
		t.Fatal(err)
	}
	var token struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	if token.Token == "" {
		t.Fatal("created key has no token")
	}
}
