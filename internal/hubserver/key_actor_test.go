package hubserver

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedKeyActorImport(t *testing.T) {
	repository, _ := projectRecordPostgresWithAdmin(t, 3)
	catalog, err := storagecatalog.Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, organization := range []tracker.OrganizationID{"org_actor_first", "org_actor_second"} {
		t.Run(string(organization), func(t *testing.T) {
			fixture := newCoreNeutralFixture(t, organization, "organizations", "hosted_tenant", "api_tokens", "hosted_members", "hosted_release_preferences", "daily_summary_preferences", "daily_summary_receipts", "hosted_project_grants", "token_grants", "hosted_sessions", "hosted_transactions", "hosted_invitations", "hosted_member_reservations", "hosted_binding_migrations", "organization_secrets", "organization_secret_audit", "project_secrets", "project_secret_audit", "hosted_audit", "hosted_audit_times")
			stamp := "2026-10-31T12:00:00Z"
			fixture.add("organizations", map[string]any{"id": string(organization), "name": "Example organization", "slug": string(organization), "local": "0", "created_at": stamp})
			fixture.add("hosted_tenant", map[string]any{"singleton": "1", "provider_id": "provider_" + string(organization), "bootstrap_subject": "example_owner", "deployment": "example", "public_url": "https://example.test", "allocation_generation": "1"})
			cases := []struct{ id, scope, kind string }{{"account_a", "operator", "personal"}, {"account_b", "admin", "personal"}, {"service", "operator", "service"}, {"worker", "worker", "service"}, {"runner", "operator", "service"}}
			for _, test := range cases {
				row := map[string]any{"id": test.id, "name": test.id, "token_hash": string(organization) + "_" + test.id, "scope": test.scope, "native_only": "1", "created_at": stamp, "updated_at": stamp, "operator_project_access": "selected", "key_kind": test.kind}
				if test.id != "service" {
					row["hosted_user_id"], row["hosted_membership_id"], row["operator_key_scope"] = "user_"+test.id, "member_"+test.id, "write"
				}
				fixture.add("api_tokens", row)
			}
			source := postgres.AccessCopyImportSource{BundlePath: fixture.write(), OrganizationID: organization, Catalog: catalog, Objects: &memoryRunDetails{}}
			if _, err := repository.ImportAccessCopy(t.Context(), source); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				for _, test := range cases {
					got, err := r.AccessCredential(t.Context(), test.id)
					if err != nil {
						return err
					}
					if got.Kind != test.kind || got.Scope != test.scope {
						t.Fatalf("%s kind=%s scope=%s want=%s/%s", test.id, got.Kind, got.Scope, test.kind, test.scope)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			missingKind := fixture.clone(t)
			table := missingKind.tables["api_tokens"]
			delete(table.Columns, "key_kind")
			missingKind.tables["api_tokens"] = table
			for index := range missingKind.records["api_tokens"] {
				delete(missingKind.records["api_tokens"][index].Values, "key_kind")
			}
			source.BundlePath = missingKind.write()
			if _, err := repository.ImportAccessCopy(t.Context(), source); !errors.Is(err, storagecatalog.ErrUnmapped) {
				t.Fatalf("source without explicit key kind error=%v", err)
			}
		})
	}
}

func TestNativeKeyActor(t *testing.T) {
	for _, test := range []struct {
		name       string
		credential hubapi.APICredential
		want       tracker.Actor
	}{
		{"browser", hubapi.APICredential{ID: "human", SessionHash: "session"}, tracker.Actor{Kind: "human", PrincipalID: "human"}},
		{"personal entry", hubapi.APICredential{ID: "human", EntryKey: &apikey.KeyAuthority{ID: "key", Kind: "personal"}}, tracker.Actor{Kind: "human", PrincipalID: "human", ViaKey: "key"}},
		{"service entry", hubapi.APICredential{ID: "service_key", EntryKey: &apikey.KeyAuthority{ID: "key", Kind: "service"}}, tracker.Actor{Kind: "integration", PrincipalID: "service_key", ViaKey: "key"}},
		{"personal hosted", hubapi.APICredential{ID: "key", HostedPrincipal: "human", HostedKeyKind: "personal", HostedKeyScope: apikey.ScopeWrite}, tracker.Actor{Kind: "human", PrincipalID: "human", ViaKey: "key"}},
		{"service hosted", hubapi.APICredential{ID: "key", HostedPrincipal: "human", HostedKeyKind: "service", HostedKeyScope: apikey.ScopeWrite}, tracker.Actor{Kind: "integration", PrincipalID: "key", ViaKey: "key"}},
		{"worker", hubapi.APICredential{ID: "worker", Scope: hubapi.APIScopeWorker}, tracker.Actor{Kind: "runner", PrincipalID: "worker"}},
		{"hosted worker", hubapi.APICredential{ID: "worker", Scope: hubapi.APIScopeWorker, HostedKeyKind: "service"}, tracker.Actor{Kind: "runner", PrincipalID: "worker"}},
		{"runner", hubapi.APICredential{ID: "runner", Scope: hubapi.APIScopeOperator, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner"}}}, tracker.Actor{Kind: "runner", PrincipalID: "runner"}},
		{"hosted runner", hubapi.APICredential{ID: "runner", Scope: hubapi.APIScopeOperator, HostedKeyKind: "service", Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner"}}}, tracker.Actor{Kind: "runner", PrincipalID: "runner"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := (hubscope.NativeScope{Credential: test.credential}).Actor(); got != test.want {
				t.Fatalf("actor=%+v want=%+v", got, test.want)
			}
		})
	}
}

func keyActorAuditForTest(t *testing.T, service *Service, route, session string) (accessstore.Audit, string) {
	t.Helper()
	organization := service.accessOrganization()
	var id int64
	if err := hostedPostgresAdmin(t, service).QueryRowContext(t.Context(), "SELECT id FROM hosted_audit WHERE organization_id=$1 AND route=$2 AND ($3='' OR session_id=$3) ORDER BY id DESC LIMIT 1", organization, route, session).Scan(&id); err != nil {
		t.Fatal(err)
	}
	var audit accessstore.Audit
	if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		audit, err = r.AccessAudit(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if audit.Mutation == nil {
		return audit, ""
	}
	raw, err := service.runDetailStore().ReadDetail(t.Context(), string(organization), *audit.Mutation)
	if err != nil {
		t.Fatal(err)
	}
	return audit, string(raw)
}

func TestMCPKeyIssueContract(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared legacy", "shared entry"} {
		t.Run(deployment, func(t *testing.T) {
			var f hostedSecurityFixture
			var shared hostedSharedFixture
			if deployment == "dedicated" {
				f = newHostedSecurityFixture(t)
			} else {
				shared = newHostedSharedFixture(t)
				f = shared.hostedSecurityFixture
			}
			user := f.user(t, "key_owner", "owner", "key-owner@example.test", "write", "")
			issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
			if err != nil {
				t.Fatal(err)
			}
			scope := hubscope.NativeScope{Organization: "org_security", Project: f.project, Credential: issuer}
			if _, err := f.service.approveTestTenantPolicy(t.Context(), "org_security/"+string(f.project), issuer.ID, policy.Change{Policy: hubTestPolicy()}); err != nil {
				t.Fatal(err)
			}
			for _, kind := range []string{"personal", "service"} {
				t.Run(kind, func(t *testing.T) {
					body := issueorigin.Stamp(issueContractTestSections, issueorigin.Origin{Kind: "worker", Fingerprint: "key-contract-" + kind})
					body = strings.Repeat("x", 42917-len(body)-1) + "\n" + body
					originalBody := body
					key, err := f.service.createHostedAPIKeyFor(t.Context(), issuer, hostedKeyRequest{Kind: kind, Name: "contract-" + kind, Scope: apikey.ScopeWrite, NeverExpires: true, ProjectAccess: hubapi.HostedProjectsAll})
					if err != nil || key.Kind != kind {
						t.Fatalf("create key=%+v error=%v", key, err)
					}
					creationScope := scope
					creationScope.SourceActor = nativeIntegrationActor("worker")
					raw, err := f.service.createNativeIssueCommand(t.Context(), creationScope, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + kind}, Title: "Key contract", Body: body, State: "Todo"})
					if err != nil {
						t.Fatal(err)
					}
					var issue tracker.NativeIssue
					if err := json.Unmarshal(raw, &issue); err != nil {
						t.Fatal(err)
					}
					checkClaimable := func(want bool) {
						t.Helper()
						evaluation, err := nativeIssueContractEvaluationForTest(t.Context(), f.service, scope, issue.WorkItemID)
						got, reason := evaluation.Allowed, evaluation.Reason
						if err != nil || got != want || (reason == "") != want {
							t.Fatalf("claimable=%v reason=%q error=%v want=%v", got, reason, err, want)
						}
					}
					checkClaimable(false)
					body = strings.Replace(body, "Complete the requested work.", "Complete the corrected work.", 1)
					expectedRevision := fmt.Sprint(issue.Revision)
					call := func(requestID string) *httptest.ResponseRecorder {
						t.Helper()
						payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.EditItem, "arguments": map[string]any{"project_id": f.project, "identifier": issue.WorkItemID, "expected_revision": expectedRevision, "body": body, "request_id": requestID}, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "key-test", "version": "1"}}}}
						path := "/api/v2/organizations/org_security/mcp"
						if deployment == "dedicated" {
							return performHubWorkCall(t, f.service, path, key.Token, operatortool.EditItem, payload)
						}
						encoded, err := json.Marshal(payload)
						if err != nil {
							t.Fatal(err)
						}
						request := hostedSharedRequest{kind: cloudassert.KindMachine, bearer: key.Token, method: http.MethodPost, target: "/organizations/org_security" + path, body: string(encoded), headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": operatortool.EditItem}}
						if deployment == "shared entry" {
							entry := payload["params"].(map[string]any)
							entry["connection_id"] = "key-contract-" + kind
							delete(entry, "_meta")
							encoded, err := json.Marshal(entry)
							if err != nil {
								t.Fatal(err)
							}
							request.target, request.body, request.bearer = "/internal/v1/keys/call", string(encoded), ""
							request.kind = cloudassert.KindKey
							request.mutate = func(claims *cloudassert.Claims) {
								claims.Key = &apikey.KeyAuthority{ID: key.ID, Kind: kind, Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "all"}}
								claims.Subject, claims.ProviderOrganization, claims.Role = user.identity.Subject, "org_provider", "owner"
								if kind == "service" {
									claims.Subject, claims.Role = "service:"+key.ID, "admin"
								}
							}
						}
						return shared.serve(t, request)
					}
					response := call("confirm")
					requireNativeStatus(t, response, http.StatusOK)
					var envelope struct {
						Result struct {
							IsError bool `json:"isError"`
							Content struct {
								Data json.RawMessage `json:"data"`
							} `json:"structuredContent"`
						} `json:"result"`
					}
					decodeHubResponse(t, response, &envelope)
					if envelope.Result.IsError {
						t.Fatalf("edit failed: %s", response.Body.String())
					}
					row, currentVersion, err := f.service.readWorkItemRecord(t.Context(), scope, string(issue.WorkItemID))
					if err != nil {
						t.Fatal(err)
					}
					issue, err = f.service.hydrateWorkItemRecord(t.Context(), row, currentVersion)
					if err != nil {
						t.Fatal(err)
					}
					if issue.Body != body || issue.Revision != 2 {
						t.Fatalf("large correction body bytes=%d revision=%d", len(issue.Body), issue.Revision)
					}
					if deployment == "shared entry" {
						decodeHubResponse(t, response, &envelope.Result.Content)
					}
					receipt := string(envelope.Result.Content.Data)
					replay := call("confirm")
					requireNativeStatus(t, replay, http.StatusOK)
					decodeHubResponse(t, replay, &envelope)
					if deployment == "shared entry" {
						decodeHubResponse(t, replay, &envelope.Result.Content)
					}
					if envelope.Result.IsError || receipt == "" || string(envelope.Result.Content.Data) != receipt {
						t.Fatal("large correction replay changed its receipt")
					}
					want := kind == "personal"
					checkClaimable(want)
					if want && len(issue.IssueContract.ConfirmedSections) != 3 || !want && len(issue.IssueContract.ConfirmedSections) != 0 {
						t.Fatalf("confirmation=%+v", issue.IssueContract)
					}
					if want {
						for section, text := range map[string]string{"acceptance criteria": "Complete the corrected work.", "must not break": "Existing behavior.", "how we know it worked": "Run the project checks."} {
							if issue.IssueContract.ConfirmedSections[section] != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) {
								t.Fatalf("confirmation does not match supplied %s section", section)
							}
						}
					}
					var original tracker.NativeIssue
					raw, err = f.service.readVersion(t.Context(), scope, string(issue.WorkItemID), "", 1)
					if err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(raw, &original); err != nil || original.Body != originalBody {
						t.Fatalf("original large issue history changed: %v", err)
					}
					var editedEvent eventstore.Event
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
						events, err := r.Events(t.Context(), scope.Project, issue.WorkItemID, 0, 3)
						if err != nil {
							return err
						}
						for _, event := range events {
							if event.Type == "issue.edited" {
								editedEvent = event
							}
						}
						if editedEvent.ID == "" {
							return corestore.ErrNotFound
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					persistedEvent, err := eventstore.NewContent(f.service.runDetailStore()).Event(t.Context(), editedEvent)
					if err != nil {
						t.Fatal(err)
					}
					actor := persistedEvent.Actor
					wantActor := tracker.Actor{Kind: "human", PrincipalID: issuer.ID, ViaKey: key.ID}
					if !want {
						wantActor.Kind, wantActor.PrincipalID = "integration", key.ID
						if deployment == "shared entry" {
							wantActor.PrincipalID = "service_" + key.ID
						}
					}
					if actor != wantActor {
						t.Fatalf("actor=%+v want=%+v", actor, wantActor)
					}
					var auditJSON, human string
					if deployment == "shared entry" {
						audit, _ := keyActorAuditForTest(t, f.service, operatortool.EditItem, key.ID)
						human = audit.ActualActor
						effective, project, route, status := audit.EffectiveUser, string(audit.ProjectID), audit.Route, audit.Status
						wantHuman := user.identity.Subject
						if !want {
							wantHuman = "service:" + key.ID
						}
						if audit.KeyID != key.ID || audit.KeyOwner != wantHuman || audit.KeyKind != kind || audit.Route != operatortool.EditItem || audit.Resource != string(issue.WorkItemID) || human != wantHuman || effective != wantHuman || project != string(f.project) || route != operatortool.EditItem || status != http.StatusOK {
							t.Fatalf("key audit=%+v actor=%s effective=%s project=%s route=%s status=%d", audit, human, effective, project, route, status)
						}
					} else {
						audit, rawAudit := keyActorAuditForTest(t, f.service, "mcp edit_item", "")
						human, auditJSON = audit.ActualActor, rawAudit
						var metadata mutation.Metadata
						if err := json.Unmarshal([]byte(auditJSON), &metadata); err != nil || metadata.ViaKey != key.ID || metadata.ActorKind != wantActor.Kind || want && human != user.identity.Subject || !want && human != wantActor.PrincipalID {
							t.Fatalf("audit=%s actor=%s error=%v", auditJSON, human, err)
						}
						update := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "cli-save"}, ExpectedRevision: issue.Revision, Body: &body}
						path := f.base + "/work-items/" + string(issue.WorkItemID)
						var response *httptest.ResponseRecorder
						if deployment == "dedicated" {
							response = performHubAPIRequest(t, f.service, http.MethodPatch, path, key.Token, update)
						} else {
							encoded, err := json.Marshal(update)
							if err != nil {
								t.Fatal(err)
							}
							response = shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, bearer: key.Token, method: http.MethodPatch, target: "/organizations/org_security" + path, body: string(encoded)})
						}
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &issue)
						checkClaimable(want)
						_, auditJSON = keyActorAuditForTest(t, f.service, "PATCH "+path, "")
						if err := json.Unmarshal([]byte(auditJSON), &metadata); err != nil || metadata.ViaKey != key.ID || metadata.ActorKind != wantActor.Kind || metadata.Source != "api" {
							t.Fatalf("CLI audit=%s error=%v", auditJSON, err)
						}
						if err := f.service.revokeHostedAPIKeyFor(t.Context(), issuer, key.ID); err != nil {
							t.Fatal(err)
						}
						refused := call("after-revoke")
						if refused.Code < 400 {
							t.Fatalf("revoked key accepted: %d %s", refused.Code, refused.Body.String())
						}
						checkClaimable(want)
					}
				})
			}
		})
	}
}

func TestMCPKeyRevisionedWrites(t *testing.T) {
	shared := newHostedSharedFixture(t)
	f := shared.hostedSecurityFixture
	user := f.user(t, "revision_owner", "owner", "revision-owner@example.test", "write", "")
	issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
	if err != nil {
		t.Fatal(err)
	}
	scope := hubscope.NativeScope{Organization: "org_security", Project: f.project, Credential: issuer}
	key, err := f.service.createHostedAPIKeyFor(t.Context(), issuer, hostedKeyRequest{Kind: "personal", Name: "revision writes", Scope: apikey.ScopeWrite, NeverExpires: true, ProjectAccess: hubapi.HostedProjectsAll})
	if err != nil {
		t.Fatal(err)
	}
	create := func(t *testing.T, name string) tracker.NativeIssue {
		t.Helper()
		raw, err := f.service.createNativeIssueCommand(t.Context(), scope, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + name}, Title: name, Body: issueContractTestSections, State: "Todo"})
		if err != nil {
			t.Fatal(err)
		}
		var item tracker.NativeIssue
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		return item
	}
	blocker := create(t, "blocker")
	call := func(t *testing.T, name, requestID string, fields map[string]any) (json.RawMessage, bool) {
		t.Helper()
		fields["project_id"], fields["request_id"] = f.project, requestID
		raw, err := json.Marshal(map[string]any{"name": name, "arguments": fields, "connection_id": "revision-writes"})
		if err != nil {
			t.Fatal(err)
		}
		response := shared.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: "/internal/v1/keys/call", body: string(raw), mutate: func(claims *cloudassert.Claims) {
			claims.Key = &apikey.KeyAuthority{ID: key.ID, Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "all"}}
			claims.Subject, claims.ProviderOrganization, claims.Role = user.identity.Subject, "org_provider", "owner"
		}})
		if response.Code != http.StatusOK {
			requireNativeStatus(t, response, http.StatusConflict)
			return json.RawMessage(response.Body.Bytes()), true
		}
		var result struct {
			Data json.RawMessage `json:"data"`
		}
		decodeHubResponse(t, response, &result)
		return result.Data, false
	}

	t.Run("revisioned writes", func(t *testing.T) {
		for _, test := range []struct {
			name, tool, relationship, operation string
		}{
			{"add dependency", operatortool.SetDependency, "dependency", "add"},
			{"remove dependency", operatortool.SetDependency, "dependency", "remove"},
			{"add related", operatortool.SetDependency, "related", "add"},
			{"remove related", operatortool.SetDependency, "related", "remove"},
			{"edit item", operatortool.EditItem, "", ""},
			{"move item", operatortool.MoveItem, "", ""},
			{"edit comment", operatortool.EditComment, "", ""},
			{"restore item", operatortool.RestoreItem, "", ""},
			{"verify fix", operatortool.VerifyFix, "", ""},
			{"transfer item", operatortool.TransferItem, "", ""},
		} {
			t.Run(test.name, func(t *testing.T) {
				item := create(t, test.name)
				fields := map[string]any{"identifier": item.WorkItemID, "expected_revision": "1", "title": "Updated"}
				raw, failed := call(t, operatortool.EditItem, test.name+" prepare", fields)
				if err := json.Unmarshal(raw, &item); err != nil || failed || item.Revision != 2 {
					t.Fatalf("prepare=%s failed=%t err=%v", raw, failed, err)
				}
				fields = map[string]any{"identifier": item.WorkItemID}
				currentRevision := item.Revision
				switch test.tool {
				case operatortool.SetDependency:
					fields["related"], fields["relationship_type"], fields["operation"] = blocker.WorkItemID, test.relationship, test.operation
					if test.operation == "remove" {
						seed := maps.Clone(fields)
						seed["operation"], seed["expected_revision"] = "add", "2"
						raw, failed := call(t, test.tool, test.name+" seed", seed)
						if err := json.Unmarshal(raw, &item); err != nil || failed || item.Revision != 3 {
							t.Fatalf("seed=%s failed=%t err=%v", raw, failed, err)
						}
						currentRevision = item.Revision
					}
				case operatortool.EditItem:
					fields["title"] = "Corrected"
				case operatortool.MoveItem:
					fields["target_state"] = "Done"
				case operatortool.TransferItem:
					if _, err := f.service.approveTestTenantPolicy(t.Context(), string(scope.Organization)+"/"+string(scope.Project), issuer.ID, policy.Change{Policy: hubTestPolicy()}); err != nil {
						t.Fatal(err)
					}
					input := changeTestInput()
					input.Repository = "https://github.com/example-org/example-project"
					bundle := changeTestSource(t, &input)
					now := f.service.config.now()
					service := f.service
					service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
					repository := service.config.TenantRepository
					orgID, projectID := scope.Organization, scope.Project
					binding := runnerauth.NewBinding()
					workerID, runnerID := hubapi.NewID("credential"), binding.RunnerID
					workerToken, err := apikey.GenerateToken()
					if err != nil {
						t.Fatal(err)
					}
					workerHash := apikey.HashToken(workerToken)
					leaseID := hubapi.NewID("lease")
					var lease leasestore.Lease
					expires := now.Add(time.Hour)
					if err := repository.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error {
						workItem, err := w.WorkItem(t.Context(), projectID, item.WorkItemID)
						if err != nil {
							return err
						}
						if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: orgID, ID: string(binding.MachineID), Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
							return err
						}
						if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: orgID, ID: workerID, Name: "Source runner", Hash: workerHash, Fingerprint: hubapi.TokenFingerprint(workerHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
							return err
						}
						if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: orgID, TokenID: workerID, ProjectID: projectID}); err != nil {
							return err
						}
						enrollmentID := hubapi.NewID("enrollment")
						operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
						if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: orgID, ID: enrollmentID, RunnerID: runnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken("example-source-enrollment"), Scope: "projects", ProjectIDs: []tracker.ProjectID{projectID}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-owner"}); err != nil {
							return err
						}
						if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, runnerID, binding.MachineID, now); err != nil {
							return err
						}
						if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: orgID, RunnerID: runnerID, MachineID: binding.MachineID, TokenID: workerID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{projectID}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
							return err
						}
						lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: string(binding.MachineID), SessionID: "source-session", LeaseID: leaseID, TTL: time.Hour})
						if err != nil {
							return err
						}
						if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: orgID, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
							return err
						}
						return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: orgID, ProjectID: projectID, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: input.PolicyID, BoundAt: now})
					}); err != nil {
						t.Fatal(err)
					}
					input.AttemptID, input.RunID = hubapi.NewID("attempt"), hubapi.NewID("run")
					for index, kind := range []string{"run.started", "run.finished"} {
						run := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: kind}, Type: kind, SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: input.AttemptID, RunID: input.RunID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: input.PolicyID, Sequence: int64(index + 1), Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "example"}}}
						if kind == "run.finished" {
							run.Data.Outcome = "succeeded"
						}
						encoded, err := json.Marshal(run)
						if err != nil {
							t.Fatal(err)
						}
						response := shared.serve(t, hostedSharedRequest{kind: cloudassert.KindMachine, bearer: workerToken, method: http.MethodPost, target: "/organizations/" + string(scope.Organization) + f.base + "/work-items/" + string(item.WorkItemID) + "/events", body: string(encoded)})
						requireNativeStatus(t, response, http.StatusOK)
					}
					ctx := f.service.contentContext(t.Context())
					raw, err := f.service.createChangeRecordCommand(ctx, scope, string(item.WorkItemID), "key-transfer-create", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "transfer-change"}, Title: "Transfer source"})
					if err != nil {
						t.Fatal(err)
					}
					var change tracker.ChangeRequest
					if err := json.Unmarshal(raw, &change); err != nil {
						t.Fatal(err)
					}
					raw, err = f.service.publishChangeVersionRecordCommand(ctx, scope, string(item.WorkItemID), change.ID, "key-transfer-publish", tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "transfer-version"}, ChangeVersionInput: input, SourceBundle: bundle})
					if err != nil {
						t.Fatalf("publish source version: %#v", err)
					}
					var version tracker.ChangeVersion
					if err := json.Unmarshal(raw, &version); err != nil {
						t.Fatal(err)
					}
					fields["version_id"] = version.ID
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "source finished")
						return err
					}); err != nil {
						t.Fatal(err)
					}
				case operatortool.RestoreItem:
					_, err := f.service.setNativeArchiveCommand(t.Context(), scope, string(item.WorkItemID), archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "archive"}, ExpectedRevision: item.Revision}, true)
					if err != nil {
						t.Fatal(err)
					}
					currentRevision++
				case operatortool.VerifyFix:
					raw, failed := call(t, operatortool.MoveItem, "verification prepare", map[string]any{"identifier": item.WorkItemID, "expected_revision": "2", "target_state": "Done"})
					if err := json.Unmarshal(raw, &item); err != nil || failed || item.Revision != 3 {
						t.Fatalf("prepare verification=%s failed=%t err=%v", raw, failed, err)
					}
					currentRevision = item.Revision
					fields["result"] = "verified"
				case operatortool.EditComment:
					raw, failed := call(t, operatortool.AddComment, test.name+" create", map[string]any{"identifier": item.WorkItemID, "body": "Original"})
					var comment tracker.NativeComment
					if err := json.Unmarshal(raw, &comment); err != nil || failed || comment.Revision != 1 {
						t.Fatalf("create comment=%s failed=%t err=%v", raw, failed, err)
					}
					fields["comment_id"], fields["body"], fields["expected_revision"] = comment.ID, "Updated", "1"
					raw, failed = call(t, test.tool, test.name+" prepare comment", maps.Clone(fields))
					if err := json.Unmarshal(raw, &comment); err != nil || failed || comment.Revision != 2 {
						t.Fatalf("prepare comment=%s failed=%t err=%v", raw, failed, err)
					}
					currentRevision = comment.Revision
					fields["body"] = "Corrected"
				}
				fields["expected_revision"] = strconv.FormatInt(int64(currentRevision-1), 10)
				raw, failed = call(t, test.tool, test.name+" stale", maps.Clone(fields))
				var conflict operatortool.ConflictError
				if err := json.Unmarshal(raw, &conflict); err != nil || !failed || conflict.Code != "revision_conflict" || conflict.CurrentRevision != int64(currentRevision) {
					t.Fatalf("stale write=%s failed=%t err=%v", raw, failed, err)
				}
				fields["expected_revision"] = strconv.FormatInt(int64(currentRevision), 10)
				raw, failed = call(t, test.tool, test.name+" fresh", maps.Clone(fields))
				var result struct {
					Revision tracker.Revision `json:"revision,string"`
				}
				if err := json.Unmarshal(raw, &result); err != nil || failed || result.Revision != currentRevision+1 {
					t.Fatalf("fresh write=%s failed=%t err=%v", raw, failed, err)
				}
				replay, failed := call(t, test.tool, test.name+" fresh", maps.Clone(fields))
				if failed || !bytes.Equal(raw, replay) {
					t.Fatalf("replay=%s failed=%t want=%s", replay, failed, raw)
				}
			})
		}
	})
}
