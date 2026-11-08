package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresMissingOperatorCatalog(t *testing.T) {
	permission := apikey.ScopeWrite
	access := apikey.ProjectContext{Access: "selected", Projects: []string{"project", "compat"}}
	authority := func(id, binding string) *apikey.KeyAuthority {
		return &apikey.KeyAuthority{ID: id, CredentialBinding: binding, Kind: "personal", Permission: permission, ProjectContext: access}
	}
	f := newSharedRouteFixture(t, func(cfg *Config) {
		plans := pilotHostedPlans()
		cfg.Hosted.Plans = &plans
		org := cfg.Hosted.OrganizationID
		cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: org, ProviderOrganization: "provider_" + org, Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: authority(origin.KeyID, origin.CredentialBinding)}, nil
		}
	})
	for _, org := range []string{"org_startup", "org_other"} {
		service := f.services[org]
		pgmcpcatSeed(t, service, tracker.OrganizationID(org))
		lease := seedEntryRuntimeAttempt(t, service, tracker.OrganizationID(org), "wi_item")
		if err := service.refreshBoardRecord(t.Context(), tracker.OrganizationID(org), viewstore.BoardCandidate{ProjectID: "project", WorkItemID: "wi_item"}, service.config.now()); err != nil {
			t.Fatal(err)
		}
		claims := func(c *cloudassert.Claims) {
			c.Kind, c.ProviderOrganization = cloudassert.KindKey, "provider_"+org
			c.Key = authority("key_"+org, strings.Repeat("c", 64))
		}
		call := func(t *testing.T, name, arguments string, status int) string {
			t.Helper()
			body := fmt.Sprintf(`{"name":%q,"arguments":{%s},"connection_id":"pgmcpcat"}`, name, arguments)
			response := f.request(t, org, "/internal/v1/keys/call", body, claims)
			requireNativeStatus(t, response, status)
			return response.Body.String()
		}
		for _, test := range []struct{ name, project, arguments, want string }{
			{operatortool.BoardState, "project", `"limit":2`, `"project"`},
			{operatortool.Dashboard, "project", `"limit":2`, `"project"`},
			{operatortool.OrderItem, "compat", `"identifier":"#1","queue_scope":"compat","state":"Todo","rank":"001","request_id":"order"`, `"applied"`},
			{operatortool.SetQueuePriority, "compat", `"identifier":"#1","queue_scope":"compat","state":"Todo","queue_priority":"high","request_id":"priority"`, `"applied"`},
			{operatortool.VerifyFix, "project", `"identifier":"#2","expected_revision":"1","result":"verified","request_id":"verify"`, `"verified"`},
			{operatortool.VerifyFix, "project", `"identifier":"#3","expected_revision":"1","result":"not_fixed","comment":"Example retained verification evidence","request_id":"reopen"`, `"Todo"`},
			{operatortool.StopRun, "project", fmt.Sprintf(`"identifier":"#1","expected_revision":"1","lease_id":%q,"fencing_token":%d,"destination":"Todo","priority":1,"request_id":"stop"`, lease.LeaseID, lease.FencingToken), `"Todo"`},
		} {
			t.Run(org+"/"+test.name, func(t *testing.T) {
				catalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, claims)
				requireNativeStatus(t, catalog, http.StatusOK)
				var definitions []operatortool.Definition
				decodeHubResponse(t, catalog, &definitions)
				if !entryKeyHasDefinition(definitions, test.name) {
					t.Errorf("tools/list omitted %s", test.name)
				}
				arguments := fmt.Sprintf(`"project_id":%q,%s`, test.project, test.arguments)
				if test.name == operatortool.OrderItem || test.name == operatortool.SetQueuePriority {
					call(t, test.name, strings.Replace(arguments, `"queue_scope":"compat"`, `"queue_scope":"organization"`, 1), http.StatusForbidden)
				}
				if test.name == operatortool.StopRun {
					call(t, test.name, strings.Replace(strings.Replace(arguments, fmt.Sprintf(`"fencing_token":%d`, lease.FencingToken), fmt.Sprintf(`"fencing_token":%d`, lease.FencingToken+1), 1), `"request_id":"stop"`, `"request_id":"stop-stale"`, 1), http.StatusConflict)
				}
				raw := call(t, test.name, arguments, http.StatusOK)
				if !strings.Contains(raw, test.want) || strings.Contains(raw, "org_other") && org != "org_other" || strings.Contains(raw, "org_startup") && org != "org_startup" {
					t.Fatalf("tools/call result: %s", raw)
				}
				replay := call(t, test.name, arguments, http.StatusOK)
				if pgmcpcatPayload(replay) != pgmcpcatPayload(raw) && test.name != operatortool.BoardState && test.name != operatortool.Dashboard {
					t.Fatalf("replay changed: %s -> %s", raw, replay)
				}
				rpc := func(method, params string) string {
					t.Helper()
					body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{%s"_meta":{"io.modelcontextprotocol/protocolVersion":%q,"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"pgmcpcat","version":"1"}}}}`, method, params, mcp.ProtocolVersion)
					response := f.request(t, org, "/api/v2/organizations/"+org+"/mcp", body, claims, map[string]string{"Mcp-Protocol-Version": mcp.ProtocolVersion, "Mcp-Method": method, "Mcp-Name": test.name})
					requireNativeStatus(t, response, http.StatusOK)
					return response.Body.String()
				}
				if listed := rpc("tools/list", ""); !strings.Contains(listed, `"name":"`+test.name+`"`) {
					t.Errorf("MCP tools/list omitted %s: %s", test.name, listed)
				}
				if called := rpc("tools/call", fmt.Sprintf(`"name":%q,"arguments":{%s},`, test.name, arguments)); strings.Contains(called, `"isError":true`) || !strings.Contains(called, test.want) {
					t.Errorf("MCP tools/call: %s", called)
				}
				access = apikey.ProjectContext{Access: "selected", Projects: []string{"ungranted"}}
				call(t, test.name, arguments, http.StatusForbidden)
				access = apikey.ProjectContext{Access: "selected", Projects: []string{"project", "compat"}}
				if test.name != operatortool.BoardState && test.name != operatortool.Dashboard {
					permission = apikey.ScopeRead
					catalog = f.request(t, org, "/internal/v1/keys/catalog", `{}`, claims)
					requireNativeStatus(t, catalog, http.StatusOK)
					decodeHubResponse(t, catalog, &definitions)
					if entryKeyHasDefinition(definitions, test.name) {
						t.Errorf("read key discovered write tool %s", test.name)
					}
					call(t, test.name, arguments, http.StatusForbidden)
					permission = apikey.ScopeWrite
				}
			})
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
			entry, err := r.QueueEntry(t.Context(), "compat", "wi_compat_item", "compat")
			if err != nil {
				return err
			}
			if entry.Rank != "001" || entry.Priority == nil || *entry.Priority != 1 {
				t.Errorf("queue mutation lost fields: %+v", entry)
			}
			row, err := r.WorkItem(t.Context(), "project", "wi_verify_item")
			if err != nil {
				return err
			}
			version, err := r.CollaborationVersion(t.Context(), "wi_verify_item", row.Revision)
			if err != nil {
				return err
			}
			issue, err := service.hydrateCanonicalWorkItemRecord(t.Context(), row, version)
			if err != nil {
				return err
			}
			if issue.Verification == nil || issue.Verification.State != tracker.VerificationVerified {
				t.Errorf("verification = %+v", issue.Verification)
			}
			reopened, err := r.WorkItem(t.Context(), "project", "wi_reopen_item")
			if err != nil {
				return err
			}
			comments, err := r.Comments(t.Context(), "project", "wi_reopen_item", 0, 10)
			if err != nil {
				return err
			}
			if stringValue(reopened.WorkflowStateName) != "Todo" || reopened.DispatchGeneration != 1 || len(comments) != 1 {
				t.Errorf("reopen state=%s dispatch=%d comments=%d", stringValue(reopened.WorkflowStateName), reopened.DispatchGeneration, len(comments))
			}
			stopped, err := r.WorkItem(t.Context(), "project", "wi_item")
			if err != nil {
				return err
			}
			if stopped.DispatchGeneration != 1 {
				t.Errorf("stop dispatch=%d", stopped.DispatchGeneration)
			}
			if stopped.Priority == nil || *stopped.Priority != 0 {
				t.Errorf("stop priority=%v", stopped.Priority)
			}
			current, err := r.Lease(t.Context(), lease.LeaseID)
			if err == nil && current.ReleasedAt == nil {
				t.Error("stop_run did not release lease")
			}
			return err
		}); err != nil {
			t.Error(err)
		}
		if t.Failed() {
			continue
		}
		allBoard := call(t, operatortool.BoardState, `"limit":10`, http.StatusOK)
		if strings.Contains(allBoard, `"id":"compat"`) {
			t.Errorf("native board included compatibility project: %s", allBoard)
		}
		cleared := call(t, operatortool.SetQueuePriority, `"project_id":"compat","identifier":"#1","queue_scope":"compat","state":"Todo","queue_priority":"none","request_id":"clear"`, http.StatusOK)
		if !strings.Contains(cleared, `"applied"`) {
			t.Errorf("clear: %s", cleared)
		}
		if err := service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
			entry, err := r.QueueEntry(t.Context(), "compat", "wi_compat_item", "compat")
			if err == nil && (entry.Priority != nil || entry.Rank != "001") {
				t.Errorf("clear lost rank or retained priority: %+v", entry)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}

		if err := service.config.TenantRepository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			base := service.config.Hosted.Plans.Base
			plan, err := w.BillingPlan(t.Context(), billingstore.PlanReference{ID: base.ID, Version: base.Version})
			if err != nil {
				return err
			}
			plan.Features = nil
			return w.ReplaceBillingPlan(t.Context(), plan)
		}); err != nil {
			t.Fatal(err)
		}
		t.Run(org+"/disabled-order", func(t *testing.T) {
			call(t, operatortool.OrderItem, `"project_id":"compat","identifier":"#1","queue_scope":"compat","state":"Todo","rank":"002","request_id":"disabled-order"`, http.StatusTooManyRequests)
		})
		t.Run(org+"/disabled-priority", func(t *testing.T) {
			call(t, operatortool.SetQueuePriority, `"project_id":"compat","identifier":"#1","queue_scope":"compat","state":"Todo","queue_priority":"high","request_id":"disabled-priority"`, http.StatusTooManyRequests)
		})

	}
}
