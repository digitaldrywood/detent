package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestOperatorNativeWorkReadErrors(t *testing.T) {
	var logs hostedLogBuffer
	f := newDefaultNativeFixture(t, Config{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	fault := f.create(t, "fault item")
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-errors")
	hidden := foreign.create(t, "hidden content")
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]any{"name": "Other error tenant"})
	requireNativeStatus(t, response, http.StatusCreated)
	var organization nativeOrganization
	decodeHubResponse(t, response, &organization)
	otherTenant := newNativeFixture(t, f.service, organization.ID, "other-error-tenant")
	otherItem := otherTenant.create(t, "other tenant content")
	credential, _, err := f.service.authenticateAPIToken(t.Context(), foreign.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/tokens/"+credential.ID+"/grants", testHubAdminToken, map[string]any{"organization_id": f.project.OrganizationID, "project_id": f.project.ID})
	requireNativeStatus(t, response, http.StatusNoContent)
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/api/v2/organizations/:organization/operator-error-fixture", func(c echo.Context) error {
		contexts <- c.Request().Context()
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	capture := func(token string) context.Context {
		response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/operator-error-fixture", token, map[string]any{})
		requireNativeStatus(t, response, http.StatusOK)
		ctx := <-contexts
		return operatortool.BindConnection(ctx, operatortool.ConnectionIdentity(ctx).PrincipalID, "test-client")
	}
	ctx := capture(f.token)
	broader := capture(foreign.token)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET actor_json='[]' WHERE native_id=?", fault.WorkItemID); err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			for _, test := range []struct {
				name, reference, want string
				broader               bool
			}{
				{"missing", "wi_missing", "work item not found in this project", false},
				{"unauthorized project", string(hidden.WorkItemID), operatortool.ErrProjectScopeRequired.Error(), false},
				{"wrong authorized project", string(hidden.WorkItemID), "work item not found in this project", true},
				{"other organization", string(otherItem.WorkItemID), "work item not found in this project", false},
				{"server fault", string(fault.WorkItemID), "Operator tool is unavailable", false},
			} {
				t.Run(test.name, func(t *testing.T) {
					callCtx := ctx
					if test.broader {
						callCtx = broader
					}
					call := hostedContextProtocol(t, f.service, callCtx, transport)
					for _, tool := range []string{operatortool.WorkItem, operatortool.WorkRuns, operatortool.WorkHistory, operatortool.WorkComments, operatortool.WorkRelationships, operatortool.ExplainItem, operatortool.BoardActivity, operatortool.ListComments} {
						t.Run(tool, func(t *testing.T) {
							before := logs.String()
							args := map[string]any{"project_id": string(f.project.ID), "reference": test.reference}
							if tool == operatortool.ListComments {
								delete(args, "reference")
								args["identifier"] = test.reference
							}
							reply := call("tools/call", tool, args)
							var result struct {
								IsError bool `json:"isError"`
								Content []struct {
									Text string `json:"text"`
								} `json:"content"`
							}
							if err := json.Unmarshal(reply.Result, &result); err != nil {
								t.Fatal(err)
							}
							if !result.IsError || len(result.Content) != 1 || test.name != "server fault" && result.Content[0].Text != test.want {
								t.Fatalf("tool error=%s %s, want %q", reply.Result, reply.Error, test.want)
							}
							var faults []json.RawMessage
							for line := range strings.SplitSeq(strings.TrimPrefix(logs.String(), before), "\n") {
								if line == "" {
									continue
								}
								var record struct {
									Message string `json:"msg"`
								}
								if err := json.Unmarshal([]byte(line), &record); err != nil {
									t.Fatal(err)
								}
								if record.Message == "operator tool failed" {
									faults = append(faults, json.RawMessage(line))
								}
							}
							if test.name == "server fault" {
								if len(faults) != 1 {
									t.Fatalf("expected one tool fault: %s", faults)
								}
								var entry struct {
									Tool          string `json:"tool"`
									CorrelationID string `json:"correlation_id"`
									Error         string `json:"error"`
								}
								if err := json.Unmarshal(faults[0], &entry); err != nil {
									t.Fatalf("fault log=%s: %v", faults[0], err)
								}
								if entry.Tool != tool || entry.CorrelationID == "" || !strings.Contains(entry.Error, "cannot unmarshal array") {
									t.Fatalf("lost fault context: %s", faults[0])
								}
								if want := test.want + " (reason_code service_unavailable, correlation_id " + entry.CorrelationID + ")"; result.Content[0].Text != want {
									t.Fatalf("tool error text = %q, want %q", result.Content[0].Text, want)
								}
							} else if len(faults) != 0 {
								t.Fatalf("expected read failure logged as server fault: %s", faults)
							}
						})
					}
				})
			}
		})
	}
}

func TestOperatorNativeWorkListBytePages(t *testing.T) {
	f := newNativeFixture(t, nil, "", "bounded-list")
	const needle = "body-only-list-match"
	expected := make([]tracker.NativeIssue, 0, 61)
	for i := range 61 {
		body := needle
		if i < 60 {
			unit := "x"
			if i%2 != 0 {
				unit = "<\"\\"
			}
			body = strings.Repeat(unit, ((256<<10)-len(needle)-len(issueContractTestSections))/len(unit))
			body += strings.Repeat("x", (256<<10)-len(body)-len(needle)-len(issueContractTestSections)) + needle
		}
		request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("large-%d", i)}, Title: strings.Repeat("<", 492) + fmt.Sprintf("%08d", i), Body: body + issueContractTestSections, State: "Todo", Labels: []string{"bounded"}}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		expected = append(expected, issue)
	}
	unmatched := f.create(t, "unmatched body and label")
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-list")
	hidden := foreign.create(t, "hidden-list-item")
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(expected[0].WorkItemID)+"/dependencies", f.token,
		tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "list-dependency"}, ExpectedRevision: expected[0].Revision, RelatedWorkItemID: expected[1].WorkItemID, Operation: "add"})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &expected[0])
	detailItem := expected[60]
	expectedByID := map[tracker.NativeWorkItemID]tracker.NativeIssue{}
	for _, item := range expected {
		expectedByID[item.WorkItemID] = item
	}

	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	var firstCursor string
	var baseline []byte
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			call := hostedContextProtocol(t, f.service, ctx, transport)
			for _, limit := range []int{1, 100, 200} {
				t.Run(strconv.Itoa(limit), func(t *testing.T) {
					args := map[string]any{"project_id": string(f.project.ID), "query": needle, "state": "Todo", "label": "bounded", "limit": limit}
					seen, pages := 0, 0
					seenIDs := map[tracker.NativeWorkItemID]bool{}
					for {
						reply := call("tools/call", operatortool.WorkList, args)
						if len(reply.Result) > mcp.MaxHTTPResponseBytes || strings.Contains(string(reply.Result), f.token) || strings.Contains(string(reply.Result), string(hidden.WorkItemID)) || strings.Contains(string(reply.Result), string(unmatched.WorkItemID)) {
							t.Fatal("unbounded or unauthorized list result")
						}
						raw := hostedContextData(t, reply, false)
						if len(raw) > operatortool.MaxResultBytes {
							t.Fatalf("work result bytes=%d", len(raw))
						}
						var result operatortool.WorkReadResult[tracker.Page[operatortool.NativeItem]]
						if err := json.Unmarshal(raw, &result); err != nil {
							t.Fatal(err)
						}
						page := result.Data
						if len(page.Items) == 0 || len(page.Items) > limit || result.Freshness != explain.SourceAvailable || result.ProjectID != string(f.project.ID) {
							t.Fatalf("invalid page count=%d freshness=%s", len(page.Items), result.Freshness)
						}
						if pages == 0 && limit > 1 {
							if page.NextCursor == "" || len(page.Items) >= len(expected) {
								t.Fatal("large list has no genuine byte continuation")
							}
							firstCursor = page.NextCursor
							shape, err := json.Marshal(page.Items)
							if err != nil {
								t.Fatal(err)
							}
							if baseline == nil {
								baseline = shape
								t.Logf("bounded first page: items=%d result_bytes=%d protocol_result_bytes=%d", len(page.Items), len(raw), len(reply.Result))
							} else if string(shape) != string(baseline) {
								t.Fatal("limit or transport changed the bounded shape")
							}
						}
						for _, item := range page.Items {
							if seen >= len(expected) {
								t.Fatal("duplicate or extra item")
							}
							want, exists := expectedByID[item.WorkItemID]
							if !exists || seenIDs[item.WorkItemID] {
								t.Fatal("duplicate or unexpected search hit")
							}
							seenIDs[item.WorkItemID] = true
							if item.Snippet == "" || len([]rune(item.Snippet)) > 240 {
								t.Fatal("missing or unbounded snippet")
							}
							if item.WorkItemID != want.WorkItemID || item.Revision != want.Revision || item.Title != want.Title || item.Identifier == "" || item.URL == "" || !reflect.DeepEqual(item.Dependencies, want.Dependencies) || !reflect.DeepEqual(item.Blockers, want.Blockers) || item.Body != "" || !slices.Contains(item.OmittedFields, "body") || item.LinkedSource != nil {
								t.Fatalf("lost summary metadata or omission at item %d", seen)
							}
							seen++
						}
						pages++
						if page.NextCursor == "" {
							break
						}
						if pages > len(expected) {
							t.Fatal("cursor did not advance")
						}
						args["cursor"] = page.NextCursor
					}
					if seen != len(expected) {
						t.Fatalf("lost items: got %d want %d", seen, len(expected))
					}
				})
			}
			itemArgs := map[string]any{"project_id": string(f.project.ID), "reference": string(detailItem.WorkItemID)}
			for _, tool := range []string{operatortool.WorkItem, operatortool.WorkExport} {
				data := hostedContextData(t, call("tools/call", tool, itemArgs), false)
				var result operatortool.WorkReadResult[tracker.NativeIssue]
				if err := json.Unmarshal(data, &result); err != nil || result.Data.Body != needle+issueContractTestSections || len(result.Data.OmittedFields) != 0 {
					t.Fatalf("%s lost full detail", tool)
				}
			}
			for _, changes := range []map[string]any{
				{"state": "Done"}, {"label": "other"}, {"query": "other"}, {"created_by": "other"},
				{"project_id": string(foreign.project.ID)}, {"cursor": firstCursor + "invalid"},
			} {
				args := map[string]any{"project_id": string(f.project.ID), "query": needle, "state": "Todo", "label": "bounded", "limit": 200, "cursor": firstCursor}
				for key, value := range changes {
					args[key] = value
				}
				hostedContextData(t, call("tools/call", operatortool.WorkList, args), true)
			}
			credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
			if err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
			params := url.Values{"q": {needle}, "state": {"Todo"}, "label": {"bounded"}, "cursor": {firstCursor}}
			_, cursor, key, err := f.service.readNativePage(t.Context(), scope, f.base+"/work-items", params)
			if err != nil {
				t.Fatal(err)
			}
			cursor.Expires = f.service.config.now().Add(-time.Second).Unix()
			expired, err := encodeNativeCursor(cursor, key)
			if err != nil {
				t.Fatal(err)
			}
			hostedContextData(t, call("tools/call", operatortool.WorkList, map[string]any{"project_id": string(f.project.ID), "query": needle, "state": "Todo", "label": "bounded", "cursor": expired}), true)
		})
	}

	t.Run("bulk byte pages", func(t *testing.T) {
		executor := hostedOperatorExecutor{service: f.service}
		args := map[string]any{"project_id": string(f.project.ID), "labels": []string{"bounded"}, "limit": 500}
		seen := 0
		seenIDs := map[tracker.NativeWorkItemID]bool{}
		pages := 0
		for {
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.WorkBulkExport, Arguments: raw})
			if err != nil {
				t.Fatal(err)
			}
			var page operatortool.WorkBulkExportPage
			if err := json.Unmarshal(result.Content, &page); err != nil {
				t.Fatal(err)
			}
			if len(result.Content) > operatortool.WorkBulkExportMaxBytes || len(page.Items) == 0 || page.Total != len(expected) || page.Truncated != (page.NextCursor != "") {
				t.Fatal("invalid byte-bounded export page")
			}
			for _, item := range page.Items {
				want, exists := expectedByID[item.WorkItemID]
				if !exists || seenIDs[item.WorkItemID] || item.Body != want.Body || len(item.OmittedFields) != 0 {
					t.Fatalf("lost full content or duplicate item at %s", item.WorkItemID)
				}
				seenIDs[item.WorkItemID] = true
				seen++
			}
			pages++
			if page.NextCursor == "" {
				break
			}
			if pages > len(expected) {
				t.Fatal("bulk cursor failed to advance")
			}
			if pages == 1 {
				for _, changes := range []map[string]any{{"states": []string{"Done"}}, {"labels": []string{"other"}}, {"archived": "all"}, {"project_id": string(foreign.project.ID)}, {"cursor": page.NextCursor + "invalid"}} {
					changed := map[string]any{"project_id": string(f.project.ID), "labels": []string{"bounded"}, "cursor": page.NextCursor}
					for key, value := range changes {
						changed[key] = value
					}
					arguments, err := json.Marshal(changed)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.WorkBulkExport, Arguments: arguments}); err == nil {
						t.Fatal("bulk cursor accepted changed scope")
					}
				}
			}
			args["cursor"] = page.NextCursor
		}
		if seen != len(expected) || pages < 2 {
			t.Fatalf("exported %d in %d byte pages", seen, pages)
		}
	})
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?limit=1&state=Todo&label=bounded&q="+url.QueryEscape(needle), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var original tracker.Page[tracker.NativeIssue]
	decodeHubResponse(t, response, &original)
	if original.Items[0].Body != expectedByID[original.Items[0].WorkItemID].Body || len(original.Items[0].OmittedFields) != 0 {
		t.Fatal("original list resource lost its complete body")
	}
	executor := hostedOperatorExecutor{service: f.service}
	args := operatortool.WorkReadRequest{ProjectID: string(f.project.ID), Query: needle, State: "Todo", Label: "bounded", Limit: 1, Cursor: original.NextCursor}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.WorkList, Arguments: raw})
	if err != nil {
		t.Fatal(err)
	}
	restContinuation := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+args.NativeWorkQuery().Encode(), f.token, nil)
	requireNativeStatus(t, restContinuation, http.StatusOK)
	var restPage tracker.NativeIssuePage
	decodeHubResponse(t, restContinuation, &restPage)
	var compatible operatortool.WorkReadResult[tracker.Page[operatortool.NativeItem]]
	if err := json.Unmarshal(result.Content, &compatible); err != nil || len(compatible.Data.Items) != 1 || compatible.Data.Items[0].WorkItemID != restPage.Items[0].WorkItemID {
		t.Fatal("original list cursor is incompatible with summary projection")
	}
	credential := operatortool.ConnectionIdentity(ctx).PrincipalID
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=? WHERE id=?", formatHubTime(f.service.config.now()), credential); err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"stdio", "http"} {
		t.Run("revoked/"+transport, func(t *testing.T) {
			call := hostedContextProtocol(t, f.service, ctx, transport)
			hostedContextData(t, call("tools/call", operatortool.WorkList, map[string]any{"project_id": string(f.project.ID), "limit": 200}), true)
		})
	}
}

func TestOperatorNativeWorkReads(t *testing.T) {
	// Real application writes populate the reads; catches bypassing native scope,
	// losing cursor pages, dropping reads from the combined command dispatcher,
	// and leaking hidden relationship IDs in saved versions.
	f := newNativeFixture(t, nil, "", "read-tools")
	first := f.create(t, "needle first")
	second := f.create(t, "needle second")
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign")
	hidden := foreign.create(t, "hidden issue")
	itemPath := f.base + "/work-items/" + string(first.WorkItemID)
	response := performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/dependencies", testHubAdminToken, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "hidden-dependency"}, ExpectedRevision: first.Revision, RelatedWorkItemID: hidden.WorkItemID, Operation: "add"})
	requireNativeStatus(t, response, http.StatusOK)
	var updated tracker.NativeIssue
	decodeHubResponse(t, response, &updated)
	var firstComment tracker.NativeComment
	for index, body := range []string{"first comment", "second comment"} {
		response = performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: body}, Body: body})
		requireNativeStatus(t, response, http.StatusOK)
		if index == 0 {
			decodeHubResponse(t, response, &firstComment)
		}
	}
	response = performHubAPIRequest(t, f.service, http.MethodPatch, itemPath+"/comments/"+firstComment.ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "edit-first"}, ExpectedRevision: firstComment.Revision, Body: "edited first comment"})
	requireNativeStatus(t, response, http.StatusOK)
	ctxs := make(chan context.Context, 1)
	f.service.echo.POST("/api/v2/organizations/:organization/operator-read-fixture", func(c echo.Context) error { ctxs <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	response = performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/operator-read-fixture", f.token, map[string]any{})
	requireNativeStatus(t, response, http.StatusOK)
	ctx := <-ctxs
	executor := hostedOperatorExecutor{service: f.service}
	call := func(tool string, args map[string]any) (operatortool.Result, error) {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		return executor.Execute(ctx, operatortool.Call{Name: tool, Arguments: raw})
	}
	args := func() map[string]any {
		return map[string]any{"project_id": string(f.project.ID), "reference": string(first.WorkItemID)}
	}
	for _, tool := range []string{operatortool.WorkItem, operatortool.WorkRelationships, operatortool.WorkHistory, operatortool.WorkRuns, operatortool.WorkReferences, operatortool.WorkExport, operatortool.BoardActivity, operatortool.BoardReceipt, operatortool.BoardSession} {
		result, err := call(tool, args())
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if strings.Contains(string(result.Content), string(hidden.WorkItemID)) || strings.Contains(string(result.Content), hidden.Title) {
			t.Fatalf("%s leaked foreign dependency: %s", tool, result.Content)
		}
		var envelope struct {
			Project   string `json:"project_id"`
			Freshness string `json:"freshness"`
		}
		if err := json.Unmarshal(result.Content, &envelope); err != nil || envelope.Project != string(f.project.ID) || envelope.Freshness == "" {
			t.Fatalf("%s envelope=%s", tool, result.Content)
		}
	}
	t.Run("retained recovery uses the existing cursor owner", func(t *testing.T) {
		request := args()
		request["view"] = "recovery"
		result, err := call(operatortool.WorkItem, request)
		var envelope operatortool.WorkReadResult[tracker.NativeRecoveryPage]
		if err != nil || json.Unmarshal(result.Content, &envelope) != nil || envelope.Data.Section != "discussion" || envelope.Data.NextCursor == "" {
			t.Fatalf("retained tool read: %s %v", result.Content, err)
		}
		path := itemPath + "?view=recovery&limit=1&cursor=" + url.QueryEscape(envelope.Data.NextCursor)
		response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var next tracker.NativeRecovery
		decodeHubResponse(t, response, &next)
		if next.Continuation == nil || next.Continuation.RecordID == envelope.Data.RecordID || next.Continuation.WorkItemID != first.WorkItemID {
			t.Fatal("tool cursor did not continue through the authenticated HTTP owner")
		}
		request["cursor"] = envelope.Data.NextCursor
		request["reference"] = second.WorkItemID
		if _, err := call(operatortool.WorkItem, request); err == nil {
			t.Fatal("retained tool accepted a foreign item cursor")
		}
	})

	versionArgs := args()
	versionArgs["revision"] = int64(updated.Revision)
	version, err := call(operatortool.WorkVersion, versionArgs)
	if err != nil || strings.Contains(string(version.Content), string(hidden.WorkItemID)) {
		t.Fatalf("version=%s err=%v", version.Content, err)
	}
	commentVersionArgs := args()
	commentVersionArgs["comment_id"] = firstComment.ID
	commentVersionArgs["revision"] = int64(firstComment.Revision)
	version, err = call(operatortool.WorkVersion, commentVersionArgs)
	if err != nil || !strings.Contains(string(version.Content), "first comment") || strings.Contains(string(version.Content), "edited first comment") {
		t.Fatalf("saved comment revision=%s err=%v", version.Content, err)
	}
	listArgs := map[string]any{"project_id": string(f.project.ID), "query": "needle", "limit": 1}
	var listing operatortool.WorkReadResult[tracker.Page[operatortool.NativeItem]]
	result, err := call(operatortool.WorkList, listArgs)
	if err != nil {
		t.Fatal(err)
	}
	listing = operatortool.WorkReadResult[tracker.Page[operatortool.NativeItem]]{}
	if err := json.Unmarshal(result.Content, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Data.Items) != 1 || listing.Data.Items[0].WorkItemID != first.WorkItemID || listing.Data.NextCursor == "" || listing.Data.Items[0].URL == "" {
		t.Fatalf("list=%s", result.Content)
	}
	listArgs["cursor"] = listing.Data.NextCursor
	result, err = call(operatortool.WorkList, listArgs)
	if err != nil {
		t.Fatal(err)
	}
	listing = operatortool.WorkReadResult[tracker.Page[operatortool.NativeItem]]{}
	if err := json.Unmarshal(result.Content, &listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Data.Items) != 1 || listing.Data.Items[0].WorkItemID != second.WorkItemID || listing.Data.NextCursor != "" {
		t.Fatalf("next list=%s", result.Content)
	}
	commentsArgs := args()
	commentsArgs["limit"] = 1
	var comments operatortool.WorkReadResult[tracker.Page[tracker.NativeComment]]
	result, err = call(operatortool.WorkComments, commentsArgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result.Content, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments.Data.Items) != 1 || comments.Data.NextCursor == "" {
		t.Fatalf("comments=%s", result.Content)
	}
	if comments.Data.Items[0].Body != "edited first comment" || comments.Data.Items[0].Revision <= firstComment.Revision {
		t.Fatalf("current edited comment=%s", result.Content)
	}
	commentsArgs["cursor"] = comments.Data.NextCursor
	result, err = call(operatortool.WorkComments, commentsArgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result.Content, &comments); err != nil {
		t.Fatal(err)
	}
	if len(comments.Data.Items) != 1 || comments.Data.Items[0].Body != "second comment" {
		t.Fatalf("next comments=%s", result.Content)
	}
	changedResource := args()
	changedResource["reference"] = string(second.WorkItemID)
	changedResource["cursor"] = commentsArgs["cursor"]
	if _, err := call(operatortool.WorkComments, changedResource); !errors.Is(err, operatortool.ErrInvalidArguments) {
		t.Fatalf("foreign cursor=%v", err)
	}
	for _, tool := range []string{operatortool.WorkItem, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkReferences} {
		foreignArgs := args()
		foreignArgs["project_id"] = string(foreign.project.ID)
		foreignArgs["reference"] = string(hidden.WorkItemID)
		if _, err := call(tool, foreignArgs); !errors.Is(err, operatortool.ErrAccessDenied) {
			t.Fatalf("%s foreign project=%v", tool, err)
		}
		foreignArgs = args()
		foreignArgs["reference"] = string(hidden.WorkItemID)
		if _, err := call(tool, foreignArgs); !errors.Is(err, operatortool.ErrProjectScopeRequired) {
			t.Fatalf("%s foreign item=%v", tool, err)
		}
	}
	if _, err := call(operatortool.WorkConfig, map[string]any{"project_id": string(f.project.ID)}); err != nil {
		t.Fatal(err)
	}
	historyArgs := args()
	historyArgs["limit"] = 1
	result, err = call(operatortool.WorkHistory, historyArgs)
	var history operatortool.WorkReadResult[tracker.Page[tracker.CollaborationEvent]]
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result.Content, &history); err != nil {
		t.Fatal(err)
	}
	if len(history.Data.Items) != 1 || history.Data.NextCursor == "" {
		t.Fatalf("history=%s", result.Content)
	}
	historyArgs["cursor"] = history.Data.NextCursor
	result, err = call(operatortool.WorkHistory, historyArgs)
	var nextHistory operatortool.WorkReadResult[tracker.Page[tracker.CollaborationEvent]]
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(result.Content, &nextHistory); err != nil {
		t.Fatal(err)
	}
	if len(nextHistory.Data.Items) != 1 || nextHistory.Data.Items[0].AggregateSequence <= history.Data.Items[0].AggregateSequence {
		t.Fatalf("next history=%s", result.Content)
	}
	if _, err := call(operatortool.BoardSessionHistory, args()); err == nil {
		t.Fatal("missing session activity succeeded")
	} else {
		var refusal *operatortool.RequestError
		if !errors.As(err, &refusal) || refusal.Code != "not_found" || refusal.Message != "No recorded session activity exists for this work item and attempt" {
			t.Fatalf("missing session activity=%v", err)
		}
	}
	listArgs = map[string]any{"project_id": string(f.project.ID), "query": "absent"}
	result, err = call(operatortool.WorkList, listArgs)
	if err != nil || !strings.Contains(string(result.Content), `"items":[]`) {
		t.Fatalf("empty=%s err=%v", result.Content, err)
	}
	t.Run("comment byte pages", func(t *testing.T) {
		paged := f.create(t, "bounded comment history")
		path := f.base + "/work-items/" + string(paged.WorkItemID) + "/comments"
		var expected []tracker.NativeComment
		for index := range 208 {
			body := fmt.Sprintf("comment %d", index)
			if index >= 100 {
				body += strings.Repeat("large comment ", 200)
				if index%2 == 0 {
					body += strings.Repeat("<\"\\\n🙂", 100)
				}
			}
			if index == 207 {
				body = strings.Repeat("x", 64<<10)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("bounded-comment-%d", index)}, Body: body})
			requireNativeStatus(t, response, http.StatusOK)
			var comment tracker.NativeComment
			decodeHubResponse(t, response, &comment)
			expected = append(expected, comment)
		}
		response := performHubAPIRequest(t, f.service, http.MethodPatch, path+"/"+expected[104].ID, f.token, tracker.UpdateComment{Mutation: tracker.Mutation{IdempotencyKey: "bounded-comment-edit"}, ExpectedRevision: expected[104].Revision, Body: expected[104].Body + " edited"})
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &expected[104])
		var firstCursor string
		protocolCtx := operatortool.BindConnection(ctx, operatortool.ConnectionIdentity(ctx).PrincipalID, "test-client")
		for _, transport := range []string{"stdio", "http"} {
			t.Run(transport, func(t *testing.T) {
				protocol := hostedContextProtocol(t, f.service, protocolCtx, transport)
				for _, mixed := range []bool{false, true} {
					t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
						arguments := map[string]any{"project_id": string(f.project.ID), "reference": string(paged.WorkItemID), "limit": 100}
						seen, pages := 0, 0
						for {
							reply := protocol("tools/call", operatortool.WorkComments, arguments)
							raw := hostedContextData(t, reply, false)
							if len(raw) > operatortool.MaxResultBytes || len(reply.Result) > mcp.MaxHTTPResponseBytes {
								t.Fatalf("unbounded comment result: data=%d protocol=%d", len(raw), len(reply.Result))
							}
							var result operatortool.WorkReadResult[tracker.Page[tracker.NativeComment]]
							if err := json.Unmarshal(raw, &result); err != nil {
								t.Fatal(err)
							}
							page := result.Data
							if len(page.Items) == 0 || len(page.Items) > arguments["limit"].(int) || seen+len(page.Items) > len(expected) {
								t.Fatalf("invalid page: seen=%d items=%d", seen, len(page.Items))
							}
							if !reflect.DeepEqual(page.Items, expected[seen:seen+len(page.Items)]) {
								t.Fatalf("comments changed, duplicated or skipped after %d items", seen)
							}
							if pages == 0 {
								if len(page.Items) != 100 || page.NextCursor == "" {
									t.Fatal("small first page did not reach the count limit")
								}
								firstCursor = page.NextCursor
							} else if pages == 1 && !mixed {
								if len(page.Items) >= 100 || page.NextCursor == "" {
									t.Fatal("large continuation did not respect the byte limit")
								}
								t.Logf("bounded continuation: items=%d bytes=%d", len(page.Items), len(raw))
							}
							seen += len(page.Items)
							pages++
							if page.NextCursor == "" {
								break
							}
							arguments["cursor"] = page.NextCursor
							if mixed && pages%2 == 1 {
								arguments["limit"] = 20
							} else {
								arguments["limit"] = 100
							}
						}
						if seen != len(expected) {
							t.Fatalf("traversal returned %d of %d comments", seen, len(expected))
						}
					})
				}
			})
		}
		for _, test := range []struct {
			name, reference, cursor string
			offset                  int
			message                 string
		}{
			{name: "cursor scope", reference: string(first.WorkItemID), cursor: firstCursor, message: "Cursor scope changed or expired"},
			{name: "offset", reference: string(paged.WorkItemID), offset: 100, message: "returned next_cursor"},
		} {
			t.Run(test.name, func(t *testing.T) {
				_, err := call(operatortool.WorkComments, map[string]any{"project_id": string(f.project.ID), "reference": test.reference, "cursor": test.cursor, "offset": test.offset, "limit": 100})
				var refusal *operatortool.RequestError
				if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, test.message) {
					t.Fatalf("comment paging refusal=%v", err)
				}
			})
		}
		oversizedBody := strings.Repeat("\x00", 64<<10)
		t.Run("expired cursor", func(t *testing.T) {
			now := f.service.config.now
			expired := now().Add(2 * time.Hour)
			f.service.config.now = func() time.Time { return expired }
			defer func() { f.service.config.now = now }()
			_, err := call(operatortool.WorkComments, map[string]any{"project_id": string(f.project.ID), "reference": string(paged.WorkItemID), "cursor": firstCursor, "limit": 100})
			var refusal *operatortool.RequestError
			if !errors.As(err, &refusal) || !strings.Contains(refusal.Message, "expired") {
				t.Fatalf("expired comment cursor=%v", err)
			}
		})
		response = performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "oversized-comment"}, Body: oversizedBody})
		requireNativeStatus(t, response, http.StatusOK)
		for _, transport := range []string{"stdio", "http"} {
			protocol := hostedContextProtocol(t, f.service, protocolCtx, transport)
			for _, limit := range []int{20, 100} {
				arguments := map[string]any{"project_id": string(f.project.ID), "reference": string(paged.WorkItemID), "limit": limit}
				seen := 0
				for {
					reply := protocol("tools/call", operatortool.WorkComments, arguments)
					var result struct {
						IsError bool `json:"isError"`
					}
					if err := json.Unmarshal(reply.Result, &result); err != nil {
						t.Fatal(err)
					}
					if result.IsError {
						var refusal operatortool.RequestError
						if err := json.Unmarshal(hostedContextData(t, reply, true), &refusal); err != nil || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, "single comment") || !strings.Contains(refusal.Message, "work_bulk_export") {
							t.Fatalf("oversized comment refusal=%s", reply.Result)
						}
						if seen != len(expected) {
							t.Fatalf("oversized comment refused after %d of %d readable comments", seen, len(expected))
						}
						break
					}
					var page operatortool.WorkReadResult[tracker.Page[tracker.NativeComment]]
					if err := json.Unmarshal(hostedContextData(t, reply, false), &page); err != nil || len(page.Data.Items) == 0 || page.Data.NextCursor == "" {
						t.Fatal("oversized comment was omitted or stalled pagination")
					}
					seen += len(page.Data.Items)
					if seen > len(expected) {
						t.Fatal("oversized comment was returned unbounded or pagination repeated comments")
					}
					arguments["cursor"] = page.Data.NextCursor
				}
			}
		}
	})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	registered := prepareRunner(t, f, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
	registered.enroll(t)
	worker := registered.redemption.Credential
	selectArgs := map[string]any{"project_id": string(f.project.ID), "reference": string(second.WorkItemID)}
	result, err = call(operatortool.ExplainItem, selectArgs)
	var explanation explain.IssueExplanation
	if err != nil || json.Unmarshal(result.Content, &explanation) != nil || explanation.Eligibility.Latest != nil || explanation.Eligibility.Source != explain.SourceAvailable || explanation.NativeRuntime == nil || len(explanation.NativeRuntime.Admission) == 0 {
		t.Fatalf("missing scheduler history was fabricated: %s %v", result.Content, err)
	}
	publishCapacity(t, f, registered, capacityReport(f.service.config.now()))
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, providerClaim(registered, second, "runtime-session"))
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	started := nativeStartedEvent(lease)
	at := time.Now().UTC()
	started.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 168, Generation: 27, Phase: "implementation", HeartbeatAt: at, Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: at}}}
	runtimePath := f.base + "/work-items/" + string(second.WorkItemID)
	for _, test := range []struct {
		name   string
		path   string
		status int
	}{
		{name: "foreign issue", path: f.base + "/work-items/" + string(hidden.WorkItemID) + "/explanation", status: http.StatusNotFound},
		{name: "unsupported selector", path: runtimePath + "/explanation?attempt_id=foreign", status: http.StatusUnprocessableEntity},
	} {
		t.Run("explanation "+test.name, func(t *testing.T) {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, test.path, f.token, nil), test.status)
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, runtimePath+"/events", worker, started), http.StatusOK)
	observed := started
	observed.Type, observed.IdempotencyKey, observed.Data.Sequence = "run.observed", "runtime-observed", 2
	runtime := *started.Data.Runtime
	runtime.Activity = &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 168, Generation: 27, SessionID: 99, Stage: "implementation", Status: "running", Coverage: "partial", StartedAt: at, AsOf: at, Dropped: 3, Unpaired: 2,
		Sources: []workflowmetrics.InstructionRef{{Name: "AGENTS.md", Hash: strings.Repeat("a", 64), PathRef: strings.Repeat("b", 64)}},
		Spans:   []workflowmetrics.ActivitySpan{{ID: "private-command-and-path", ParentID: "private-parent", Kind: "context_read", Evidence: "read_tool", Outcome: "completed", StartedAt: at, FinishedAt: at, CausalAttribution: "secret", Sources: []workflowmetrics.InstructionRef{{Name: "private-instruction-content", PathRef: "/private/instructions", Hash: "secret"}}}, {ID: "second-span", Kind: "implementation", Evidence: "edit_tool", Outcome: "running", StartedAt: at}}}
	runtime.REST = &tracker.NativeRESTEvidence{Source: "probe", Coverage: "complete", ObservedAt: at, Requests: 19,
		Windows:     []tracker.NativeRESTWindow{{CredentialIdentity: "github-rest:abcdef012345", Resource: "core", EndpointFamily: "other", BudgetScope: "private-path", Requests: 19, Limit: 5000, Used: 3820, UsedObserved: true, Remaining: 1180, ResetAt: at.Add(time.Hour), ObservedAt: at, Status: 200}},
		Divergences: []tracker.NativeRESTDivergence{{CredentialIdentity: "github-rest:abcdef012345", Resource: "core", Attribution: "unattributed", ObservedRequests: 2678, DetentRequests: 19, UnattributedRequests: 2659, WindowStartedAt: at.Add(-6 * time.Minute), LastObservedAt: at, ResetAt: at.Add(time.Hour)}}}
	elapsed, wall := int64(30), int64(10)
	runtime.GitHub = &tracker.NativeGitHubScope{Scope: "native_landing", StartedAt: at, ObservedAt: at, WallElapsedNS: &wall,
		RESTCounts: []tracker.NativeGitHubCount{{NativeGitHubKey: tracker.NativeGitHubKey{Stage: "merging", Step: "land", EndpointFamily: "pull requests", Outcome: "200"}, Count: 2}},
		Timings: []tracker.NativeGitHubTiming{
			{NativeGitHubKey: tracker.NativeGitHubKey{Stage: "merging", Step: "land", EndpointFamily: "pull requests", Outcome: "200"}, Protocol: "rest", Boundary: "http_transport", AttemptCount: 2, TimedCount: 2, ElapsedSumNS: &elapsed, ElapsedMaxNS: &elapsed, FirstObservedAt: at, LastObservedAt: at},
			{NativeGitHubKey: tracker.NativeGitHubKey{Stage: "merging", Step: "prepare", EndpointFamily: "graphql", Outcome: "error"}, Protocol: "graphql", QueryPurpose: "graphql", Boundary: "http_transport", AttemptCount: 1, FirstObservedAt: at, LastObservedAt: at},
			{NativeGitHubKey: tracker.NativeGitHubKey{Stage: "merging", Step: "land", EndpointFamily: "pull requests", Outcome: "200"}, Protocol: "rest", Boundary: "token_resolution_inclusive", AttemptCount: 2, TimedCount: 2, ElapsedSumNS: &elapsed, ElapsedMaxNS: &elapsed, FirstObservedAt: at, LastObservedAt: at},
			{NativeGitHubKey: tracker.NativeGitHubKey{Stage: "merging", Step: "land", EndpointFamily: "app installation tokens", Outcome: "200"}, Protocol: "rest", Boundary: "http_transport", AttemptCount: 1, TimedCount: 1, ElapsedSumNS: &elapsed, ElapsedMaxNS: &elapsed, FirstObservedAt: at, LastObservedAt: at},
		}}
	observed.Data.Runtime = &runtime
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, runtimePath+"/events", worker, observed), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, runtimePath+"/events", worker, observed), http.StatusOK)
	for _, test := range []struct {
		name   string
		change func(*tracker.NativeRuntimeObservation)
		status int
	}{
		{"changed local identity", func(r *tracker.NativeRuntimeObservation) { r.LocalAttemptID++; r.Activity = nil }, http.StatusConflict},
		{"older heartbeat", func(r *tracker.NativeRuntimeObservation) { r.HeartbeatAt = at.Add(-time.Second) }, http.StatusConflict},
		{"worker supplied landing destination", func(r *tracker.NativeRuntimeObservation) {
			r.Landing = &tracker.NativeLandingReceipt{RefusalKind: "conflict", TargetState: "Done", ObservedAt: at}
		}, http.StatusUnprocessableEntity},
		{"negative landing gate duration", func(r *tracker.NativeRuntimeObservation) {
			r.Landing = &tracker.NativeLandingReceipt{GateFailed: true, Gate: &gate.CommandResult{Command: "make check-land", ExitCode: 1, DurationNS: -1, HeadSHA: strings.Repeat("c", 40), TreeSHA: strings.Repeat("d", 40)}, ObservedAt: at}
		}, http.StatusUnprocessableEntity},
		{"failed landing with a passing command receipt", func(r *tracker.NativeRuntimeObservation) {
			r.Landing = &tracker.NativeLandingReceipt{GateFailed: true, Gate: &gate.CommandResult{Command: "make check-land", ExitCode: 0, DurationNS: 1, HeadSHA: strings.Repeat("c", 40), TreeSHA: strings.Repeat("d", 40)}, ObservedAt: at}
		}, http.StatusUnprocessableEntity},
		{"free text landing refusal", func(r *tracker.NativeRuntimeObservation) {
			r.Landing = &tracker.NativeLandingReceipt{RefusalKind: "conflict", Refusal: "private command and credentials", ObservedAt: at}
		}, http.StatusUnprocessableEntity},
		{"landing version without its Change owner", func(r *tracker.NativeRuntimeObservation) {
			r.Landing = &tracker.NativeLandingReceipt{VersionID: "version_" + strings.Repeat("b", 32), RefusalKind: "conflict", ObservedAt: at}
		}, http.StatusUnprocessableEntity},
		{"forged landing", func(r *tracker.NativeRuntimeObservation) {
			r.Landing = &tracker.NativeLandingReceipt{ChangeID: "change_" + strings.Repeat("a", 32), VersionID: "version_" + strings.Repeat("b", 32), HeadSHA: strings.Repeat("c", 40), Landed: true, MergeSHA: strings.Repeat("d", 40), BaseRef: "develop", Method: "squash", ObservedAt: at}
		}, http.StatusNotFound},
		{"landing CI for another head", func(r *tracker.NativeRuntimeObservation) {
			r.Landing = &tracker.NativeLandingReceipt{HeadSHA: strings.Repeat("c", 40), CI: &tracker.NativeLandingCIReceipt{HeadSHA: strings.Repeat("d", 40), PullRequest: 7, State: "pending"}, ObservedAt: at}
		}, http.StatusUnprocessableEntity},
		{"landed pending CI", func(r *tracker.NativeRuntimeObservation) {
			r.Landing = &tracker.NativeLandingReceipt{ChangeID: "change_" + strings.Repeat("a", 32), VersionID: "version_" + strings.Repeat("b", 32), HeadSHA: strings.Repeat("c", 40), Landed: true, MergeSHA: strings.Repeat("d", 40), BaseRef: "develop", Method: "squash", CI: &tracker.NativeLandingCIReceipt{HeadSHA: strings.Repeat("c", 40), PullRequest: 7, State: "pending"}, ObservedAt: at}
		}, http.StatusUnprocessableEntity},
		{"private credential", func(r *tracker.NativeRuntimeObservation) {
			r.REST = &tracker.NativeRESTEvidence{Windows: []tracker.NativeRESTWindow{{CredentialIdentity: "secret-token", EndpointFamily: "other"}}}
		}, http.StatusUnprocessableEntity},
		{"private timing query", func(r *tracker.NativeRuntimeObservation) {
			scope := *r.GitHub
			scope.Timings = slices.Clone(scope.Timings)
			scope.Timings[0].QueryPurpose = "query private token"
			r.GitHub = &scope
		}, http.StatusUnprocessableEntity},
		{"empty GraphQL purpose", func(r *tracker.NativeRuntimeObservation) {
			scope := *r.GitHub
			scope.Timings = slices.Clone(scope.Timings)
			scope.Timings[1].QueryPurpose = ""
			r.GitHub = &scope
		}, http.StatusUnprocessableEntity},
		{"REST GraphQL purpose", func(r *tracker.NativeRuntimeObservation) {
			scope := *r.GitHub
			scope.Timings = slices.Clone(scope.Timings)
			scope.Timings[0].QueryPurpose = "hydrate_pull_request"
			r.GitHub = &scope
		}, http.StatusUnprocessableEntity},
		{"unbounded timing records", func(r *tracker.NativeRuntimeObservation) {
			scope := *r.GitHub
			scope.Timings = make([]tracker.NativeGitHubTiming, tracker.NativeGitHubAggregateLimit+1)
			r.GitHub = &scope
		}, http.StatusUnprocessableEntity},
		{"unbounded windows", func(r *tracker.NativeRuntimeObservation) {
			r.REST = &tracker.NativeRESTEvidence{Windows: make([]tracker.NativeRESTWindow, 65)}
		}, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := observed
			invalid.IdempotencyKey = test.name
			invalid.Data.Sequence = 3
			r := runtime
			test.change(&r)
			invalid.Data.Runtime = &r
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, runtimePath+"/events", worker, invalid), test.status)
		})
	}
	result, err = call(operatortool.BoardSession, selectArgs)
	var runtimeResult operatortool.WorkReadResult[tracker.NativeRuntimeEvidence]
	if err != nil || json.Unmarshal(result.Content, &runtimeResult) != nil {
		t.Fatalf("runtime=%s %v", result.Content, err)
	}
	attempt := runtimeResult.Data.Attempt
	if attempt.TerminalFailure != nil || attempt.TerminalFailureAvailability != "unavailable" || !slices.Contains(runtimeResult.Data.Unavailable, "terminal_failure") || attempt.Finalization != nil || attempt.FinalizationAvailability != "unavailable" || !slices.Contains(runtimeResult.Data.Unavailable, "host_finalization") || !slices.Contains(runtimeResult.Data.Unavailable, "host_issue_acceptance") || attempt.ClaimReleasedAt != nil {
		t.Fatalf("missing historical host evidence was fabricated: %s", result.Content)
	}
	if attempt == nil || attempt.AttemptID != started.Data.AttemptID || attempt.Runtime.LocalAttemptID != 168 || attempt.Runtime.Generation != 27 || !attempt.Current || attempt.RuntimeFreshness != "available" || attempt.Runtime.Phase != "implementation" || attempt.Runtime.Activity.Dropped != 3 || attempt.Runtime.Activity.Unpaired != 2 {
		t.Fatalf("runtime attempt=%#v", attempt)
	}
	timingArgs := map[string]any{"project_id": string(f.project.ID), "reference": string(second.WorkItemID), "native_attempt_id": started.Data.AttemptID, "runner_id": registered.binding.RunnerID}
	protocolCtx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	timingPath := runtimePath + "/runtime/github-timings?native_attempt_id=" + started.Data.AttemptID + "&runner_id=" + registered.binding.RunnerID
	var timingData tracker.NativeGitHubTimingEvidence
	for _, transport := range []string{"stdio", "http"} {
		t.Run("github timings/"+transport, func(t *testing.T) {
			protocol := hostedContextProtocol(t, f.service, protocolCtx, transport)
			reply := protocol("tools/call", operatortool.GitHubScopeTimings, timingArgs)
			raw := hostedContextData(t, reply, false)
			var envelope operatortool.WorkReadResult[tracker.NativeGitHubTimingEvidence]
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			timingData = envelope.Data
			if timingData.RunnerID != registered.binding.RunnerID || timingData.AttemptID != started.Data.AttemptID || len(timingData.Scope.Timings) != 4 || *timingData.Scope.Timings[0].ElapsedSumNS <= *timingData.Scope.WallElapsedNS || timingData.Scope.Timings[1].ElapsedSumNS != nil || timingData.Scope.Timings[1].ElapsedMaxNS != nil || !slices.Contains(timingData.Unavailable, "graphql_http") || slices.Contains(timingData.Unavailable, "installation_http") || !slices.Contains(timingData.Unavailable, "observed_build") || !strings.Contains(timingData.Scope.ElapsedSemantics, "inclusive") {
				t.Fatalf("timing boundaries=%s", raw)
			}
			for _, secret := range []string{"github-rest:abcdef012345", "private-command-and-path", "private-path", f.token, "private-instruction-content"} {
				if strings.Contains(string(reply.Result), secret) {
					t.Fatalf("timing read leaked %q", secret)
				}
			}
			for _, selector := range []struct{ key, value string }{{"runner_id", runnerauth.NewBinding().RunnerID}, {"reference", string(first.WorkItemID)}, {"project_id", string(foreign.project.ID)}} {
				args := make(map[string]any)
				for key, value := range timingArgs {
					args[key] = value
				}
				args[selector.key] = selector.value
				hostedContextData(t, protocol("tools/call", operatortool.GitHubScopeTimings, args), true)
			}
		})
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, timingPath, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var apiTimings tracker.NativeGitHubTimingEvidence
	decodeHubResponse(t, response, &apiTimings)
	if !reflect.DeepEqual(apiTimings, timingData) {
		t.Fatalf("API and MCP timing projection differ: %#v, %#v", apiTimings, timingData)
	}
	otherRunner := prepareRunner(t, f, runnerauth.Read)
	otherRunner.enroll(t)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, timingPath, otherRunner.redemption.Credential, nil), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, timingPath+"&runner_id="+otherRunner.binding.RunnerID, f.token, nil), http.StatusUnprocessableEntity)
	for _, selector := range []map[string]any{{"native_attempt_id": started.Data.AttemptID}, {"attempt_id": int64(168)}} {
		receiptArgs := map[string]any{"project_id": string(f.project.ID), "reference": string(second.WorkItemID)}
		for key, value := range selector {
			receiptArgs[key] = value
		}
		if _, err := call(operatortool.WorkAttemptReceipt, receiptArgs); err != nil {
			t.Fatal(err)
		}
		receiptArgs["reference"] = string(first.WorkItemID)
		if _, err := call(operatortool.WorkAttemptReceipt, receiptArgs); !errors.Is(err, explain.ErrNotFound) {
			t.Fatalf("foreign attempt=%v", err)
		}
	}
	sessionArgs := map[string]any{"project_id": string(f.project.ID), "reference": string(second.WorkItemID), "native_attempt_id": started.Data.AttemptID, "limit": 1}
	result, err = call(operatortool.BoardSessionHistory, sessionArgs)
	var activityPage struct {
		Data struct {
			Profile workflowmetrics.ActivityProfile                     `json:"profile"`
			Page    operatortool.ReadPage[workflowmetrics.ActivitySpan] `json:"page"`
		} `json:"data"`
	}
	if err != nil || json.Unmarshal(result.Content, &activityPage) != nil || len(activityPage.Data.Page.Items) != 1 || activityPage.Data.Page.NextOffset == nil || activityPage.Data.Profile.Coverage != "partial" || activityPage.Data.Profile.Dropped != 3 {
		t.Fatalf("activity=%s %v", result.Content, err)
	}
	for _, secret := range []string{"private-command-and-path", "private-parent", "private-instruction-content", "/private/instructions", "secret"} {
		if strings.Contains(string(result.Content), secret) {
			t.Fatalf("activity secret leaked: %s", result.Content)
		}
	}
	sessionArgs["offset"] = *activityPage.Data.Page.NextOffset
	if result, err = call(operatortool.BoardSessionHistory, sessionArgs); err != nil || !strings.Contains(string(result.Content), "edit_tool") {
		t.Fatalf("activity next=%s %v", result.Content, err)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, runtimePath+"/runtime?native_attempt_id="+started.Data.AttemptID, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &runtimeResult.Data)
	explanationResponse := performHubAPIRequest(t, f.service, http.MethodGet, runtimePath+"/explanation", f.token, nil)
	requireNativeStatus(t, explanationResponse, http.StatusOK)
	var browserExplanation explain.IssueExplanation
	decodeHubResponse(t, explanationResponse, &browserExplanation)
	if browserExplanation.Schema != explain.SchemaVersion || browserExplanation.Identity.IssueID != string(second.WorkItemID) || browserExplanation.NativeRuntime.Attempt.AttemptID != started.Data.AttemptID || len(browserExplanation.NativeRuntime.Attempt.Runtime.Activity.Spans) != 0 {
		t.Fatalf("browser explanation lost native identity or exposed activity details: %#v", browserExplanation)
	}
	for _, secret := range []string{"private-command-and-path", "private-parent", "private-instruction-content", "/private/instructions", "secret"} {
		if strings.Contains(explanationResponse.Body.String(), secret) {
			t.Fatalf("browser explanation leaked %q", secret)
		}
	}
	rest := runtimeResult.Data.Attempt.Runtime.REST
	if rest.Source != "ordinary_response_headers" || !strings.Contains(rest.Coverage, "unavailable") || rest.Windows[0].Used != 3820 || !rest.Windows[0].UsedObserved || rest.Windows[0].ObservedAt != at || rest.Windows[0].BudgetScope != "native_landing" || rest.Divergences[0].UnattributedRequests != 2659 {
		t.Fatalf("REST authority=%#v", rest)
	}
	if len(runtimeResult.Data.Attempt.Runtime.Activity.Spans) != 2 {
		t.Fatal("API runtime omitted recorded spans")
	}
	clock := f.service.config.now
	f.service.config.now = func() time.Time { return at.Add(2 * time.Minute) }
	result, err = call(operatortool.BoardSession, selectArgs)
	if err != nil || !strings.Contains(string(result.Content), `"runtime_freshness":"expired"`) || !strings.Contains(string(result.Content), `"current":false`) {
		t.Fatalf("stale runtime=%s %v", result.Content, err)
	}
	f.service.config.now = clock
	finished := observed
	finished.Type, finished.IdempotencyKey, finished.Data.Sequence, finished.Data.Outcome = "run.finished", "runtime-finished", 3, "succeeded"
	finishedRuntime := runtime
	finishedRuntime.Phase = "completed"
	finishedRuntime.Completion = &tracker.NativeCompletionObservation{AcceptanceRecorded: false, ObservedAt: at}
	finishedRuntime.Landing = &tracker.NativeLandingReceipt{RefusalKind: "nothing_to_land", ObservedAt: at}
	finished.Data.Runtime = &finishedRuntime
	finished.Data.Disposition = &tracker.NativeDisposition{Status: "complete"}
	finished.Data.Finalization = &tracker.NativeFinalization{ObservedAt: at, Settled: true, BaseSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("c", 40), VersionCode: "invalid_request", VersionError: "the final attempt diff does not identify the current Change Request head: /private/source/main.go token=private-secret\nprivate source content"}
	for _, test := range []struct {
		name   string
		change func(*tracker.NativeFinalization)
	}{
		{"oversized refusal", func(f *tracker.NativeFinalization) {
			f.VersionError = strings.Repeat("é", tracker.NativeFinalizationTextLimit)
		}},
		{"invalid head", func(f *tracker.NativeFinalization) { f.HeadSHA = "/private/source" }},
		{"private refusal code", func(f *tracker.NativeFinalization) { f.VersionCode = "private/source" }},
		{"unowned source version", func(f *tracker.NativeFinalization) {
			f.SourceVersion = &tracker.NativeChangeReference{ChangeID: newNativeID("change"), VersionID: newNativeID("version"), HeadSHA: strings.Repeat("c", 40)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := finished
			invalid.IdempotencyKey = test.name
			finalization := *finished.Data.Finalization
			test.change(&finalization)
			invalid.Data.Finalization = &finalization
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, runtimePath+"/events", worker, invalid), http.StatusUnprocessableEntity)
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, runtimePath+"/events", worker, finished), http.StatusOK)
	response = performHubAPIRequest(t, f.service, http.MethodPost, runtimePath+"/workflow", worker, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "runner-done", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ExpectedRevision: second.Revision, State: "Done", Reason: "worker_progress"})
	requireNativeStatus(t, response, http.StatusOK)
	result, err = call(operatortool.ExplainItem, selectArgs)
	if err != nil || json.Unmarshal(result.Content, &explanation) != nil || explanation.LatestTransition == nil || explanation.LatestTransition.Actor.Kind != "runner" || explanation.LatestTransition.Reason != "worker_progress" || explanation.Attempt.NativeID != started.Data.AttemptID || explanation.NativeRuntime.Attempt.Runtime.Landing.Landed || explanation.NativeRuntime.Attempt.Runtime.Landing.RefusalKind != "nothing_to_land" || explanation.NativeRuntime.Attempt.Runtime.Landing.Refusal != "nothing_to_land" || explanation.NativeRuntime.Attempt.Runtime.Landing.TargetState != "Done" || slices.Contains(explanation.NativeRuntime.Unavailable, "attempt_landing_receipt") {
		t.Fatalf("native explanation=%s %v", result.Content, err)
	}
	projected := explanation.NativeRuntime.Attempt
	if projected.FinalizationAvailability != "available" || projected.Finalization.VersionCode != "invalid_request" || !projected.Finalization.Settled || !projected.Finalization.ObservedAt.Equal(at) || !projected.Finalization.TextRedacted || projected.Runtime.Completion.AcceptanceRecorded || projected.Disposition.Status != "complete" || slices.Contains(explanation.NativeRuntime.Unavailable, "host_finalization") || slices.Contains(explanation.NativeRuntime.Unavailable, "host_issue_acceptance") {
		t.Fatalf("host result was confused with provider completion: %s", result.Content)
	}
	for _, secret := range []string{"/private/source", "private-secret", "private source content"} {
		if strings.Contains(string(result.Content), secret) {
			t.Fatalf("finalizer read leaked %q", secret)
		}
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	runResult, err := call(operatortool.GetNativeRun, map[string]any{"project_id": string(f.project.ID), "work_item_id": string(second.WorkItemID), "attempt_id": started.Data.AttemptID})
	var run operatortool.ChangeResult
	if err != nil || json.Unmarshal(runResult.Content, &run) != nil || run.Attempt == nil || run.Attempt.ClaimReleasedAt == nil || run.Attempt.Current || !reflect.DeepEqual(run.Attempt.Finalization, projected.Finalization) || !reflect.DeepEqual(run.Attempt.Runtime.Completion, projected.Runtime.Completion) {
		t.Fatalf("run projection lost host result or release evidence: %s %v", runResult.Content, err)
	}
	var decisionID string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='scheduler.decision' ORDER BY sequence DESC LIMIT 1", f.project.OrganizationID, f.project.ID, second.WorkItemID).Scan(&decisionID); err != nil {
		t.Fatal(err)
	}
	decision := explanation.Eligibility.Latest
	if decision == nil || decision.EvidenceID != decisionID || decision.Source != "native_claim" || decision.Outcome != "claimed" || !decision.Historical || explanation.Eligibility.Source != explain.SourceAvailable {
		t.Fatalf("recorded claim history missing: %#v", explanation.Eligibility)
	}
	for _, setupOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded terminal failure setup=%t", setupOnly), func(t *testing.T) {
			failedIssue := f.create(t, fmt.Sprintf("Terminal failure setup=%t", setupOnly))
			path := f.base + "/work-items/" + string(failedIssue.WorkItemID)
			if setupOnly {
				result, err := call(operatortool.BoardSession, map[string]any{"project_id": string(f.project.ID), "reference": string(failedIssue.WorkItemID)})
				var evidence operatortool.WorkReadResult[tracker.NativeRuntimeEvidence]
				if err != nil || json.Unmarshal(result.Content, &evidence) != nil || evidence.Data.Attempt != nil || !slices.Contains(evidence.Data.Unavailable, "native_attempt") {
					t.Fatalf("no-attempt case fabricated history: %s err=%v", result.Content, err)
				}
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, providerClaim(registered, failedIssue, fmt.Sprintf("provider-terminal-session-%t", setupOnly)))
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			started := nativeStartedEvent(lease)
			if setupOnly {
				older := started
				older.IdempotencyKey = "older-provider-start"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, older), http.StatusOK)
				older.Type, older.IdempotencyKey, older.Data.Sequence, older.Data.Outcome = "run.finished", "older-provider-finish", 2, "succeeded"
				older.Data.Disposition = &tracker.NativeDisposition{Status: "complete"}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, older), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, providerClaim(registered, failedIssue, "setup-terminal-session"))
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &lease)
				started = nativeStartedEvent(lease)
			}
			if !setupOnly {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, started), http.StatusOK)
			}
			code, maxChars, actualChars := -32602, int64(1048576), int64(2927066)
			finished := started
			finished.Type, finished.IdempotencyKey, finished.Data.Sequence, finished.Data.Outcome = "run.finished", fmt.Sprintf("provider-finished-%t", setupOnly), 2, "failed"
			finished.Data.TerminalFailure = &tracker.NativeTerminalFailure{Error: "count 116 attempts failed /private/error token=private-secret\nprivate-prompt", ErrorClass: "protocol", ObservedAt: at, Provider: "codex", Operation: "turn/start", RPCCode: &code, ProviderCode: "input_too_large", MaxChars: &maxChars, ActualChars: &actualChars, Source: "/private/source", Summary: "private-prompt token=private-secret", Coverage: "private-rpc-data", Unavailable: []string{"private-prompt"}}
			if setupOnly {
				finished.Data.Identity, finished.Data.Sequence = nil, 1
				finished.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 169, Generation: 28, Phase: "completed", HeartbeatAt: at}
				finished.Data.TerminalFailure = &tracker.NativeTerminalFailure{Error: "after_create exit status 127: script not found /private/error token=private-secret\nprivate-prompt", ErrorClass: "workspace_hook", ObservedAt: at}
			}
			for _, test := range []struct {
				name   string
				change func(*tracker.NativeRunEvent)
			}{
				{"nonterminal failure", func(e *tracker.NativeRunEvent) { e.Type, e.Data.Outcome = "run.checkpointed", "" }},
				{"successful failure", func(e *tracker.NativeRunEvent) { e.Data.Outcome = "succeeded" }},
				{"unsequenced failure", func(e *tracker.NativeRunEvent) { e.Data.Sequence = 0 }},
				{"undated failure", func(e *tracker.NativeRunEvent) { e.Data.TerminalFailure.ObservedAt = time.Time{} }},
				{"setup claims source", func(e *tracker.NativeRunEvent) {
					if setupOnly {
						e.Data.Finalization = &tracker.NativeFinalization{ObservedAt: at, Settled: true}
					} else {
						e.Data.Identity = nil
					}
				}},
				{"setup claims provider session", func(e *tracker.NativeRunEvent) {
					if setupOnly {
						copy := *e.Data.Runtime
						copy.Activity = &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 43}
						e.Data.Runtime = &copy
					} else {
						e.Data.Identity = nil
					}
				}},
				{"setup missing local identity", func(e *tracker.NativeRunEvent) {
					if setupOnly {
						copy := *e.Data.Runtime
						copy.LocalAttemptID = 0
						e.Data.Runtime = &copy
					} else {
						e.Data.Identity = nil
					}
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					invalid := finished
					failure := *finished.Data.TerminalFailure
					invalid.Data.TerminalFailure = &failure
					invalid.IdempotencyKey = test.name
					test.change(&invalid)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, invalid), http.StatusUnprocessableEntity)
				})
			}
			for range 2 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finished), http.StatusOK)
			}
			if setupOnly {
				result, err := call(operatortool.WorkRuns, map[string]any{"project_id": string(f.project.ID), "reference": string(failedIssue.WorkItemID)})
				var runs operatortool.WorkReadResult[tracker.Page[tracker.NativeAttempt]]
				if err != nil || json.Unmarshal(result.Content, &runs) != nil || len(runs.Data.Items) != 2 || runs.Data.Items[0].Status != "succeeded" || runs.Data.Items[0].Identity == nil || runs.Data.Items[1].AttemptID != finished.Data.AttemptID || runs.Data.Items[1].TerminalFailure == nil {
					t.Fatalf("work_runs lost setup or historical provider attempt: %s err=%v", result.Content, err)
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
			wantSignature := "count <n> attempts failed [redacted] [redacted]"
			if setupOnly {
				wantSignature = "after_create exit status 127: script not found [redacted] [redacted]"
			}
			for _, tool := range []string{operatortool.GetNativeRun, operatortool.WorkAttemptReceipt, operatortool.BoardSession, operatortool.ExplainItem} {
				t.Run(tool, func(t *testing.T) {
					args := map[string]any{"project_id": string(f.project.ID), "reference": string(failedIssue.WorkItemID), "native_attempt_id": started.Data.AttemptID}
					if tool == operatortool.ExplainItem || tool == operatortool.BoardSession {
						delete(args, "native_attempt_id")
					}
					if tool == operatortool.GetNativeRun {
						args = map[string]any{"project_id": string(f.project.ID), "work_item_id": string(failedIssue.WorkItemID), "attempt_id": started.Data.AttemptID}
					}
					result, err := call(tool, args)
					if err != nil {
						t.Fatal(err)
					}
					var run operatortool.ChangeResult
					var runtime operatortool.WorkReadResult[tracker.NativeRuntimeEvidence]
					var explanation explain.IssueExplanation
					var attempt *tracker.NativeAttempt
					switch tool {
					case operatortool.GetNativeRun:
						err = json.Unmarshal(result.Content, &run)
						attempt = run.Attempt
					case operatortool.WorkAttemptReceipt, operatortool.BoardSession:
						err = json.Unmarshal(result.Content, &runtime)
						attempt = runtime.Data.Attempt
					case operatortool.ExplainItem:
						err = json.Unmarshal(result.Content, &explanation)
						if explanation.FailureSignature == nil || explanation.FailureSignature.Signature != wantSignature || explanation.FailureSignature.Count != 1 || explanation.FailureSignature.Partial {
							t.Fatalf("MCP lost failure signature: %s", result.Content)
						}
						if explanation.NativeRuntime != nil {
							attempt = explanation.NativeRuntime.Attempt
						}
					}
					if err != nil || attempt == nil || attempt.AttemptID != started.Data.AttemptID || attempt.FencingToken != lease.FencingToken || attempt.TerminalFailureAvailability != "available" || attempt.FinalizationAvailability != "unavailable" || attempt.ClaimReleasedAt == nil || attempt.Status != "failed" {
						t.Fatalf("MCP lost terminal failure identity: %s, %v", result.Content, err)
					}
					failure := attempt.TerminalFailure
					if !setupOnly && (failure == nil || failure.Provider != "codex" || failure.Operation != "turn/start" || failure.ProviderCode != "input_too_large" || failure.RPCCode == nil || *failure.RPCCode != code || failure.MaxChars == nil || *failure.MaxChars != maxChars || failure.ActualChars == nil || *failure.ActualChars != actualChars || !failure.ObservedAt.Equal(at) || failure.Source != "host_runner_completion") {
						t.Fatalf("MCP lost recorded provider diagnostics: %s", result.Content)
					}
					if setupOnly && (attempt.Identity != nil || attempt.Runtime.LocalAttemptID != 169 || attempt.Runtime.Generation != 28 || failure == nil || failure.Stage != "pre_provider" || failure.Attribution != "instance" || failure.Provider != "" || failure.Error != "after_create exit status 127: script not found [redacted] [redacted]" || !failure.ObservedAt.Equal(at)) {
						t.Fatalf("MCP lost setup diagnostic or selected older provider history: %s", result.Content)
					}
					outputs := []operatortool.Result{result}
					if tool == operatortool.ExplainItem {
						args["include"] = []string{"summary"}
						compact, compactErr := call(tool, args)
						var summary explain.IssueExplanation
						if compactErr != nil || json.Unmarshal(compact.Content, &summary) != nil || summary.FailureSignature == nil || summary.FailureSignature.Signature != wantSignature || summary.Attempt == nil || summary.Attempt.NativeID != started.Data.AttemptID || summary.NativeRuntime != nil {
							t.Fatalf("compact MCP lost redacted failure summary: %s error=%v", compact.Content, compactErr)
						}
						outputs = append(outputs, compact)
					}
					for _, output := range outputs {
						for _, private := range []string{"/private/source", "/private/error", "private-prompt", "private-secret", "private-rpc-data"} {
							if strings.Contains(string(output.Content), private) {
								t.Fatalf("MCP exposed %q", private)
							}
						}
					}
				})
			}
		})
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE organization_id=? AND project_id=? AND token_id=(SELECT id FROM api_tokens WHERE token_hash=?)", f.project.OrganizationID, f.project.ID, apikey.HashToken(f.token)); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{operatortool.ExplainItem, operatortool.BoardReceipt, operatortool.BoardSession} {
		if _, err := call(tool, selectArgs); !errors.Is(err, operatortool.ErrAccessDenied) {
			t.Fatalf("revoked %s=%v", tool, err)
		}
	}
	for _, transport := range []string{"stdio", "http"} {
		protocol := hostedContextProtocol(t, f.service, protocolCtx, transport)
		hostedContextData(t, protocol("tools/call", operatortool.GitHubScopeTimings, timingArgs), true)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, timingPath, f.token, nil), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, runtimePath+"/runtime", f.token, nil), http.StatusNotFound)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, runtimePath+"/explanation", f.token, nil), http.StatusNotFound)

}

func TestOperatorArchivedWorkReads(t *testing.T) {
	f := newAttemptDiffFixture(t)
	requireNativeStatus(t, f.post(t, f.request(1, tracker.AttemptDiffFile{Path: "first.go", Status: "added", Additions: 1, Patch: "+first"}, tracker.AttemptDiffFile{Path: "second.go", Status: "added", Additions: 1, Patch: "+second"})), http.StatusAccepted)
	f.event(t, "run.finished", 2, "succeeded")
	at := f.service.config.now()
	f.service.config.now = func() time.Time { return at }
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json=json_set(data_json,'$.runtime',json(?)) WHERE id=?`, `{"local_attempt_id":42,"generation":1,"phase":"completed","heartbeat_at":"2026-10-21T00:00:00Z","phases":[],"phases_dropped":0}`, f.attempt); err != nil {
		t.Fatal(err)
	}
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	call := hostedContextProtocol(t, f.service, ctx, "stdio")
	read := func(t *testing.T, name string, args map[string]any) []byte {
		t.Helper()
		args["project_id"] = string(f.project.ID)
		reply := call("tools/call", name, args)
		var result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(reply.Result, &result) != nil || result.IsError || len(result.Content) != 1 {
			t.Fatalf("%s failed: %s %s", name, reply.Result, reply.Error)
		}
		return []byte(result.Content[0].Text)
	}
	item := string(f.issue.WorkItemID)
	cases := []struct {
		name    string
		args    map[string]any
		fetches int
		hot     []byte
	}{
		{operatortool.WorkHistory, map[string]any{"reference": item, "limit": 1}, 1, nil},
		{operatortool.WorkRuns, map[string]any{"reference": item}, 0, nil},
		{operatortool.WorkAttemptReceipt, map[string]any{"reference": item, "native_attempt_id": f.attempt}, 1, nil},
		{operatortool.WorkAttemptReceipt, map[string]any{"reference": item, "attempt_id": 42}, 1, nil},
		{operatortool.WorkItem, map[string]any{"reference": item, "view": "recovery"}, 1, nil},
		{operatortool.WorkVersion, map[string]any{"reference": item, "revision": 1}, 1, nil},
		{operatortool.GetNativeRun, map[string]any{"work_item_id": item, "attempt_id": f.attempt}, 1, nil},
		{operatortool.GetAttemptDiff, map[string]any{"work_item_id": item, "attempt_id": f.attempt, "limit": 1}, 1, nil},
		{operatortool.GetWorkItemDiff, map[string]any{"work_item_id": item, "limit": 1}, 1, nil},
	}
	for i := range cases {
		cases[i].hot = read(t, cases[i].name, cases[i].args)
	}
	var history struct {
		Data tracker.Page[tracker.CollaborationEvent] `json:"data"`
	}
	if err := json.Unmarshal(cases[0].hot, &history); err != nil {
		t.Fatal(err)
	}
	if history.Data.NextCursor == "" {
		t.Fatal("missing pre-archive history cursor")
	}
	args := map[string]any{"reference": item, "limit": 1, "cursor": history.Data.NextCursor}
	cases = append(cases, struct {
		name    string
		args    map[string]any
		fetches int
		hot     []byte
	}{operatortool.WorkHistory, args, 1, read(t, operatortool.WorkHistory, args)})
	var lastEvent int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT max(sequence) FROM collaboration_events WHERE work_item_id=?", item).Scan(&lastEvent); err != nil {
		t.Fatal(err)
	}
	allHistory := read(t, operatortool.WorkHistory, map[string]any{"reference": item, "limit": 100})
	historyTable := archiveTestTable(t, f.service, "collaboration_events", "work_item_id=?", item)
	objects := map[string][]byte{}
	for _, part := range []struct {
		kind, subject string
		first, last   int64
		tables        []attachment.ArchiveTable
	}{
		{"run", f.attempt, 0, 0, []attachment.ArchiveTable{
			archiveTestTable(t, f.service, "native_attempts", "id=?", f.attempt),
			archiveTestTable(t, f.service, "native_attempt_events", "attempt_id=?", f.attempt),
			archiveTestTable(t, f.service, "attempt_diffs", "attempt_id=?", f.attempt),
			archiveTestTable(t, f.service, "attempt_diff_files", "diff_id IN (SELECT id FROM attempt_diffs WHERE attempt_id=?)", f.attempt),
		}},
		{"revision", item, 1, 1, []attachment.ArchiveTable{archiveTestTable(t, f.service, "collaboration_versions", "record_id=?", item)}},
		{"history", item, 1, lastEvent, []attachment.ArchiveTable{historyTable}},
	} {
		ref, data := publishTestSection(t, f.nativeFixture, item, part.kind, part.subject, part.first, part.last, part.tables...)
		objects[ref.Key] = data
	}
	for _, statement := range []string{
		`UPDATE native_attempts SET data_json=json_remove(data_json,'$.runtime')`,
		`DELETE FROM attempt_diff_files`,
		`DROP TRIGGER collaboration_versions_no_delete`,
		`DELETE FROM collaboration_versions`,
		`DROP TRIGGER collaboration_events_no_update`,
		`UPDATE collaboration_events SET data_json='{}' WHERE type='issue.created'`,
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	fetches := 0
	f.service.config.ArchiveReader = func(_ context.Context, ref attachment.ArchiveReference, _ bool) (entryauthority.ArchiveResponse, error) {
		fetches++
		return entryauthority.ArchiveResponse{Data: objects[ref.Key]}, nil
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fetches = 0
			archived := read(t, test.name, test.args)
			assertArchiveGolden(t, test.hot, archived)
			if fetches != test.fetches {
				t.Fatalf("section fetches=%d want %d", fetches, test.fetches)
			}
		})
	}

	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM native_archive_sections WHERE work_item_id=? AND kind='history'", item); err != nil {
		t.Fatal(err)
	}
	boundary := len(historyTable.Rows) / 2
	for _, part := range []struct{ start, end int }{{0, boundary}, {boundary, len(historyTable.Rows)}} {
		table := historyTable
		table.Rows = table.Rows[part.start:part.end]
		index := slices.Index(table.Columns, "sequence")
		var first, last int64
		if err := json.Unmarshal(table.Rows[0][index], &first); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(table.Rows[len(table.Rows)-1][index], &last); err != nil {
			t.Fatal(err)
		}
		ref, data := publishTestSection(t, f.nativeFixture, item, "history", item, first, last, table)
		objects[ref.Key] = data
	}
	var want struct {
		Data tracker.Page[tracker.CollaborationEvent] `json:"data"`
	}
	if err := json.Unmarshal(allHistory, &want); err != nil {
		t.Fatal(err)
	}
	var events []tracker.CollaborationEvent
	cursor := ""
	pages := 0
	for {
		args := map[string]any{"reference": item, "limit": 100}
		if cursor != "" {
			args["cursor"] = cursor
		}
		fetches = 0
		var page struct {
			Data tracker.Page[tracker.CollaborationEvent] `json:"data"`
		}
		if err := json.Unmarshal(read(t, operatortool.WorkHistory, args), &page); err != nil {
			t.Fatal(err)
		}
		if fetches != 1 || len(page.Data.Items) == 0 {
			t.Fatalf("history page fetches=%d items=%d", fetches, len(page.Data.Items))
		}
		events = append(events, page.Data.Items...)
		pages++
		if page.Data.NextCursor == "" {
			break
		}
		cursor = page.Data.NextCursor
	}
	if pages != 2 || !reflect.DeepEqual(events, want.Data.Items) {
		t.Fatal("section pagination changed history records or anchors")
	}
}
