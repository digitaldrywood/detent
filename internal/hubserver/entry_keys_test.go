package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

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
	requireNativeStatus(t, f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: "/internal/v1/keys/catalog", mutate: mutate}), http.StatusOK)
	requireNativeStatus(t, f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate}), http.StatusOK)
	encodedRead, err := json.Marshal(map[string]any{"name": operatortool.WorkItem, "arguments": map[string]any{"project_id": f.project, "reference": issue.WorkItemID}, "connection_id": "move-errors"})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: "/internal/v1/keys/call", body: string(encodedRead), mutate: mutate}), http.StatusOK)
	if auditCount() != beforeRead {
		t.Fatal("successful reads wrote audit rows")
	}
	operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET active=0 WHERE user_id=?", user.identity.Subject)
	beforeCatalog := auditCount()
	requireNativeStatus(t, f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: "/internal/v1/keys/catalog", mutate: mutate}), http.StatusForbidden)
	if auditCount() != beforeCatalog+1 {
		t.Fatalf("catalog refusal wrote %d audit rows", auditCount()-beforeCatalog)
	}
	var catalogOperation string
	var catalogStatus int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT route,status FROM hosted_audit WHERE session_id=? ORDER BY id DESC LIMIT 1", key.ID).Scan(&catalogOperation, &catalogStatus); err != nil {
		t.Fatal(err)
	}
	if catalogOperation != "POST /internal/v1/keys/catalog" || catalogStatus != http.StatusForbidden {
		t.Fatalf("catalog audit operation=%s status=%d", catalogOperation, catalogStatus)
	}
	operatorSQL(t, f.hostedSecurityFixture, "UPDATE hosted_members SET active=1 WHERE user_id=?", user.identity.Subject)
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
			beforeAudit := auditCount()
			response := move("move-errors-"+strings.ReplaceAll(test.name, " ", "-"), test.arguments)
			if auditCount() != beforeAudit+1 {
				t.Fatalf("refusal wrote %d audit rows", auditCount()-beforeAudit)
			}
			failure := requireNativeCode(t, response, http.StatusUnprocessableEntity, test.code)
			if test.message == "" && !strings.HasPrefix(failure.Message, "move_item cannot resume an item already in the requested state") || test.message != "" && failure.Message != test.message {
				t.Fatalf("message=%q want %q", failure.Message, test.message)
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

type entryKeyAuditWriter struct {
	*httptest.ResponseRecorder
	beforeWrite func()
}

func (w *entryKeyAuditWriter) WriteHeader(status int) {
	if w.beforeWrite != nil {
		before := w.beforeWrite
		w.beforeWrite = nil
		before()
	}
	w.ResponseRecorder.WriteHeader(status)
}

func (w *entryKeyAuditWriter) Write(body []byte) (int, error) {
	if w.beforeWrite != nil {
		before := w.beforeWrite
		w.beforeWrite = nil
		before()
	}
	return w.ResponseRecorder.Write(body)
}

func TestEntryKeyMutationAuditCommit(t *testing.T) {
	for _, transport := range []string{"HTTP", "tool"} {
		for _, failure := range []string{"cancel response", "reject audit insert", "reject audit update", "reject completed audit"} {
			t.Run(transport+"/"+failure, func(t *testing.T) {
				var logs hostedLogBuffer
				f := newHostedSharedFixture(t, func(cfg *Config) {
					cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
				})
				user := f.user(t, "key_owner", "owner", "key-owner@example.test", "write", "")
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET duplicate_challenge=0 WHERE id=?", f.project)
				for i := range 2 {
					key := apikey.KeyAuthority{ID: fmt.Sprintf("key_audit_%d", i), Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "all"}}
					title := fmt.Sprintf("Audited mutation %d", i)
					target := f.base + "/work-items"
					args := map[string]any{"title": title, "body": nativeContractTestBody, "state": "Todo", "idempotency_key": fmt.Sprintf("audit-%d", i)}
					var payload any = args
					if transport == "tool" {
						target = "/internal/v1/keys/call"
						payload = map[string]any{"name": operatortool.FileIssue, "connection_id": "audit-commit-" + key.ID, "arguments": map[string]any{"project_id": f.project, "title": title, "description": nativeContractTestBody, "state": "Todo", "request_id": fmt.Sprintf("audit-%d", i)}}
					}
					body, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					if i == 0 {
						switch failure {
						case "reject audit insert":
							operatorSQL(t, f.hostedSecurityFixture, `CREATE TRIGGER reject_key_audit BEFORE INSERT ON hosted_audit WHEN NEW.event='key_call' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
						case "reject audit update":
							operatorSQL(t, f.hostedSecurityFixture, `CREATE TRIGGER reject_key_audit BEFORE UPDATE ON hosted_audit WHEN NEW.event='key_call' AND NEW.status=200 BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
						}
					}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body)).WithContext(ctx)
					request.Header.Set("Content-Type", "application/json")
					claims := f.claims(nil, cloudassert.KindKey, http.MethodPost, request.URL.RequestURI(), body)
					claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "org_provider", "owner"
					assertion, err := cloudassert.Sign(f.key, claims)
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set(cloudassert.Header, assertion)
					recorder := httptest.NewRecorder()
					writer := &entryKeyAuditWriter{ResponseRecorder: recorder}
					if failure == "cancel response" {
						writer.beforeWrite = func() {
							var count int
							if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE session_id=? AND event='key_call' AND status=200", key.ID).Scan(&count); err != nil {
								t.Fatal(err)
							}
							if count != 1 {
								t.Errorf("mutation reached response before durable audit: count=%d", count)
							}
							cancel()
						}
					}
					if failure == "reject completed audit" {
						writer.beforeWrite = func() {
							operatorSQL(t, f.hostedSecurityFixture, `CREATE TRIGGER reject_key_audit BEFORE UPDATE ON hosted_audit WHEN NEW.event='key_call' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
						}
					}
					auditStartedAt := f.service.config.now().UnixNano()
					f.service.Handler().ServeHTTP(writer, request)
					var items, audits int
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE title=?", title).Scan(&items); err != nil {
						t.Fatal(err)
					}
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE session_id=? AND event='key_call' AND recorded_at_ns BETWEEN ? AND ?", key.ID, auditStartedAt, f.service.config.now().UnixNano()).Scan(&audits); err != nil {
						t.Fatal(err)
					}
					wantItems, wantAudits := 0, 1
					if failure == "cancel response" || failure == "reject completed audit" {
						wantItems = 1
						requireNativeStatus(t, recorder, http.StatusOK)
					} else {
						requireNativeStatus(t, recorder, http.StatusInternalServerError)
					}
					if failure == "reject audit insert" {
						wantAudits = 0
					}
					if items != wantItems || audits != wantAudits {
						t.Fatalf("items=%d audits=%d want items=%d audits=%d", items, audits, wantItems, wantAudits)
					}
					if !json.Valid(recorder.Body.Bytes()) {
						t.Fatalf("response is not one JSON document: %s", recorder.Body.String())
					}
					if failure == "reject completed audit" {
						if !strings.Contains(logs.String(), "Key audit is unavailable") {
							t.Fatal("post-handler audit failure was not logged")
						}
						for _, secret := range []string{title, nativeContractTestBody, assertion} {
							if strings.Contains(logs.String(), secret) {
								t.Fatal("audit failure log exposed request content")
							}
						}
						operatorSQL(t, f.hostedSecurityFixture, "DROP TRIGGER reject_key_audit")
						replay := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: target, body: string(body), mutate: func(claims *cloudassert.Claims) {
							claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "org_provider", "owner"
						}})
						requireNativeStatus(t, replay, http.StatusOK)
						if !json.Valid(replay.Body.Bytes()) {
							t.Fatalf("replay response is not one JSON document: %s", replay.Body.String())
						}
						if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE title=?", title).Scan(&items); err != nil || items != 1 {
							t.Fatalf("replay items=%d error=%v", items, err)
						}
					}
				}
			})
		}
	}
}

func TestEntryKeyAuditPreservesHandlerErrors(t *testing.T) {
	for _, test := range []struct {
		name      string
		committed bool
		err       error
		status    int
	}{
		{"uncommitted handler error", false, echo.NewHTTPError(http.StatusConflict, "Handler refused"), http.StatusConflict},
		{"committed handler error", true, echo.NewHTTPError(http.StatusConflict, "Handler refused"), http.StatusForbidden},
		{"uncommitted audit error", false, nil, http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs hostedLogBuffer
			f := newHostedSharedFixture(t, func(cfg *Config) {
				cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			})
			operatorSQL(t, f.hostedSecurityFixture, `CREATE TRIGGER reject_key_audit BEFORE UPDATE ON hosted_audit WHEN NEW.event='key_call' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
			request := httptest.NewRequest(http.MethodPost, f.base+"/work-items", nil)
			claims := f.claims(nil, cloudassert.KindKey, http.MethodPost, request.URL.RequestURI(), nil)
			claims.Key = &apikey.KeyAuthority{ID: "key_error", Kind: "service", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "all"}}
			claims.Subject, claims.ProviderOrganization, claims.Role = "service:key_error", "org_provider", "admin"
			assertion, err := cloudassert.Sign(f.key, claims)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set(cloudassert.Header, assertion)
			recorder := httptest.NewRecorder()
			c := echo.New().NewContext(request, recorder)
			result := f.service.hostedSharedEntry(func(c echo.Context) error {
				if test.committed {
					if err := c.JSON(http.StatusForbidden, apiErrorResponse{Code: "handler_refused", Message: "Handler refused"}); err != nil {
						return err
					}
				}
				return test.err
			})(c)
			if result != test.err {
				t.Fatalf("handler error=%v want=%v", result, test.err)
			}
			if result != nil {
				c.Error(result)
			}
			if recorder.Code != test.status || !json.Valid(recorder.Body.Bytes()) {
				t.Fatalf("response=%d %s want status=%d and one JSON document", recorder.Code, recorder.Body.String(), test.status)
			}
			if !strings.Contains(logs.String(), "Key audit is unavailable") {
				t.Fatal("audit failure was not logged")
			}
		})
	}
}
