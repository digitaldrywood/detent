package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

// This fake is an application command, not a protocol dispatcher. Both
// transports must preserve its typed selector and current authority checks.
type protocolApplication struct {
	denied      atomic.Bool
	writeDenied atomic.Bool
}

func (a *protocolApplication) OpenConnection(ctx context.Context) error {
	_, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead})
	return err
}
func (a *protocolApplication) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeRead}); err != nil {
		return nil, err
	}
	tools := operatortool.Catalog()
	for _, d := range operatortool.CommandCatalog() {
		if d.Annotations.ReadOnly {
			tools = append(tools, d)
		} else if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite}); err == nil {
			tools = append(tools, d)
		}
	}
	return tools, nil
}
func (a *protocolApplication) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	var args struct {
		ProjectID  string `json:"project_id"`
		RequestID  string `json:"request_id"`
		Identifier string `json:"identifier"`
		Priority   string `json:"priority"`
	}
	if call.Name != operatortool.SetPriority || operatortool.DecodeArguments(call.Arguments, &args) != nil || args.RequestID == "" || args.Identifier != "issue-3339" || args.Priority != "high" {
		return operatortool.Result{}, operatortool.ErrInvalidArguments
	}
	if _, err := operatortool.AuthorizeCurrent(ctx, operatortool.Requirement{Scope: apikey.ScopeWrite, ProjectID: args.ProjectID, ResourceKind: "issue", ResourceID: args.Identifier}); err != nil {
		return operatortool.Result{}, err
	}
	return operatortool.Result{Content: json.RawMessage(`{"organization_id":"org","project_id":"project","resource_id":"issue-3339","url":"https://detent.example/issues/3339","status":"executed"}`)}, nil
}
func (a *protocolApplication) context(ctx context.Context, identity operatortool.Identity) context.Context {
	return operatortool.WithConnection(ctx, operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
		return operatortool.Authority{Identity: identity, Check: func(_ context.Context, r operatortool.Requirement) error {
			if a.denied.Load() || a.writeDenied.Load() && r.Scope == apikey.ScopeWrite || identity.PrincipalID != "operator" || identity.OrganizationID != "org" || identity.CredentialID != "credential" || r.ProjectID != "" && r.ProjectID != "project" || r.ResourceID != "" && r.ResourceID != "issue-3339" {
				return operatortool.ErrAccessDenied
			}
			return nil
		}}, nil
	}})
}

type protocolFixture struct {
	request     func(string, map[string]any) rpcResponse
	application *protocolApplication
	identity    *operatortool.Identity
	modern      bool
}

func newProtocolFixture(t *testing.T, transport, version string) protocolFixture {
	t.Helper()
	app := &protocolApplication{}
	identity := &operatortool.Identity{PrincipalID: "operator", OrganizationID: "org", CredentialID: "credential"}
	modern := version == ProtocolVersion
	var send func(string) rpcResponse
	if transport == "stdio" {
		client := startLiveServerContext(t, app, app.context(context.WithoutCancel(t.Context()), *identity))
		t.Cleanup(client.close)
		send = func(frame string) rpcResponse { client.write(frame); return client.read() }
		if !modern {
			send(strings.Replace(initializeRequest, LegacyProtocolVersion, version, 1))
			client.write(initializedNotice)
		}
	} else {
		handler := NewHTTPHandler(app, "test-version", HTTPConfig{Principal: func(req *http.Request) operatortool.Identity { return operatortool.ConnectionIdentity(req.Context()) }})
		t.Cleanup(func() {
			if err := handler.Shutdown(context.Background()); err != nil {
				t.Error(err)
			}
		})
		sessionID := ""
		send = func(frame string) rpcResponse {
			var message request
			if err := json.Unmarshal([]byte(frame), &message); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "http://detent.example/mcp", strings.NewReader(frame))
			req = req.WithContext(app.context(req.Context(), *identity))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if modern {
				req.Header.Set(httpProtocolHeader, version)
				req.Header.Set("Mcp-Method", message.Method)
				var params struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(message.Params, &params)
				if params.Name != "" {
					req.Header.Set("Mcp-Name", params.Name)
				}
			} else if sessionID != "" {
				req.Header.Set(httpSessionHeader, sessionID)
				req.Header.Set(httpProtocolHeader, version)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if modern && recorder.Header().Get(httpSessionHeader) != "" {
				t.Fatal("modern response minted protocol session")
			}
			if message.Method == "initialize" {
				sessionID = recorder.Header().Get(httpSessionHeader)
			}
			if len(message.ID) == 0 {
				return rpcResponse{}
			}
			var response rpcResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			return response
		}
		if !modern {
			send(strings.Replace(initializeRequest, LegacyProtocolVersion, version, 1))
			send(initializedNotice)
		}
	}
	id := 0
	return protocolFixture{application: app, identity: identity, modern: modern, request: func(method string, params map[string]any) rpcResponse {
		id++
		if params == nil {
			params = map[string]any{}
		}
		if modern {
			params["_meta"] = map[string]any{protocolMetaKey: version, capabilitiesMetaKey: map[string]any{}, clientMetaKey: map[string]string{"name": "portable-client", "version": "1"}, "detent/context": map[string]string{"principal_id": "admin", "organization_id": "other"}, "detent/mode": "yolo", "detent/session": "forged"}
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		return send(string(raw))
	}}
}

func TestProtocolApplicationParity(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"stdio", "http"} {
		for _, version := range supportedVersions() {
			t.Run(transport+"/"+version, func(t *testing.T) {
				t.Parallel()
				fixture := newProtocolFixture(t, transport, version)
				if fixture.modern {
					var discover struct {
						ResultType string   `json:"resultType"`
						Supported  []string `json:"supportedVersions"`
					}
					decodeResult(t, fixture.request("server/discover", nil), &discover)
					if discover.ResultType != "complete" || !reflect.DeepEqual(discover.Supported, supportedVersions()) {
						t.Fatalf("discovery: %+v", discover)
					}
				}
				// Direct invocation before catalog discovery must still check the application.
				params := map[string]any{"name": operatortool.SetPriority, "arguments": map[string]string{"project_id": "other", "request_id": "request-1", "identifier": "issue-3339", "priority": "high"}}
				var denied toolCallResult
				decodeResult(t, fixture.request("tools/call", params), &denied)
				if !denied.IsError || denied.Content[0].Text != operatortool.ErrAccessDenied.Error() {
					t.Fatalf("direct selector bypass: %+v", denied)
				}
				cursor := ""
				var tools []operatortool.Definition
				pages := 0
				for {
					var page catalogPage
					decodeResult(t, fixture.request("tools/list", map[string]any{"cursor": cursor}), &page)
					tools = append(tools, page.Tools...)
					pages++
					if len(page.Tools) > catalogPageSize {
						t.Fatal("unbounded page")
					}
					cursor = page.NextCursor
					if cursor == "" {
						break
					}
					if pages > (len(operatortool.Catalog())+len(operatortool.CommandCatalog())+catalogPageSize-1)/catalogPageSize {
						t.Fatal("pagination did not finish")
					}
				}
				want := append(operatortool.Catalog(), operatortool.CommandCatalog()...)
				if !reflect.DeepEqual(tools, want) || pages != (len(want)+catalogPageSize-1)/catalogPageSize {
					t.Fatalf("typed catalog/schema/annotation parity mismatch: pages=%d tools=%+v", pages, tools)
				}
				for _, tool := range tools {
					if tool.Meta.Toolset == "" {
						t.Fatalf("ungrouped %s", tool.Name)
					}
				}
				params["arguments"].(map[string]string)["project_id"] = "project"
				response := fixture.request("tools/call", params)
				var result toolCallResult
				decodeResult(t, response, &result)
				if result.IsError || len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, `"resource_id":"issue-3339"`) {
					t.Fatalf("typed command result: %+v", result)
				}
				if version != "2024-11-05" && version != "2025-03-26" && !jsonEqual(result.StructuredContent, json.RawMessage(result.Content[0].Text)) {
					t.Fatal("structured/text results diverged")
				}
				if fixture.modern && !bytes.Contains(response.Result, []byte(`"resultType":"complete"`)) {
					t.Fatal("missing complete discriminator")
				}
				fixture.application.denied.Store(true)
				decodeResult(t, fixture.request("tools/call", params), &denied)
				if !denied.IsError || denied.Content[0].Text != operatortool.ErrAccessDenied.Error() {
					t.Fatalf("revoked authority reused: %+v", denied)
				}
			})
		}
	}
}

func TestModernHTTPMetadataValidation(t *testing.T) {
	t.Parallel()
	good := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"set_priority","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	tests := []struct {
		name, body   string
		headers      map[string]string
		code, status int
		publicOrigin string
	}{
		{"missing metadata", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil, codeInvalidParams, 400, ""},
		{"missing capability", strings.Replace(good, `,"io.modelcontextprotocol/clientCapabilities":{}`, "", 1), nil, codeInvalidParams, 400, ""},
		{"unsupported version", strings.Replace(good, ProtocolVersion, "2099-01-01", 1), nil, codeUnsupportedVersion, 400, ""},
		{"method mismatch", good, map[string]string{"Mcp-Method": "tools/list"}, codeHeaderMismatch, 400, ""},
		{"missing name", good, map[string]string{"Mcp-Name": ""}, codeHeaderMismatch, 400, ""},
		{"version mismatch", good, map[string]string{httpProtocolHeader: LegacyProtocolVersion}, codeHeaderMismatch, 400, ""},
		{"malformed encoded name", good, map[string]string{"Mcp-Name": "=?base64?invalid?="}, codeHeaderMismatch, 400, ""},
		{"encoded name", good, map[string]string{"Mcp-Name": "=?base64?c2V0X3ByaW9yaXR5?="}, 0, 200, ""},
		{"unknown method", strings.Replace(good, `"method":"tools/call"`, `"method":"unknown"`, 1), map[string]string{"Mcp-Method": "unknown"}, codeMethodNotFound, 404, ""},
		{"invalid origin scheme", good, map[string]string{"Origin": "ftp://detent.example"}, -32000, 403, ""},
		{"trusted TLS proxy", good, map[string]string{"Origin": "https://detent.example"}, 0, 200, "https://detent.example"},
		{"forwarding header cannot select origin", good, map[string]string{"Origin": "https://detent.example", "X-Forwarded-Proto": "https"}, -32000, 403, ""},
		{"origin userinfo", good, map[string]string{"Origin": "http://user@detent.example"}, -32000, 403, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := NewHTTPHandler(&staticExecutor{result: operatortool.Result{Content: json.RawMessage(`{"ok":true}`)}}, "test", HTTPConfig{Principal: func(*http.Request) operatortool.Identity {
				return operatortool.Identity{PrincipalID: "p", OrganizationID: "o", CredentialID: "c"}
			}})
			headers := map[string]string{httpProtocolHeader: ProtocolVersion, "Mcp-Method": "tools/call", "Mcp-Name": "set_priority", httpSessionHeader: "forged-session"}
			for key, value := range test.headers {
				headers[key] = value
			}
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if test.publicOrigin != "" {
					r = r.WithContext(operatortool.WithConnection(r.Context(), operatortool.Connection{DashboardURL: test.publicOrigin}))
				}
				h.ServeHTTP(w, r)
			})
			r := performMCPRequest(t, handler, http.MethodPost, test.body, "", "", headers)
			var response rpcResponse
			if err := json.Unmarshal(r.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			code := 0
			if response.Error != nil {
				code = response.Error.Code
			}
			if r.Code != test.status || code != test.code || r.Body.Len() > MaxHTTPResponseBytes {
				t.Fatalf("%d %s; want status=%d code=%d", r.Code, r.Body.String(), test.status, test.code)
			}
			if response.Error != nil && strings.Contains(response.Error.Message, "forged") {
				t.Fatal("unsafe error")
			}
		})
	}
}

func TestCatalogCursorCurrentAuthority(t *testing.T) {
	t.Parallel()
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			t.Parallel()
			fixture := newProtocolFixture(t, transport, ProtocolVersion)
			var first catalogPage
			decodeResult(t, fixture.request("tools/list", nil), &first)
			if first.NextCursor == "" {
				t.Fatal("missing next page")
			}
			for _, cursor := range []string{"invalid", strings.Repeat("a", 257)} {
				response := fixture.request("tools/list", map[string]any{"cursor": cursor})
				if response.Error == nil || response.Error.Code != codeInvalidParams {
					t.Fatalf("invalid cursor: %+v", response)
				}
			}
			fixture.application.writeDenied.Store(true)
			changed := fixture.request("tools/list", map[string]any{"cursor": first.NextCursor})
			if changed.Error == nil || changed.Error.Code != codeInvalidParams {
				t.Fatalf("changed permissions reused cursor: %+v", changed)
			}
			var readPage catalogPage
			decodeResult(t, fixture.request("tools/list", nil), &readPage)
			for _, tool := range readPage.Tools {
				if !tool.Annotations.ReadOnly {
					t.Fatal("write scope escaped filtering")
				}
			}
			fixture.application.writeDenied.Store(false)
			fixture.application.denied.Store(true)
			response := fixture.request("tools/list", map[string]any{"cursor": first.NextCursor})
			if response.Error == nil || response.Error.Message != operatortool.ErrAccessDenied.Error() {
				t.Fatalf("stale cursor widened authority: %+v", response)
			}
			if transport == "http" {
				fixture.application.denied.Store(false)
				fixture.identity.PrincipalID = "other"
				response = fixture.request("tools/list", map[string]any{"cursor": first.NextCursor})
				if response.Error == nil {
					t.Fatal("cross-principal cursor widened authority")
				}
			}
		})
	}
}

// Catalog discovery is synchronous, whereas calls execute asynchronously. Both
// must see request disconnect and handler shutdown, without a protocol session.
type blockingCatalogApplication struct{ *blockingExecutor }

func (a blockingCatalogApplication) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	_, err := a.Execute(ctx, operatortool.Call{})
	return nil, err
}

func TestModernHTTPRequestCancellation(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"tools/list", "tools/call"} {
		for _, end := range []string{"disconnect", "shutdown"} {
			t.Run(method+"/"+end, func(t *testing.T) {
				t.Parallel()
				blocker := newBlockingExecutor()
				h := NewHTTPHandler(blockingCatalogApplication{blocker}, "test", HTTPConfig{Principal: func(*http.Request) operatortool.Identity {
					return operatortool.Identity{PrincipalID: "p", OrganizationID: "o", CredentialID: "c"}
				}})
				params := map[string]any{"_meta": map[string]any{protocolMetaKey: ProtocolVersion, capabilitiesMetaKey: map[string]any{}}}
				if method == "tools/call" {
					params["name"] = operatortool.FleetHealth
					params["arguments"] = map[string]any{}
				}
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				req := httptest.NewRequest(http.MethodPost, "http://detent.example/mcp", bytes.NewReader(raw)).WithContext(ctx)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set(httpProtocolHeader, ProtocolVersion)
				req.Header.Set("Mcp-Method", method)
				req.Header.Set("Mcp-Name", operatortool.FleetHealth)
				done := make(chan struct{})
				go func() { h.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
				blocker.waitStarted(t)
				if end == "disconnect" {
					cancel()
				} else {
					if err := h.Shutdown(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				blocker.waitCancelled(t)
				waitChannel(t, done, "modern HTTP completion")
			})
		}
	}
}
