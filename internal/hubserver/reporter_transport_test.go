package hubserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func reporterAdministrationArguments(t *testing.T, definition operatortool.Definition) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(definition.InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	var value func(string, map[string]any) any
	value = func(name string, property map[string]any) any {
		if enum, ok := property["enum"].([]any); ok && len(enum) > 0 {
			return enum[0]
		}
		switch property["type"] {
		case "boolean":
			return false
		case "integer":
			if min, ok := property["minimum"].(float64); ok {
				return int(min)
			}
			return 1
		case "array":
			count := 0
			if min, ok := property["minItems"].(float64); ok {
				count = int(min)
			}
			items, _ := property["items"].(map[string]any)
			result := []any{}
			for range count {
				result = append(result, value(name, items))
			}
			return result
		case "object":
			result := map[string]any{}
			properties, _ := property["properties"].(map[string]any)
			required, _ := property["required"].([]any)
			for _, raw := range required {
				field := raw.(string)
				spec, _ := properties[field].(map[string]any)
				result[field] = value(field, spec)
			}
			return result
		default:
			if name == "email" {
				return "tester@example.test"
			}
			if strings.Contains(name, "revision") {
				return "1"
			}
			return "example"
		}
	}
	arguments := value("", schema).(map[string]any)
	if _, found := arguments["request_id"]; found {
		arguments["request_id"] = definition.Name
	}
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operatoradmin.Decode(definition.Name, raw); err != nil {
		t.Fatalf("%s fixture arguments invalid: %v", definition.Name, err)
	}
	return arguments
}

func reporterEntryRequest(t *testing.T, f hostedKeyMCPFixture, deployment, token, transport, method, path string, raw []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	if deployment == "shared" {
		shared := hostedSharedFixture{hostedSecurityFixture: f.hostedSecurityFixture, key: hostedSharedKey(7)}
		request := hostedSharedRequest{method: method, target: path, body: string(raw), headers: headers}
		if transport == "web-session" {
			request.user = &f.user
			request.csrf = cloudassert.CSRFToken("shared-"+f.user.identity.Subject, "org_security")
		} else {
			request.kind, request.bearer = cloudassert.KindMachine, token
		}
		return shared.serve(t, request)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	if transport == "web-session" {
		request.AddCookie(&http.Cookie{Name: hostedCookie, Value: f.user.token})
		request.Header.Set("X-CSRF-Token", hostedCSRF(f.user.token))
	} else {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func TestReporterWriteRefusalsThroughEntryPoints(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		t.Run(deployment, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, deployment, "reporter")
			catalog, err := operatorCatalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			key, err := f.service.createHostedAPIKeyFor(ctx, catalog.credential, hostedKeyRequest{Name: "Reporter API key", Scope: apikey.ScopeWrite, Days: 1, Projects: []string{string(f.project)}})
			if err != nil {
				t.Fatal(err)
			}
			send := func(t *testing.T, transport, method, path string, raw []byte, headers map[string]string) *httptest.ResponseRecorder {
				return reporterEntryRequest(t, f, deployment, key.Token, transport, method, path, raw, headers)
			}

			owner := f
			owner.user = f.hostedSecurityFixture.user(t, "owner", "owner", "owner@example.test", "write", "")
			item := string(f.seedIssue(t, 1))
			cases := []struct {
				name      string
				arguments map[string]any
			}{
				{operatortool.MoveItem, map[string]any{"identifier": item, "target_state": "Todo", "expected_revision": "1"}},
				{operatortool.EditItem, map[string]any{"identifier": item, "title": "Replacement", "expected_revision": "1"}},
				{operatortool.SetDependency, map[string]any{"identifier": item, "related": "wi_related", "operation": "add", "expected_revision": "1"}},
				{operatortool.SetQueuePriority, map[string]any{"identifier": item, "queue_scope": "project", "state": "Todo", "priority": 1}},
				{operatortool.OrderItem, map[string]any{"identifier": item, "queue_scope": "project", "state": "Todo", "rank": "1"}},
				{operatortool.TransferItem, map[string]any{"identifier": item, "expected_revision": "1", "version_id": "version_example", "destination_runner_id": "runner_example"}},
				{operatortool.RestoreItem, map[string]any{"identifier": item, "expected_revision": "1"}},
				{operatortool.DeleteAttachment, map[string]any{"attachment_id": "att_00000000000000000000000000000001"}},
			}
			for _, definition := range operatortool.AdministrationCatalog() {
				cases = append(cases, struct {
					name      string
					arguments map[string]any
				}{definition.Name, reporterAdministrationArguments(t, definition)})
			}
			for _, transport := range []string{"web-session", "MCP-key"} {
				for _, test := range cases {
					t.Run(transport+"/"+test.name, func(t *testing.T) {
						arguments := map[string]any{}
						for name, value := range test.arguments {
							arguments[name] = value
						}
						if !operatortool.IsAdministration(test.name) {
							arguments["project_id"], arguments["request_id"] = string(f.project), transport+test.name
						}
						raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": test.name, "arguments": arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]string{"name": "Reporter test", "version": "1"}}}})
						if err != nil {
							t.Fatal(err)
						}
						response := send(t, transport, http.MethodPost, "/api/v2/organizations/org_security/mcp", raw, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": test.name})

						var result struct {
							Result struct {
								IsError bool `json:"isError"`
							} `json:"result"`
						}
						if response.Code == http.StatusOK {
							if json.Unmarshal(response.Body.Bytes(), &result) != nil || !result.Result.IsError {
								t.Fatalf("write allowed: %s", response.Body.String())
							}
						} else if response.Code != http.StatusForbidden {
							t.Fatalf("unexpected refusal=%d %s", response.Code, response.Body.String())
						}
						if !strings.Contains(response.Body.String(), operatortool.ErrAccessDenied.Error()) {
							t.Fatalf("missing permission refusal: %s", response.Body.String())
						}

					})
				}
			}
			for _, transport := range []string{"web-session", "REST-key"} {
				for _, test := range []struct {
					name, method, path string
					body               any
				}{
					{operatortool.EditItem, http.MethodPatch, f.base + "/work-items/" + item, map[string]any{"idempotency_key": "edit", "expected_revision": "1", "title": "Replacement"}},
					{operatortool.SetDependency, http.MethodPost, f.base + "/work-items/" + item + "/dependencies", map[string]any{"idempotency_key": "dependency", "expected_revision": "1", "related_work_item_id": "wi_related", "operation": "add"}},
					{operatortool.RestoreItem, http.MethodPost, f.base + "/work-items/" + item + "/restore", map[string]any{"idempotency_key": "restore", "expected_revision": "1"}},
					{operatortool.TransferItem, http.MethodPost, f.base + "/work-items/" + item + "/source-recovery", map[string]any{"idempotency_key": "transfer", "expected_revision": "1", "version_id": "version_example", "destination_runner_id": "runner_example"}},
					{operatortool.MoveItem, http.MethodPost, f.base + "/work-items/" + item + "/workflow", map[string]any{"idempotency_key": "move", "expected_revision": "1", "state": "Todo"}},
					{operatortool.DeleteAttachment, http.MethodDelete, f.base + "/attachment-metadata/att_00000000000000000000000000000001", nil},
					{operatortool.SetQueuePriority, http.MethodPost, "/api/v1/work-items/1/priority", map[string]any{"idempotency_key": "priority", "scope": f.project, "state": "Todo", "priority": 1}},
					{operatortool.OrderItem, http.MethodPost, "/api/v1/work-items/1/order", map[string]any{"idempotency_key": "order", "scope": f.project, "state": "Todo", "rank": "1"}},
					{operatortool.InvitationSend, http.MethodPost, "/api/v2/organizations/org_security/members/invitations", map[string]any{"idempotency_key": "invite", "email": "tester@example.test", "role": "member"}},
					{operatortool.MemberRole, http.MethodPut, "/api/v2/organizations/org_security/members/example/role", map[string]any{"idempotency_key": "role", "role": "member"}},
					{operatortool.MemberGrant, http.MethodPut, "/api/v2/organizations/org_security/members/example/grants", map[string]any{"idempotency_key": "grant", "project_id": f.project, "write": true}},
					{operatortool.InvitationEdit, http.MethodPut, "/api/v2/organizations/org_security/members/invitations/example", map[string]any{"idempotency_key": "invite-edit", "grants": []any{}}},
					{operatortool.InvitationRevoke, http.MethodDelete, "/api/v2/organizations/org_security/members/invitations/example", nil},
					{operatortool.InvitationResend, http.MethodPost, "/api/v2/organizations/org_security/members/invitations/example/resend", map[string]any{"idempotency_key": "invite-resend"}},
					{operatortool.MemberRemove, http.MethodDelete, "/api/v2/organizations/org_security/members/example", nil},
					{operatortool.OrganizationProjectRankUpdate, http.MethodPut, "/api/v2/organizations/org_security/project-rank", map[string]any{"idempotency_key": "rank", "expected_revision": "1", "project_ids": []string{string(f.project)}}},
					{operatortool.CredentialCreate, http.MethodPost, "/api/v1/tokens", map[string]any{"name": "Example", "scope": "admin"}},
					{operatortool.CredentialRotate, http.MethodPost, "/api/v1/tokens/example/rotate", nil},
					{operatortool.CredentialGrant, http.MethodPost, "/api/v2/tokens/example/grants", map[string]any{"organization_id": "org_security", "project_id": f.project}},
					{operatortool.CredentialRevoke, http.MethodDelete, "/api/v1/tokens/example", nil},
					{operatortool.OrganizationCreate, http.MethodPost, "/api/v2/organizations", map[string]any{"name": "Example organization"}},
					{operatortool.OrganizationSwitch, http.MethodPost, "/api/v2/organizations/org_security/switch", map[string]any{"organization_id": "org_security"}},
					{operatortool.InvitationAccept, http.MethodPost, "/api/v2/organizations/org_security/invitations/accept", map[string]any{"invitation_id": "example"}},
					{operatortool.SupportStart, http.MethodPost, "/api/v2/organizations/org_security/support/start", map[string]any{"organization_id": "org_security", "reason": "Example support request"}},
				} {
					t.Run(transport+"/REST/"+test.name, func(t *testing.T) {
						raw, err := json.Marshal(test.body)
						if err != nil {
							t.Fatal(err)
						}
						response := send(t, transport, test.method, test.path, raw, nil)

						wantStatus := http.StatusForbidden
						if strings.HasPrefix(test.path, "/api/v1/") || deployment == "dedicated" && test.name == operatortool.DeleteAttachment || deployment == "shared" && (test.name == operatortool.CredentialGrant || test.name == operatortool.OrganizationCreate) {
							wantStatus = http.StatusNotFound
						}
						if wantStatus == http.StatusNotFound {
							control := reporterEntryRequest(t, owner, deployment, "", "web-session", test.method, test.path, raw, nil)
							var unavailable apiErrorResponse
							if control.Code != http.StatusNotFound || json.Unmarshal(control.Body.Bytes(), &unavailable) != nil || unavailable.Code != "not_found" {
								t.Fatalf("route is not uniformly unavailable to owner: %d %s", control.Code, control.Body.String())
							}
						}
						if response.Code != wantStatus {
							t.Fatalf("REST write %s=%d %s", test.name, response.Code, response.Body.String())
						}
						var failure apiErrorResponse
						wantCode := "access_denied"
						if wantStatus == http.StatusNotFound {
							wantCode = "not_found"
						}
						if json.Unmarshal(response.Body.Bytes(), &failure) != nil || failure.Code != wantCode || wantStatus == http.StatusForbidden && failure.Message != operatortool.ErrAccessDenied.Error() {
							t.Fatalf("unexpected refusal for %s: %s", test.name, response.Body.String())
						}
					})
				}
			}
			var title string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT title FROM issues WHERE native_id=?", item).Scan(&title); err != nil {
				t.Fatal(err)
			}
			if title != "private-issue-sentinel" {
				t.Fatalf("denied call changed issue: %s", title)
			}

		})
	}
}
