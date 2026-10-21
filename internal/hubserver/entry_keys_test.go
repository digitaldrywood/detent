package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestEntryKeyCurrentAuthority(t *testing.T) {
	for _, kind := range []string{"personal", "service"} {
		for _, access := range []string{"all", "selected", "project"} {
			for _, role := range []string{"owner", "admin", "member", "viewer"} {
				for _, member := range []bool{true, false} {
					name := fmt.Sprintf("%s/%s/%s/member=%v", kind, access, role, member)
					t.Run(name, func(t *testing.T) {
						f := newHostedSharedFixture(t)
						user := f.user(t, "key_owner", role, "key-owner@example.test", "write", "")
						if !member {
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET active=0 WHERE user_id=?", user.identity.Subject)
						}
						projects := []string(nil)
						if access != "all" {
							projects = []string{string(f.project)}
						}
						key := apikey.KeyAuthority{ID: "key_matrix", Kind: kind, Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: access, Projects: projects}}
						mutate := func(claims *cloudassert.Claims) {
							claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "org_provider", role
							if kind == "service" {
								claims.Subject, claims.Role = "service:"+key.ID, "admin"
							}
						}
						read := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
						wantRead := http.StatusOK
						if kind == "personal" && !member {
							wantRead = http.StatusForbidden
						}
						if read.Code != wantRead {
							t.Fatalf("read=%d want=%d %s", read.Code, wantRead, read.Body.String())
						}
						if kind == "service" {
							second := key
							second.ID = "key_matrix_second"
							secondRead := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: func(claims *cloudassert.Claims) {
								mutate(claims)
								claims.Key, claims.Subject = &second, "service:"+second.ID
							}})
							if secondRead.Code != http.StatusOK {
								t.Fatalf("second service key read=%d %s", secondRead.Code, secondRead.Body.String())
							}
						}
						write := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: f.base + "/work-items", body: "{\"body\":\"\\n## Acceptance criteria\\nComplete the requested work.\\n## Must not break\\nExisting behavior.\\n## How we know it worked\\nRun the project checks.\",\"idempotency_key\":\"matrix-write\",\"state\":\"Todo\",\"title\":\"Scope and membership regression\"}", mutate: mutate})
						wantWrite := http.StatusOK
						if kind == "personal" && !member {
							wantWrite = http.StatusForbidden
						} else if kind == "personal" && role == "viewer" {
							wantWrite = http.StatusNotFound
						}
						if write.Code != wantWrite {
							t.Fatalf("write=%d want=%d %s", write.Code, wantWrite, write.Body.String())
						}
						if role == "owner" && member && access == "all" {
							key.Permission = apikey.ScopeRead
							var logs bytes.Buffer
							previousLogger := f.service.config.Logger
							f.service.config.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: f.base + "/work-items", body: `{"title":"private-token"}`, mutate: mutate})
							f.service.config.Logger = previousLogger
							key.Permission = apikey.ScopeWrite
							failure := requireNativeCode(t, denied, http.StatusForbidden, "insufficient_scope")
							var entry map[string]any
							if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
								t.Fatalf("expected exactly one JSON log entry: %v: %s", err, &logs)
							}
							const reason = "API key permission does not permit writes"
							if failure.Message != reason || entry["reason"] != reason || entry["level"] != "WARN" || entry["code"] != "insufficient_scope" || strings.Contains(logs.String()+denied.Body.String(), "private-token") {
								t.Fatalf("refusal = %#v, log = %#v", failure, entry)
							}
						}
						if kind == "service" || member {
							claims := f.claims(nil, cloudassert.KindKey, http.MethodGet, f.base+"/work-items", nil)
							mutate(&claims)
							credential, _, err := f.service.entryKeyCredential(t.Context(), claims)
							if err != nil {
								t.Fatal(err)
							}
							visible, err := f.service.hostedReadableProjects(t.Context(), credential)
							if err != nil || len(visible) != 1 || visible[0].ID != string(f.project) {
								t.Fatalf("live key projects=%v error=%v", visible, err)
							}
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET deleted_at='2026-10-09T00:00:00Z' WHERE id=?", f.project)
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
							if denied.Code != http.StatusNotFound {
								t.Fatalf("deleted project read=%d %s", denied.Code, denied.Body.String())
							}
							visible, err = f.service.hostedReadableProjects(t.Context(), credential)
							if err != nil || len(visible) != 0 {
								t.Fatalf("deleted key projects=%v error=%v", visible, err)
							}
							operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET deleted_at=NULL WHERE id=?", f.project)
						}
						if kind == "personal" && member {
							operatorSQL(t, f.hostedSecurityFixture, "DELETE FROM hosted_project_grants WHERE user_id=?", user.identity.Subject)
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
							if denied.Code != http.StatusNotFound {
								t.Fatalf("removed grant=%d %s", denied.Code, denied.Body.String())
							}
						}
						if access != "all" {
							key.Projects = []string{"prj_other"}
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
							if kind == "service" || member {
								if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "key_project_denied") {
									t.Fatalf("scope refusal=%d %s", denied.Code, denied.Body.String())
								}
							}
						}
					})
				}
			}
		}
	}
}

func TestEntryKeyCallReportsMoveItemRequestErrors(t *testing.T) {
	f := newHostedSharedFixture(t)
	user := f.user(t, "key_owner", "owner", "key-owner@example.test", "write", "")
	key := apikey.KeyAuthority{ID: "key_move", Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "all"}}
	mutate := func(claims *cloudassert.Claims) {
		claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "org_provider", "owner"
	}
	created := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: f.base + "/work-items", body: "{\"body\":\"\\n## Acceptance criteria\\nComplete the requested work.\\n## Must not break\\nExisting behavior.\\n## How we know it worked\\nRun the project checks.\",\"idempotency_key\":\"move-errors\",\"state\":\"Todo\",\"title\":\"Move error reporting\"}", mutate: mutate})
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	auditCount := func() int {
		t.Helper()
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE session_id=?", key.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	beforeRead := auditCount()
	requireNativeStatus(t, f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate}), http.StatusOK)
	encodedRead, err := json.Marshal(map[string]any{"name": operatortool.WorkItem, "arguments": map[string]any{"project_id": f.project, "reference": issue.WorkItemID}, "connection_id": "move-errors"})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: "/internal/v1/keys/call", body: string(encodedRead), mutate: mutate}), http.StatusOK)
	if auditCount() != beforeRead {
		t.Fatal("successful reads wrote audit rows")
	}
	move := func(requestID string, arguments map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		arguments["project_id"], arguments["identifier"], arguments["request_id"] = f.project, issue.WorkItemID, requestID
		encoded, err := json.Marshal(map[string]any{"name": operatortool.MoveItem, "arguments": arguments, "connection_id": "move-errors"})
		if err != nil {
			t.Fatal(err)
		}
		return f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: "/internal/v1/keys/call", body: string(encoded), mutate: mutate})
	}
	revision := fmt.Sprint(issue.Revision)
	for _, test := range []struct {
		name, code, message string
		arguments           map[string]any
	}{
		{"numeric revision", "invalid_request", "expected_revision: must be a string", map[string]any{"target_state": "Done", "expected_revision": issue.Revision}},
		{"empty revision", "invalid_request", "expected_revision: must contain at least 1 characters", map[string]any{"target_state": "Done", "expected_revision": ""}},
		{"status block detail", "invalid_request", "reason_detail: must be plain text without a status block", map[string]any{"target_state": "Done", "expected_revision": revision, "reason_detail": "```detent-status\nstatus: blocked\n```"}},
		{"same state", "invalid_request", "", map[string]any{"target_state": "Todo", "expected_revision": revision}},
		{"graph", "transition_not_allowed", "Workflow transition is not allowed", map[string]any{"target_state": "Blocked", "expected_revision": revision}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			beforeAudit := auditCount()
			response := move("move-errors-"+strings.ReplaceAll(test.name, " ", "-"), test.arguments)
			if auditCount() != beforeAudit+1 {
				t.Fatalf("refusal wrote %d audit rows", auditCount()-beforeAudit)
			}
			failure := requireNativeCode(t, response, http.StatusUnprocessableEntity, test.code)
			if test.message == "" && !strings.HasPrefix(failure.Message, "move_item cannot resume an item already in the requested state") || test.message != "" && failure.Message != test.message {
				t.Fatalf("message=%q want %q", failure.Message, test.message)
			}
			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
				t.Fatalf("expected exactly one refusal log: %v: %s", err, &logs)
			}
			if entry["level"] != "WARN" || entry["code"] != test.code || entry["reason"] != failure.Message || entry["route"] != "/internal/v1/keys/call" {
				t.Fatalf("refusal log = %#v", entry)
			}
		})
	}
	beforeAudit := auditCount()
	moved := move("move-errors-done", map[string]any{"target_state": "Done", "expected_revision": revision})
	requireNativeStatus(t, moved, http.StatusOK)
	if auditCount() != beforeAudit+1 {
		t.Fatalf("mutation wrote %d audit rows", auditCount()-beforeAudit)
	}
	var operation, resource string
	var status int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT route,json_extract(mutation_json,'$.resource'),status FROM hosted_audit WHERE session_id=? ORDER BY id DESC LIMIT 1", key.ID).Scan(&operation, &resource, &status); err != nil {
		t.Fatal(err)
	}
	if operation != operatortool.MoveItem || resource != string(issue.WorkItemID) || status != http.StatusOK {
		t.Fatalf("audit operation=%s resource=%s status=%d", operation, resource, status)
	}
	var receipt struct {
		Data tracker.NativeIssue `json:"data"`
	}
	decodeHubResponse(t, moved, &receipt)
	if receipt.Data.State != "Done" {
		t.Fatalf("state=%q", receipt.Data.State)
	}
}
