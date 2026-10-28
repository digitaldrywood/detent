package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/labstack/echo/v4"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeIssueTypes(t *testing.T) {
	f := newNativeFixture(t, nil, "", "issue-types")
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/api/v2/organizations/:organization/issue-types-test", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "issue-types", "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/issue-types-test", f.token, map[string]any{})
	requireNativeStatus(t, response, http.StatusOK)
	ctx := <-contexts
	executor := hostedOperatorExecutor{f.service}
	call := func(name string, arguments map[string]any) operatortool.Result {
		t.Helper()
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		result, err := executor.Execute(ctx, operatortool.Call{Name: name, Arguments: raw})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	var typed tracker.NativeIssue
	for _, issueType := range []string{"", "bug", "change_request"} {
		t.Run("file/"+issueType, func(t *testing.T) {
			arguments := map[string]any{"project_id": f.project.ID, "request_id": "file-" + issueType, "title": "Issue classification", "description": nativeContractTestBody, "labels": []string{"bug", "enhancement"}}
			if issueType != "" {
				arguments["type"] = issueType
			}
			result := call(operatortool.FileIssue, arguments)
			var created operatortool.WorkReadResult[tracker.NativeIssue]
			if err := json.Unmarshal(result.Content, &created); err != nil {
				t.Fatal(err)
			}
			result = call(operatortool.WorkItem, map[string]any{"project_id": f.project.ID, "reference": created.Data.WorkItemID})
			var fetched operatortool.WorkReadResult[operatortool.NativeItem]
			if err := json.Unmarshal(result.Content, &fetched); err != nil {
				t.Fatal(err)
			}
			if fetched.Data.Type != issueType || !reflect.DeepEqual(fetched.Data.Labels, []string{"bug", "enhancement"}) {
				t.Fatalf("issue=%#v", fetched.Data)
			}
			if issueType == "bug" {
				typed = created.Data
			}
		})
	}
	drainTestNativeBoard(t, f.service)
	for _, issueType := range []string{"bug", "change_request"} {
		result := call(operatortool.WorkList, map[string]any{"project_id": f.project.ID, "type": issueType})
		var page operatortool.WorkReadResult[tracker.Page[operatortool.NativeItem]]
		if err := json.Unmarshal(result.Content, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Data.Items) != 1 || page.Data.Items[0].Type != issueType {
			t.Fatalf("filtered page=%s", result.Content)
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?include=board&type="+issueType, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var board tracker.NativeIssuePage
		decodeHubResponse(t, response, &board)
		if len(board.Items) != 1 || board.Items[0].Type != issueType {
			t.Fatalf("board-filtered page=%#v", board)
		}
	}
	result := call(operatortool.EditItem, map[string]any{"project_id": f.project.ID, "identifier": typed.WorkItemID, "request_id": "edit-type", "expected_revision": typed.Revision, "type": "change_request"})
	var edited operatortool.WorkReadResult[tracker.NativeIssue]
	if err := json.Unmarshal(result.Content, &edited); err != nil {
		t.Fatal(err)
	}
	if edited.Data.Type != "change_request" || !reflect.DeepEqual(edited.Data.Labels, typed.Labels) {
		t.Fatalf("edited=%s", result.Content)
	}
	path := f.base + "/work-items/" + string(typed.WorkItemID)
	var history tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", f.token, nil), &history)
	if !slices.ContainsFunc(history.Items, func(event tracker.CollaborationEvent) bool {
		return event.Type == "issue.edited" && event.Data.Revision == edited.Data.Revision && slices.Contains(event.Data.Fields, "type")
	}) {
		t.Fatalf("type edit missing from history: %#v", history)
	}
	for _, test := range []struct{ name, token string }{{"member", f.token}, {"worker", f.worker(t, "issue-types-worker")}} {
		for _, issueType := range []string{"", "bug", "change_request", "question"} {
			t.Run(test.name+"/"+issueType, func(t *testing.T) {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", test.token, tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: test.name + "-" + issueType}, Type: issueType, Title: "API classification"})
				if issueType == "question" {
					requireNativeStatus(t, response, http.StatusUnprocessableEntity)
					return
				}
				requireNativeStatus(t, response, http.StatusOK)
				var created tracker.NativeIssue
				decodeHubResponse(t, response, &created)
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(created.WorkItemID), test.token, nil), &created)
				if created.Type != issueType {
					t.Fatalf("type=%q, want %q", created.Type, issueType)
				}
			})
		}
	}
	for _, target := range []string{f.base + "/work-items?type=question", path} {
		method, body := http.MethodGet, any(nil)
		if target == path {
			method, body = http.MethodPatch, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "invalid-type"}, ExpectedRevision: edited.Data.Revision, Type: new("question")}
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, method, target, f.token, body), http.StatusUnprocessableEntity)
	}
}

func TestNativeIssueTypeMigration(t *testing.T) {
	f := newNativeFixture(t, nil, "", "issue-type-migration")
	first, second := f.create(t, "Existing bug label"), f.create(t, "Existing enhancement label")
	if _, err := f.service.database.db.ExecContext(t.Context(), "ALTER TABLE issues DROP COLUMN issue_type"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id    tracker.NativeWorkItemID
		label string
	}{{first.WorkItemID, "bug"}, {second.WorkItemID, "enhancement"}} {
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET labels_json=? WHERE native_id=?", "[\""+test.label+"\"]", test.id); err != nil {
			t.Fatal(err)
		}
		syncNativeIssueFixtureFacts(t, f.service.database.db, test.id)
	}
	data, err := migrationFiles.ReadFile("migrations/20261013150500_issue_type.sql")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, f.service.database.db, fstest.MapFS{"20261013150500_issue_type.sql": &fstest.MapFile{Data: data}}, goose.WithDisableGlobalRegistry(true), goose.WithTableName("issue_type_migration_test"), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		id    tracker.NativeWorkItemID
		label string
	}{{first.WorkItemID, "bug"}, {second.WorkItemID, "enhancement"}} {
		var stored sql.NullString
		if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT issue_type FROM issues WHERE native_id=?", test.id).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored.Valid {
			t.Fatalf("existing type=%v", stored)
		}
		page, err := f.service.readIssues(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, url.Values{"label": {test.label}})
		if err != nil || len(page.Items) != 1 || page.Items[0].WorkItemID != test.id || page.Items[0].Type != "" {
			t.Fatalf("existing issue page=%#v err=%v", page, err)
		}
	}
}

func TestNativeIssueReportContext(t *testing.T) {
	f := newNativeFixture(t, nil, "", "report-context")
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/api/v2/organizations/:organization/report-context-test", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "report-context", "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/report-context-test", f.token, map[string]any{}), http.StatusOK)
	ctx := <-contexts
	executor := hostedOperatorExecutor{f.service}
	full := &tracker.ReportContext{URL: "https://app.example.test/board?tab=1", AppBuild: "v1.4.2+abc123", Browser: "Firefox 131", OS: "Ubuntu 24.04", Viewport: "390x844", CapturedAt: "2026-10-13T12:00:00Z"}
	for _, test := range []struct {
		name    string
		context any
		want    *tracker.ReportContext
	}{
		{"absent", nil, nil},
		{"all fields", map[string]any{"url": full.URL, "app_build": full.AppBuild, "browser": full.Browser, "os": full.OS, "viewport": full.Viewport, "captured_at": full.CapturedAt}, full},
		{"trimmed partial", map[string]any{"browser": "  Safari 18 ", "url": ""}, &tracker.ReportContext{Browser: "Safari 18"}},
		{"blank", map[string]any{"os": "  "}, nil},
	} {
		t.Run("mcp/"+test.name, func(t *testing.T) {
			arguments := map[string]any{"description": nativeContractTestBody, "project_id": f.project.ID, "request_id": "context-" + test.name, "title": "Context " + test.name}
			if test.context != nil {
				arguments["report_context"] = test.context
			}
			raw, err := json.Marshal(arguments)
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.FileIssue, Arguments: raw})
			if err != nil {
				t.Fatal(err)
			}
			var created operatortool.WorkReadResult[tracker.NativeIssue]
			if err := json.Unmarshal(result.Content, &created); err != nil {
				t.Fatal(err)
			}
			raw, err = json.Marshal(map[string]any{"project_id": f.project.ID, "reference": created.Data.WorkItemID})
			if err != nil {
				t.Fatal(err)
			}
			result, err = executor.Execute(ctx, operatortool.Call{Name: operatortool.WorkItem, Arguments: raw})
			if err != nil {
				t.Fatal(err)
			}
			var fetched operatortool.WorkReadResult[operatortool.NativeItem]
			if err := json.Unmarshal(result.Content, &fetched); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fetched.Data.ReportContext, test.want) {
				t.Fatalf("work_item report_context=%+v, want %+v", fetched.Data.ReportContext, test.want)
			}
			if test.want == nil && strings.Contains(string(result.Content), "report_context") {
				t.Fatalf("issue without context exposes report_context: %s", result.Content)
			}
			for _, query := range []string{"?include=board", ""} {
				var page tracker.NativeIssuePage
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items"+query, f.token, nil), &page)
				for _, item := range page.Items {
					if item.ReportContext != nil {
						t.Fatalf("list %q carries report_context: %+v", query, item)
					}
				}
			}
		})
	}
	for _, test := range []struct {
		name   string
		body   string
		status int
		want   *tracker.ReportContext
	}{
		{"valid", `{"browser":"Edge 130","app_build":"v2"}`, http.StatusOK, &tracker.ReportContext{Browser: "Edge 130", AppBuild: "v2"}},
		{"unknown key", `{"browser":"Edge 130","cookie":"secret-cookie"}`, http.StatusUnprocessableEntity, nil},
		{"oversized", `{"app_build":"` + strings.Repeat("secret", 60) + `"}`, http.StatusUnprocessableEntity, nil},
		{"control character", `{"os":"secret\u0000os"}`, http.StatusUnprocessableEntity, nil},
		{"non-string", `{"viewport":["secret"]}`, http.StatusUnprocessableEntity, nil},
	} {
		t.Run("rest/"+test.name, func(t *testing.T) {
			body := json.RawMessage(`{"idempotency_key":"rest-context-` + strings.ReplaceAll(test.name, " ", "-") + `","title":"REST context ` + test.name + `","body":` + strconv.Quote(nativeContractTestBody) + `,"state":"","labels":[],"assignees":[],"report_context":` + test.body + `}`)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, body)
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				var failure apiErrorResponse
				decodeHubResponse(t, response, &failure)
				if failure.Code != "invalid_request" || strings.Contains(failure.Message, "secret") {
					t.Fatalf("failure=%#v", failure)
				}
				return
			}
			var created tracker.NativeIssue
			decodeHubResponse(t, response, &created)
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(created.WorkItemID), f.token, nil), &created)
			if !reflect.DeepEqual(created.ReportContext, test.want) {
				t.Fatalf("report_context=%+v, want %+v", created.ReportContext, test.want)
			}
		})
	}
}
