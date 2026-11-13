package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeIssueTypes(t *testing.T) {
	t.Parallel()
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
	actors := []struct{ name, token string }{{"member", f.token}, {"worker", f.worker(t, "issue-types-worker")}, {"other-worker", f.worker(t, "issue-types-other-worker")}}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.States = append(project.States, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}})
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range actors {
		var lease leasestore.Lease
		if test.name != "member" {
			item := f.create(t, "Classification source "+test.name)
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				credential, err := w.CredentialByHash(t.Context(), apikey.HashToken(test.token))
				if err != nil {
					return err
				}
				runner, err := w.RunnerIdentityByCredential(t.Context(), credential.ID)
				if err != nil {
					return err
				}
				row, err := w.WorkItem(t.Context(), f.project.ID, item.WorkItemID)
				if err != nil {
					return err
				}
				lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: string(runner.MachineID), SessionID: test.name + "-session", LeaseID: hubapi.NewID("lease"), TTL: time.Minute})
				if err != nil {
					return err
				}
				if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: f.project.OrganizationID, LeaseID: lease.LeaseID, RunnerID: runner.RunnerID, AssignedAt: lease.AcquiredAt}); err != nil {
					return err
				}
				return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: hubTestPolicy().ID, BoundAt: lease.AcquiredAt})
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, issueType := range []string{"", "bug", "change_request", "question"} {
			t.Run(test.name+"/"+issueType, func(t *testing.T) {
				request := tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: test.name + "-" + issueType}, Type: issueType, Title: "API classification"}
				if lease.LeaseID != "" {
					request.Body = issueorigin.Stamp(request.Body, issueorigin.Origin{Kind: "worker", Fingerprint: test.name + "-" + issueType})
					request.State = "Backlog"
					request.LeaseID, request.FencingToken = tracker.LeaseID(lease.LeaseID), tracker.FencingToken(lease.FencingToken)
				}
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", test.token, request)
				if issueType == "question" {
					requireNativeStatus(t, response, http.StatusUnprocessableEntity)
					return
				}
				requireNativeStatus(t, response, http.StatusOK)
				var created tracker.NativeIssue
				decodeHubResponse(t, response, &created)
				response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(created.WorkItemID), test.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &created)
				if created.Type != issueType {
					t.Fatalf("type=%q, want %q", created.Type, issueType)
				}
			})
		}
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "ungranted-worker-project")
	for _, test := range actors[1:] {
		t.Run(test.name+"/project isolation", func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/work-items", test.token, nil), http.StatusNotFound)
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items", actors[1].token+"invalid", nil), http.StatusUnauthorized)
	for _, target := range []string{f.base + "/work-items?type=question", path} {
		method, body := http.MethodGet, any(nil)
		if target == path {
			method, body = http.MethodPatch, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "invalid-type"}, ExpectedRevision: edited.Data.Revision, Type: new("question")}
		}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, method, target, f.token, body), http.StatusUnprocessableEntity)
	}
}

func TestNativeIssueTypeMigration(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "issue-type-migration")
	fixture := coreWorkItemNeutralFixture(t, f.project.OrganizationID, f.project.ID, false)
	cases := []struct {
		id    tracker.NativeWorkItemID
		label string
	}{{"wi_core_import", "bug"}, {"wi_core_import_second", "enhancement"}}
	for _, test := range cases {
		labels, err := json.Marshal([]string{test.label})
		if err != nil {
			t.Fatal(err)
		}
		fixture.set("issues", map[string]string{"native_id": string(test.id)}, map[string]any{"issue_type": nil, "labels_json": string(labels)})
		var base, patch []byte
		for _, row := range fixture.records["collaboration_versions"] {
			var id, raw string
			var revision int64
			if err := row.Decode(map[string]any{"record_id": &id, "revision": &revision, "record_json": &raw}); err != nil {
				t.Fatal(err)
			}
			if id != string(test.id) {
				continue
			}
			if revision == 1 {
				base = []byte(raw)
			} else {
				patch = []byte(raw)
			}
		}
		current, err := applyNativeVersionPatch(base, patch)
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []*[]byte{&base, &current} {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(*raw, &fields); err != nil {
				t.Fatal(err)
			}
			fields["labels"], fields["type"] = labels, json.RawMessage("null")
			*raw, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
		}
		fixture.set("collaboration_versions", map[string]string{"record_id": string(test.id), "revision": "1"}, map[string]any{"encoding": "snapshot", "record_json": string(base)})
		patch, encoding, err := encodeNativeVersion(base, "snapshot", current, 2)
		if err != nil {
			t.Fatal(err)
		}
		fixture.set("collaboration_versions", map[string]string{"record_id": string(test.id), "revision": "2"}, map[string]any{"encoding": encoding, "record_json": string(patch)})
	}
	source := CoreWorkItemImportSource{BundlePath: fixture.write(), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, Objects: f.service.runDetailStore()}
	if count, err := ImportCoreWorkItems(t.Context(), f.service.config.TenantRepository.(*postgres.Repository), source); err != nil || count != 2 {
		t.Fatalf("historical issue import count=%d error=%v", count, err)
	}
	drainTestNativeBoard(t, f.service)
	for _, test := range cases {
		var stored corestore.WorkItem
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			stored, err = r.WorkItem(t.Context(), f.project.ID, test.id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if stored.IssueType != "" {
			t.Fatalf("existing type=%v", stored.IssueType)
		}
		page, err := f.service.readIssues(t.Context(), nativeBoardFixtureScope(t, f), url.Values{"label": {test.label}})
		if err != nil || len(page.Items) != 1 || page.Items[0].WorkItemID != test.id || page.Items[0].Type != "" {
			t.Fatalf("existing issue page=%#v err=%v", page, err)
		}
	}
}

func TestNativeIssueReportContext(t *testing.T) {
	t.Parallel()
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
				var failure hubapi.APIErrorResponse
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
