package conversation_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const fleetProtocolMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"fleet-test","version":"1"}}`

func TestPostgresOperatorChatSessions(t *testing.T) {
	h := openHub(t, hubserver.Config{})
	first := hubtest.Organization(t, h.repository, hubtest.OrganizationOptions{}).OrganizationID
	second := hubtest.Organization(t, h.repository, hubtest.OrganizationOptions{}).OrganizationID
	type tenant struct {
		project tracker.ProjectID
		token   string
	}
	tenants := map[tracker.OrganizationID]tenant{}
	for _, org := range []tracker.OrganizationID{first, second} {
		project := hubtest.Project(t, h.repository, org, hubtest.ProjectOptions{Name: string(org)})
		credential := hubtest.Credential(t, h.repository, org, hubtest.CredentialOptions{Scope: "admin", KeyScope: "admin", AllProject: true})
		tenants[org] = tenant{project: project.ID, token: credential.Token}
	}
	for pass := 0; pass < 2; pass++ {
		if pass > 0 {
			h = h.reopen(t, hubserver.Config{})
		}
		for _, org := range []tracker.OrganizationID{first, second} {
			call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "get_project_integration", "arguments": map[string]any{"project_id": string(tenants[org].project)}, "_meta": json.RawMessage(fleetProtocolMeta)}}
			path := "/api/v2/organizations/" + string(org) + "/mcp"
			response := h.mcpCall(t, path, tenants[org].token, "get_project_integration", call)
			requireStatus(t, response, http.StatusOK)
			var envelope struct {
				Result struct {
					IsError bool `json:"isError"`
					Data    struct {
						Organization string                   `json:"organization_id"`
						Integration  projectstore.Integration `json:"data"`
					} `json:"structuredContent"`
				} `json:"result"`
			}
			decodeResponse(t, response, &envelope)
			if envelope.Result.IsError || envelope.Result.Data.Organization != string(org) || envelope.Result.Data.Integration.Name != string(org) {
				t.Fatalf("MCP=%s", response.Body.String())
			}
			var count int
			if err := h.admin.QueryRowContext(t.Context(), `SELECT count(*) FROM operator_chat_sessions WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 1 {
				t.Fatalf("persisted sessions=%d err=%v", count, err)
			}
			foreign := first
			if org == foreign {
				foreign = second
			}
			requireStatus(t, h.mcpCall(t, path, tenants[foreign].token, "get_project_integration", call), http.StatusUnauthorized)
		}
	}
}
