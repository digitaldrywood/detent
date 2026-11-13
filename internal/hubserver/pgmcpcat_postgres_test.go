package hubserver

import (
	"context"
	"encoding/json"
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
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresMissingOperatorCatalog(t *testing.T) {
	t.Parallel()
	permission := apikey.ScopeWrite
	access := apikey.ProjectContext{Access: "selected", Projects: []string{"project", "compat"}}
	authority := func(id, binding string) *apikey.KeyAuthority {
		return &apikey.KeyAuthority{ID: id, CredentialBinding: binding, Kind: "personal", Permission: permission, ProjectContext: access}
	}
	f := newSharedRouteFixture(t, func(cfg *Config) {
		plans := hostedplans.PilotPlans()
		cfg.Hosted.Plans = &plans
		org := cfg.Hosted.OrganizationID
		cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: org, ProviderOrganization: "provider_" + org, Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: authority(origin.KeyID, origin.CredentialBinding)}, nil
		}
	})
	for _, org := range []string{"org_startup", "org_other"} {
		service := f.services[org]
		pgmcpcatSeed(t, service, tracker.OrganizationID(org))
		now := service.config.now().UTC()
		blocker := tracker.NativeIssue{
			NativeReference: tracker.NativeReference{OrganizationID: tracker.OrganizationID(org), ProjectID: "compat", WorkItemID: "wi_compat_blocker", Number: 2, Revision: 1, Profile: "github_compatible"},
			Title:           "Example dependency blocker", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: org + "_owner"},
			CreatedAt: now, UpdatedAt: now,
		}
		blockerRaw, err := json.Marshal(blocker)
		if err != nil {
			t.Fatal(err)
		}
		version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{
			OrganizationID: blocker.OrganizationID, ProjectID: blocker.ProjectID, WorkItemID: blocker.WorkItemID, RecordID: string(blocker.WorkItemID), Revision: 1,
		}, blockerRaw)
		if err != nil {
			t.Fatal(err)
		}
		document, err := searchstore.NewContent(service.runDetailStore()).Prepare(t.Context(), searchstore.Document{
			OrganizationID: blocker.OrganizationID, ProjectID: blocker.ProjectID, WorkItemID: blocker.WorkItemID, ID: "issue:" + string(blocker.WorkItemID),
			SourceID: string(blocker.WorkItemID), Kind: "issue", Scope: "issue", Revision: 1, UpdatedAt: now,
		}, searchstore.Text{Title: blocker.Title})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), blocker.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{
				OrganizationID: blocker.OrganizationID, ProjectID: blocker.ProjectID, NativeID: blocker.WorkItemID, Number: 2, Revision: 1,
				Title: blocker.Title, WorkflowStateName: &blocker.State, ActorKind: &blocker.Actor.Kind, ActorPrincipalID: &blocker.Actor.PrincipalID,
				NativeCreatedAt: now, NativeUpdatedAt: now,
			}); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
				return err
			}
			return w.PutSearch(t.Context(), document, 0)
		}); err != nil {
			t.Fatal(err)
		}
		lease := seedEntryRuntimeAttempt(t, service, tracker.OrganizationID(org), "wi_00000000000000000000000000000001")
		if err := service.refreshBoardRecord(t.Context(), tracker.OrganizationID(org), viewstore.BoardCandidate{ProjectID: "project", WorkItemID: "wi_00000000000000000000000000000001"}, service.config.now()); err != nil {
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
			{operatortool.SetDependency, "compat", `"identifier":"#1","related":"wi_compat_blocker","operation":"add","request_id":"dependency"`, `"applied"`},
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
				if test.name == operatortool.SetDependency {
					collided := call(t, test.name, strings.Replace(arguments, `"operation":"add"`, `"operation":"remove"`, 1), http.StatusConflict)
					if !strings.Contains(collided, `"code":"revision_conflict"`) {
						t.Fatalf("changed dependency request reused the receipt: %s", collided)
					}
					crossProject := call(t, test.name, `"project_id":"compat","identifier":"#1","related":"wi_00000000000000000000000000000001","operation":"add","request_id":"cross-project"`, http.StatusUnprocessableEntity)
					if !strings.Contains(crossProject, `"code":"not_found"`) {
						t.Fatalf("dependency lookup escaped its project: %s", crossProject)
					}
					call(t, test.name, `"project_id":"compat","identifier":"#1","related":"wi_compat_blocker","relationship_type":"related","operation":"add","request_id":"related"`, http.StatusUnprocessableEntity)
					call(t, test.name, `"project_id":"compat","identifier":"#2","related":"wi_compat_item","operation":"add","request_id":"cycle"`, http.StatusUnprocessableEntity)

					checkDependency := func(want bool, eventCount int) {
						t.Helper()
						if err := service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
							source, err := r.WorkItem(t.Context(), "compat", "wi_compat_item")
							if err != nil {
								return err
							}
							var applied struct {
								WorkItemID int64  `json:"work_item_id"`
								Kind       string `json:"kind"`
								Status     string `json:"status"`
							}
							got := pgmcpcatPayload(raw)
							if err := json.Unmarshal([]byte(got), &applied); err != nil || applied.WorkItemID != source.ID || applied.Kind != "set_dependency" || applied.Status != "applied" || source.Revision != 1 {
								t.Errorf("compatibility response or revision changed: %s, revision %d, error %v", got, source.Revision, err)
							}
							page, err := r.Relationships(t.Context(), "compat", source.NativeID, corestore.RelationshipCursor{}, 4)
							if err != nil {
								return err
							}
							if !want && len(page.Items) != 0 || want && (len(page.Items) != 1 || page.Items[0].Type != "dependency" || page.Items[0].Direction != "blocked_by" || page.Items[0].RelatedProjectID != "compat" || page.Items[0].RelatedWorkItemID != blocker.WorkItemID) {
								t.Errorf("unexpected compatibility dependency facts: %+v", page.Items)
							}
							events, err := r.Events(t.Context(), "compat", source.NativeID, 0, 4)
							if err != nil {
								return err
							}
							dependencyEvents, orderEvents := 0, 0
							for _, event := range events {
								switch event.Type {
								case "dependency.changed":
									dependencyEvents++
								case "queue.order_changed":
									orderEvents++
								default:
									t.Errorf("unexpected compatibility dependency event: %q", event.Type)
								}
							}
							if dependencyEvents != eventCount || orderEvents != 1 {
								t.Errorf("got %d dependency events and %d order events, want %d and the one order_item event", dependencyEvents, orderEvents, eventCount)
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					checkDependency(true, 1)
					remove := strings.Replace(strings.Replace(arguments, `"operation":"add"`, `"operation":"remove"`, 1), `"request_id":"dependency"`, `"request_id":"remove-dependency"`, 1)
					call(t, test.name, remove, http.StatusOK)
					if replay := call(t, test.name, arguments, http.StatusOK); pgmcpcatPayload(replay) != pgmcpcatPayload(raw) {
						t.Fatalf("add receipt changed after removal: %s", replay)
					}
					checkDependency(false, 2)
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
			row, err := r.WorkItem(t.Context(), "project", "wi_00000000000000000000000000000002")
			if err != nil {
				return err
			}
			version, err := r.CollaborationVersion(t.Context(), "wi_00000000000000000000000000000002", row.Revision)
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
			reopened, err := r.WorkItem(t.Context(), "project", "wi_00000000000000000000000000000003")
			if err != nil {
				return err
			}
			comments, err := r.Comments(t.Context(), "project", "wi_00000000000000000000000000000003", 0, 10)
			if err != nil {
				return err
			}
			if hubapi.StringValue(reopened.WorkflowStateName) != "Todo" || reopened.DispatchGeneration != 1 || len(comments) != 1 {
				t.Errorf("reopen state=%s dispatch=%d comments=%d", hubapi.StringValue(reopened.WorkflowStateName), reopened.DispatchGeneration, len(comments))
			}
			stopped, err := r.WorkItem(t.Context(), "project", "wi_00000000000000000000000000000001")
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
