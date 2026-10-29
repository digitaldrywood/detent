package hubserver

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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
					if deployment == "shared entry" {
						var effective, project, route string
						var status int
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT actual_actor,effective_user,project_id,route,status,mutation_json FROM hosted_audit WHERE session_id=? AND event='key_call' ORDER BY id DESC LIMIT 1", key.ID).Scan(&human, &effective, &project, &route, &status, &auditJSON); err != nil {
							t.Fatal(err)
						}
						var metadata struct {
							KeyID     string `json:"key_id"`
							Owner     string `json:"owner"`
							Kind      string `json:"kind"`
							Operation string `json:"operation"`
							Resource  string `json:"resource"`
						}
						wantHuman := user.identity.Subject
						if !want {
							wantHuman = "service:" + key.ID
						}
						if err := json.Unmarshal([]byte(auditJSON), &metadata); err != nil || metadata.KeyID != key.ID || metadata.Owner != wantHuman || metadata.Kind != kind || metadata.Operation != operatortool.EditItem || metadata.Resource != string(issue.WorkItemID) || human != wantHuman || effective != wantHuman || project != string(f.project) || route != operatortool.EditItem || status != http.StatusOK {
							t.Fatalf("key audit=%s actor=%s effective=%s project=%s route=%s status=%d error=%v", auditJSON, human, effective, project, route, status, err)
						}
					} else {
						if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT actual_actor,mutation_json FROM hosted_audit WHERE route='mcp edit_item' ORDER BY id DESC LIMIT 1").Scan(&human, &auditJSON); err != nil {
							t.Fatal(err)
						}
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

func TestMCPKeyRevisionedWrites(t *testing.T) {
	shared := newHostedSharedFixture(t)
	f := shared.hostedSecurityFixture
	user := f.user(t, "revision_owner", "owner", "revision-owner@example.test", "write", "")
	issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: "org_security", project: f.project, credential: issuer}
	key, err := f.service.createHostedAPIKeyFor(t.Context(), issuer, hostedKeyRequest{Kind: "personal", Name: "revision writes", Scope: apikey.ScopeWrite, NeverExpires: true, ProjectAccess: hostedProjectsAll})
	if err != nil {
		t.Fatal(err)
	}
	create := func(t *testing.T, name string) tracker.NativeIssue {
		t.Helper()
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
				t.Error(err)
			}
		})
		item, err := createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: name, Body: issueContractTestSections, State: "Todo"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.database.commit(t.Context(), tx); err != nil {
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
					now := f.service.config.now()
					machine := tracker.MachineID(newNativeID("machine"))
					operatorSQL(t, f, `INSERT INTO machines(id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id,token_id) VALUES(?,?,?,1,'test',?,?,?,?,?)`, machine, machine, machine, formatHubTime(now), formatHubTime(now), formatHubTime(now), scope.organization, issuer.ID)
					tx, err := f.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
							t.Error(err)
						}
					})
					input := changeTestInput()
					input.Repository = "https://github.com/example-org/example-project"
					if _, err := f.service.approveChangeReviewPolicyCommand(t.Context(), tx, scope, tracker.ApproveChangeReviewPolicy{Policy: tracker.ChangeReviewPolicy{PolicyID: input.PolicyID}}); err != nil {
						t.Fatal(err)
					}
					input.AttemptID = seedHealthRateAttempt(t, tx, scope, item, string(machine), "succeeded", tracker.NativeRunData{PolicyID: input.PolicyID}, now, -1)
					input.RunID = newNativeID("run")
					if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET run_id=? WHERE id=?", input.RunID, input.AttemptID); err != nil {
						t.Fatal(err)
					}
					bundle := changeTestSource(t, &input)
					created, err := f.service.createChangeCommand(t.Context(), tx, scope, string(item.WorkItemID), tracker.CreateChange{Title: "Transfer source"}, now)
					if err != nil {
						t.Fatal(err)
					}
					change := created.(tracker.ChangeRequest)
					published, err := f.service.publishChangeVersionCommand(t.Context(), tx, scope, string(item.WorkItemID), change.ID, tracker.PublishChangeVersion{ChangeVersionInput: input, SourceBundle: bundle}, now)
					if err != nil {
						t.Fatalf("publish source: %#v", err)
					}
					if err := f.service.database.commit(t.Context(), tx); err != nil {
						t.Fatal(err)
					}
					fields["version_id"] = published.(tracker.ChangeVersion).ID
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
