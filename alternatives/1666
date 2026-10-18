package hubserver

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedKeyActorMigration(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE api_tokens(id TEXT PRIMARY KEY, hosted_user_id TEXT, hosted_membership_id TEXT, operator_key_scope TEXT, scope TEXT, native_only INTEGER);
CREATE TABLE runner_identities(token_id TEXT);`); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id, scope     string
		owned, runner bool
	}{
		{id: "account_a", scope: "operator", owned: true},
		{id: "account_b", scope: "admin", owned: true},
		{id: "service", scope: "operator"},
		{id: "worker", scope: "worker", owned: true},
		{id: "runner", scope: "operator", owned: true, runner: true},
	} {
		var owner, membership, permission any
		if test.owned {
			owner, membership, permission = "user_"+test.id, "member_"+test.id, "write"
		}
		if _, err := db.ExecContext(t.Context(), "INSERT INTO api_tokens VALUES(?,?,?,?,?,1)", test.id, owner, membership, permission, test.scope); err != nil {
			t.Fatal(err)
		}
		if test.runner {
			if _, err := db.ExecContext(t.Context(), "INSERT INTO runner_identities VALUES(?)", test.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	raw, err := migrationFiles.ReadFile("migrations/20261013030500_api_key_actor_kind.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), strings.Split(string(raw), "-- +goose Down")[0]); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ id, kind string }{
		{"account_a", "personal"}, {"account_b", "personal"}, {"service", "service"}, {"worker", "service"}, {"runner", "service"},
	} {
		var got string
		if err := db.QueryRowContext(t.Context(), "SELECT key_kind FROM api_tokens WHERE id=?", test.id).Scan(&got); err != nil || got != test.kind {
			t.Fatalf("%s kind=%s want=%s error=%v", test.id, got, test.kind, err)
		}
	}
}

func TestNativeKeyActor(t *testing.T) {
	for _, test := range []struct {
		name       string
		credential apiCredential
		want       tracker.Actor
	}{
		{"browser", apiCredential{ID: "human", SessionHash: "session"}, tracker.Actor{Kind: "human", PrincipalID: "human"}},
		{"personal entry", apiCredential{ID: "human", EntryKey: &apikey.KeyAuthority{ID: "key", Kind: "personal"}}, tracker.Actor{Kind: "human", PrincipalID: "human", ViaKey: "key"}},
		{"service entry", apiCredential{ID: "service_key", EntryKey: &apikey.KeyAuthority{ID: "key", Kind: "service"}}, tracker.Actor{Kind: "integration", PrincipalID: "service_key", ViaKey: "key"}},
		{"personal hosted", apiCredential{ID: "key", HostedPrincipal: "human", HostedKeyKind: "personal", HostedKeyScope: apikey.ScopeWrite}, tracker.Actor{Kind: "human", PrincipalID: "human", ViaKey: "key"}},
		{"service hosted", apiCredential{ID: "key", HostedPrincipal: "human", HostedKeyKind: "service", HostedKeyScope: apikey.ScopeWrite}, tracker.Actor{Kind: "integration", PrincipalID: "key", ViaKey: "key"}},
		{"worker", apiCredential{ID: "worker", Scope: apiScopeWorker}, tracker.Actor{Kind: "runner", PrincipalID: "worker"}},
		{"hosted worker", apiCredential{ID: "worker", Scope: apiScopeWorker, HostedKeyKind: "service"}, tracker.Actor{Kind: "runner", PrincipalID: "worker"}},
		{"runner", apiCredential{ID: "runner", Scope: apiScopeOperator, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner"}}}, tracker.Actor{Kind: "runner", PrincipalID: "runner"}},
		{"hosted runner", apiCredential{ID: "runner", Scope: apiScopeOperator, HostedKeyKind: "service", Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner"}}}, tracker.Actor{Kind: "runner", PrincipalID: "runner"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := (nativeScope{credential: test.credential}).actor(); got != test.want {
				t.Fatalf("actor=%+v want=%+v", got, test.want)
			}
		})
	}
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
			scope := nativeScope{organization: "org_security", project: f.project, credential: issuer}
			if _, err := f.service.database.approvePolicy(t.Context(), "org_security/"+string(f.project), issuer.ID, policy.Change{Policy: hubTestPolicy()}); err != nil {
				t.Fatal(err)
			}
			for _, kind := range []string{"personal", "service"} {
				t.Run(kind, func(t *testing.T) {
					body := issueorigin.Stamp(issueContractTestSections, issueorigin.Origin{Kind: "worker", Fingerprint: "key-contract-" + kind})
					body = strings.Repeat("x", 42917-len(body)-1) + "\n" + body
					originalBody := body
					key, err := f.service.createHostedAPIKeyFor(t.Context(), issuer, hostedKeyRequest{Kind: kind, Name: "contract-" + kind, Scope: apikey.ScopeWrite, NeverExpires: true, ProjectAccess: hostedProjectsAll})
					if err != nil || key.Kind != kind {
						t.Fatalf("create key=%+v error=%v", key, err)
					}
					creationScope := scope
					creationScope.sourceActor = nativeIntegrationActor("worker")
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
						_, id, err := readNativeIssue(t.Context(), f.service.database.db, scope, string(issue.WorkItemID))
						if err != nil {
							t.Fatal(err)
						}
						got, reason, err := nativeIssueContractClaimable(t.Context(), f.service.database.db, scope, id)
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
					issue, _, err = readNativeIssue(t.Context(), f.service.database.db, scope, string(issue.WorkItemID))
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
					var actorJSON string
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT actor_json FROM collaboration_events WHERE work_item_id=? AND type='issue.edited' ORDER BY sequence DESC LIMIT 1", issue.WorkItemID).Scan(&actorJSON); err != nil {
						t.Fatal(err)
					}
					var actor tracker.Actor
					if err := json.Unmarshal([]byte(actorJSON), &actor); err != nil {
						t.Fatal(err)
					}
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
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT actual_actor,mutation_json FROM hosted_audit WHERE route='mcp edit_item' ORDER BY id DESC LIMIT 1").Scan(&human, &auditJSON); err != nil {
						t.Fatal(err)
					}
					var metadata mutation.Metadata
					if err := json.Unmarshal([]byte(auditJSON), &metadata); err != nil || metadata.ViaKey != key.ID || metadata.ActorKind != wantActor.Kind || want && human != user.identity.Subject || !want && human != wantActor.PrincipalID {
						t.Fatalf("audit=%s actor=%s error=%v", auditJSON, human, err)
					}
					if deployment != "shared entry" {
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
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT mutation_json FROM hosted_audit WHERE route=? ORDER BY id DESC LIMIT 1", "PATCH "+path).Scan(&auditJSON); err != nil {
							t.Fatal(err)
						}
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
