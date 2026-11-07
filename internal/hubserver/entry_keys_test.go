package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
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
						f := newEntryKeyFixture(t)
						user := f.user(t, role)
						if !member {
							f.exec(t, "UPDATE hosted_members SET active=false WHERE organization_id='org_security' AND user_id=$1", user.identity.Subject)
						}
						projects := []string(nil)
						if access != "all" {
							projects = []string{string(f.project)}
						}
						key := apikey.KeyAuthority{ID: "key_matrix", Kind: kind, Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: access, Projects: projects}}
						mutate := func(claims *cloudassert.Claims) {
							claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "provider_org_security", role
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
							wantWrite = http.StatusForbidden
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
							f.exec(t, "UPDATE projects SET deleted_at='2026-10-09T00:00:00Z' WHERE organization_id='org_security' AND id=$1", f.project)
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
							if denied.Code != http.StatusNotFound {
								t.Fatalf("deleted project read=%d %s", denied.Code, denied.Body.String())
							}
							visible, err = f.service.hostedReadableProjects(t.Context(), credential)
							if err != nil || len(visible) != 0 {
								t.Fatalf("deleted key projects=%v error=%v", visible, err)
							}
							f.exec(t, "UPDATE projects SET deleted_at=NULL WHERE organization_id='org_security' AND id=$1", f.project)
						}
						if kind == "personal" && member {
							f.exec(t, "DELETE FROM hosted_project_grants WHERE organization_id='org_security' AND user_id=$1", user.identity.Subject)
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
							if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "access_denied") {
								t.Fatalf("removed grant=%d %s", denied.Code, denied.Body.String())
							}
						}
						if access != "all" {
							key.Projects = []string{"prj_other"}
							denied := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, target: f.base + "/work-items", mutate: mutate})
							if kind == "service" || member {
								if denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "access_denied") {
									t.Fatalf("scope refusal=%d %s", denied.Code, denied.Body.String())
								}
							}
						}
						f.assertOtherTenant(t)
					})
				}
			}
		}
	}
}

func TestPostgresNativeSupportReadAudit(t *testing.T) {
	f := newEntryKeyFixture(t, func(cfg *Config) {
		cfg.runMaintenance = func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
			<-ctx.Done()
			return ctx.Err()
		}
	})
	user := f.user(t, "owner")
	for _, organization := range []string{"org_security", "org_other"} {
		t.Run(organization, func(t *testing.T) {
			service := f.service
			if organization != service.config.Hosted.OrganizationID {
				cfg := startupConfig(startupConnection(t, f.admin, "detent_runtime_v2", ""))
				hosted := *f.service.config.Hosted
				entry := *hosted.SharedEntry
				hosted.OrganizationID, hosted.WorkOSOrganizationID = organization, "provider_"+organization
				hosted.SharedEntry = &entry
				cfg.Hosted, cfg.SecretKeys = &hosted, f.service.config.SecretKeys
				cfg.RunDetails = f.service.config.RunDetails
				cfg.runMaintenance = f.service.config.runMaintenance
				var err error
				service, err = Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := service.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			service.echo.HEAD(nativeBase, service.getNativeProject, service.requireNativeScope(apiScopeWorker, apiScopeOperator))
			client := f.hostedSharedFixture
			client.service = service
			target := "/api/v2/organizations/" + organization + "/projects/" + string(f.project)
			key := apikey.KeyAuthority{ID: "key_read", Kind: "personal", Permission: apikey.ScopeRead, ProjectContext: apikey.ProjectContext{Access: "all"}}
			routes := []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodHead, ""}, {http.MethodGet, "/integration"}, {http.MethodGet, "/onboarding"}}
			read := func(kind, method, suffix string) *httptest.ResponseRecorder {
				t.Helper()
				return client.serve(t, hostedSharedRequest{kind: kind, method: method, target: target + suffix, mutate: func(claims *cloudassert.Claims) {
					claims.Audience, claims.Subject, claims.ProviderOrganization, claims.Role = organization, user.identity.Subject, "provider_"+organization, "owner"
					if kind == cloudassert.KindKey {
						claims.Key = &key
						return
					}
					now := service.config.now()
					claims.Email, claims.ProviderSession = user.identity.Email, "session_support"
					claims.SessionCreatedAt, claims.SessionExpiresAt, claims.AccessExpiresAt = now, now.Add(time.Hour), now.Add(time.Minute)
					claims.Binding = cloudassert.AuthorizationBinding("support", organization, claims.ProviderSession)
					claims.CSRF = cloudassert.CSRFToken("support", organization)
					claims.SupportActor, claims.SupportReason = "support@example.test", "customer-request"
				}})
			}
			for _, route := range routes {
				method, suffix := route.method, route.suffix
				t.Run(method+suffix, func(t *testing.T) {
					for range 3 {
						requireNativeStatus(t, read(cloudassert.KindKey, method, suffix), http.StatusOK)
						requireNativeStatus(t, read(cloudassert.KindBrowser, method, suffix), http.StatusOK)
					}
					var ordinary, support, attributed int
					if err := f.admin.QueryRowContext(t.Context(), `SELECT
						count(*) FILTER (WHERE session_id=$2 AND event IN ('action','key_call')),
						count(*) FILTER (WHERE session_id='session_support' AND event='action' AND route=$3),
						count(*) FILTER (WHERE session_id='session_support' AND event='action' AND route=$3 AND actual_actor='support@example.test' AND effective_user=$4 AND reason='customer-request' AND project_id=$5)
						FROM hosted_audit WHERE organization_id=$1`, organization, key.ID, method+" "+nativeBase+suffix, user.identity.Subject, f.project).Scan(&ordinary, &support, &attributed); err != nil {
						t.Fatal(err)
					}
					if ordinary != 0 || support != 3 || attributed != 3 {
						t.Errorf("ordinary audits=%d support=%d attributed=%d, want 0/3/3", ordinary, support, attributed)
					}
				})
			}

			tenant := hubmaintenance.Tenant{Organization: organization, Generation: service.config.Hosted.SharedEntry.Generation}
			for range 4 {
				ctx := (hubmaintenance.Budget{Items: 1, Bytes: integrationPageMaintenanceBytes}).Context(t.Context(), tenant)
				if err := service.OnboardingPageMaintenanceFeature().Step(ctx, tenant, hubmaintenance.Budget{}); err != nil {
					t.Fatal(err)
				}
			}
			var viewers, pages int
			if err := f.admin.QueryRowContext(t.Context(), `SELECT (SELECT count(*) FROM project_onboarding_page_viewers WHERE organization_id=$1 AND project_id=$2),(SELECT count(*) FROM project_onboarding_pages WHERE organization_id=$1 AND project_id=$2)`, organization, f.project).Scan(&viewers, &pages); err != nil {
				t.Fatal(err)
			}
			if viewers != 2 || pages != 2 {
				t.Errorf("hosted onboarding viewers=%d ready=%d want=2/2", viewers, pages)
			}
			f.exec(t, `CREATE OR REPLACE FUNCTION reject_support_audit_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'audit unavailable'; END $$`)
			f.exec(t, "CREATE TRIGGER reject_support_audit BEFORE INSERT ON hosted_audit FOR EACH ROW WHEN (NEW.organization_id='"+organization+"' AND NEW.event='action') EXECUTE FUNCTION reject_support_audit_write()")
			for _, route := range routes {
				method, suffix := route.method, route.suffix
				t.Run("audit unavailable/"+method+suffix, func(t *testing.T) {
					requireNativeStatus(t, read(cloudassert.KindKey, method, suffix), http.StatusOK)
					refused := read(cloudassert.KindBrowser, method, suffix)
					if refused.Code != http.StatusInternalServerError {
						t.Errorf("support read=%d, want %d", refused.Code, http.StatusInternalServerError)
					}
				})
			}
			f.exec(t, "DROP TRIGGER reject_support_audit ON hosted_audit")
		})
	}
}

func TestEntryKeyCallReportsMoveItemRequestErrors(t *testing.T) {
	f := newEntryKeyFixture(t)
	user := f.user(t, "owner")
	key := apikey.KeyAuthority{ID: "key_move", Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "all"}}
	mutate := func(claims *cloudassert.Claims) {
		claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "provider_org_security", "owner"
	}
	created := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: f.base + "/work-items", body: "{\"body\":\"\\n## Acceptance criteria\\nComplete the requested work.\\n## Must not break\\nExisting behavior.\\n## How we know it worked\\nRun the project checks.\",\"idempotency_key\":\"move-errors\",\"state\":\"Todo\",\"title\":\"Move error reporting\"}", mutate: mutate})
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	auditCount := func() int {
		t.Helper()
		var count int
		if err := f.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id='org_security' AND session_id=$1", key.ID).Scan(&count); err != nil {
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
		var events string
		if err := f.admin.QueryRowContext(t.Context(), "SELECT string_agg(event || ':' || route, ', ' ORDER BY id) FROM hosted_audit WHERE organization_id='org_security' AND session_id=$1", key.ID).Scan(&events); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("successful reads wrote audit rows: before=%d after=%d events=%s", beforeRead, auditCount(), events)
	}
	f.exec(t, "UPDATE hosted_members SET active=false WHERE organization_id='org_security' AND user_id=$1", user.identity.Subject)
	beforeCatalog := auditCount()
	requireNativeStatus(t, f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: "/internal/v1/keys/catalog", mutate: mutate}), http.StatusForbidden)
	if auditCount() != beforeCatalog+1 {
		t.Fatalf("catalog refusal wrote %d audit rows", auditCount()-beforeCatalog)
	}
	var catalogOperation string
	var catalogStatus int
	if err := f.admin.QueryRowContext(t.Context(), "SELECT route,status FROM hosted_audit WHERE organization_id='org_security' AND session_id=$1 ORDER BY id DESC LIMIT 1", key.ID).Scan(&catalogOperation, &catalogStatus); err != nil {
		t.Fatal(err)
	}
	if catalogOperation != "POST /internal/v1/keys/catalog" || catalogStatus != http.StatusForbidden {
		t.Fatalf("catalog audit operation=%s status=%d", catalogOperation, catalogStatus)
	}
	f.exec(t, "UPDATE hosted_members SET active=true WHERE organization_id='org_security' AND user_id=$1", user.identity.Subject)
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
	if err := f.admin.QueryRowContext(t.Context(), "SELECT route,resource,status FROM hosted_audit WHERE organization_id='org_security' AND session_id=$1 ORDER BY id DESC LIMIT 1", key.ID).Scan(&operation, &resource, &status); err != nil {
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
				f := newEntryKeyFixture(t, func(cfg *Config) {
					cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
				})
				user := f.user(t, "owner")
				f.exec(t, "UPDATE project_settings SET duplicate_challenge=false WHERE organization_id='org_security' AND project_id=$1", f.project)
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
							f.rejectAudit(t, "INSERT", "")
						case "reject audit update":
							f.rejectAudit(t, "UPDATE", " AND NEW.status=200")
						}
					}
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					request := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body)).WithContext(ctx)
					request.Header.Set("Content-Type", "application/json")
					claims := f.claims(nil, cloudassert.KindKey, http.MethodPost, request.URL.RequestURI(), body)
					claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "provider_org_security", "owner"
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
							if err := f.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id='org_security' AND session_id=$1 AND event='key_call' AND status=200", key.ID).Scan(&count); err != nil {
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
							f.rejectAudit(t, "UPDATE", "")
						}
					}
					auditStartedAt := f.service.config.now().UnixNano()
					f.service.Handler().ServeHTTP(writer, request)
					var items, audits int
					if err := f.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id='org_security' AND title=$1", title).Scan(&items); err != nil {
						t.Fatal(err)
					}
					if err := f.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id='org_security' AND session_id=$1 AND event='key_call' AND recorded_at_ns BETWEEN $2 AND $3", key.ID, auditStartedAt, f.service.config.now().UnixNano()).Scan(&audits); err != nil {
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
					var successfulAudits int
					if err := f.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id='org_security' AND session_id=$1 AND event='key_call' AND status=200", key.ID).Scan(&successfulAudits); err != nil {
						t.Fatal(err)
					}
					if successfulAudits != wantItems {
						t.Fatalf("successful audits=%d want=%d", successfulAudits, wantItems)
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
						f.exec(t, "DROP TRIGGER reject_key_audit ON hosted_audit")
						replay := f.serve(t, hostedSharedRequest{kind: cloudassert.KindKey, method: http.MethodPost, target: target, body: string(body), mutate: func(claims *cloudassert.Claims) {
							claims.Key, claims.Subject, claims.ProviderOrganization, claims.Role = &key, user.identity.Subject, "provider_org_security", "owner"
						}})
						requireNativeStatus(t, replay, http.StatusOK)
						if !json.Valid(replay.Body.Bytes()) {
							t.Fatalf("replay response is not one JSON document: %s", replay.Body.String())
						}
						if err := f.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE organization_id='org_security' AND title=$1", title).Scan(&items); err != nil || items != 1 {
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
			f := newEntryKeyFixture(t, func(cfg *Config) {
				cfg.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			})
			f.rejectAudit(t, "UPDATE", "")
			request := httptest.NewRequest(http.MethodPost, f.base+"/work-items", nil)
			claims := f.claims(nil, cloudassert.KindKey, http.MethodPost, request.URL.RequestURI(), nil)
			claims.Key = &apikey.KeyAuthority{ID: "key_error", Kind: "service", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "all"}}
			claims.Subject, claims.ProviderOrganization, claims.Role = "service:key_error", "provider_org_security", "admin"
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
			if !errors.Is(result, test.err) {
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
