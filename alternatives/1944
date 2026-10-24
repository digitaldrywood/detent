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
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedNativeBulkExportLargeItem(t *testing.T) {
	f, ctx := newHostedKeyMCPFixture(t, "dedicated", "owner")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET duplicate_challenge=0 WHERE id=?", f.project); err != nil {
		t.Fatal(err)
	}
	const observedResultBytes = 1705891
	type discussion struct {
		body  string
		count int
		state string
	}
	largeBody := strings.Repeat("Stored imported discussion. ", observedResultBytes/28+1)
	shapes := []discussion{
		{strings.Repeat("Stored imported discussion. ", 18), 852, "Backlog"},
		{largeBody, 1, "Todo"},
		{largeBody, 1, "Rework"},
	}
	for range 10 {
		shapes = append(shapes, discussion{largeBody, 1, "Backlog"})
	}
	now := formatHubTime(time.Now())
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scope := nativeScope{organization: "org_security", project: f.project}
	project, err := readNativeProject(t.Context(), tx, scope)
	if err != nil {
		t.Fatal(err)
	}
	states := append(project.States, tracker.NativeState{Name: "Backlog"}, tracker.NativeState{Name: "Rework"})
	if err := applyNativeProjectStates(t.Context(), tx, scope, states, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	expected := map[tracker.NativeWorkItemID]discussion{}
	for index, shape := range shapes {
		response := f.request(t, f.user, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("large-export-%d", index)}, Title: fmt.Sprintf("Export item %d", index), State: "Backlog"})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state=?) WHERE native_id=?", f.project, shape.state, issue.WorkItemID); err != nil {
			t.Fatal(err)
		}
		expected[issue.WorkItemID] = shape
		for sequence := range shape.count {
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at) VALUES(?,?,?,?,1,?,?,'{}',?,?)", fmt.Sprintf("comment_%s_%d", issue.WorkItemID, sequence), issue.OrganizationID, issue.ProjectID, issue.WorkItemID, sequence+1, shape.body, now, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, transport := range []string{"native", "stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			var call func(string, string, any) hostedContextReply
			if transport != "native" {
				call = hostedContextProtocol(t, f.service, ctx, transport)
			}
			for _, selection := range []struct {
				state string
				limit int
			}{{"", 500}, {"", 50}, {"Backlog", 500}, {"Backlog", 20}, {"Todo", 100}, {"Rework", 500}, {"Backlog", 1}} {
				t.Run(fmt.Sprintf("%s/%d", selection.state, selection.limit), func(t *testing.T) {
					args := map[string]any{"project_id": string(f.project), "limit": selection.limit}
					query := url.Values{"limit": {strconv.Itoa(selection.limit)}}
					if selection.state == "" {
						args["open"] = true
						query.Set("open", "true")
					} else {
						args["states"] = []string{selection.state}
						query.Set("state", selection.state)
					}
					want := map[tracker.NativeWorkItemID]discussion{}
					for id, shape := range expected {
						if selection.state == "" || selection.state == shape.state {
							want[id] = shape
						}
					}
					seen := map[tracker.NativeWorkItemID]bool{}
					calls, totalBytes := 0, 0
					for {
						calls++
						var raw []byte
						if transport == "native" {
							response := f.request(t, f.user, http.MethodGet, f.base+"/work-items/export?"+query.Encode(), nil)
							requireNativeStatus(t, response, http.StatusOK)
							raw = response.Body.Bytes()
						} else {
							raw = hostedContextData(t, call("tools/call", operatortool.WorkBulkExport, args), false)
						}
						var page operatortool.WorkBulkExportPage
						if err := json.Unmarshal(raw, &page); err != nil {
							t.Fatal(err)
						}
						totalBytes += len(raw)
						if len(raw) <= operatortool.MaxResultBytes || len(raw) > operatortool.WorkBulkExportMaxBytes || page.Total != len(want) || len(page.Items) == 0 || page.Truncated != (page.NextCursor != "") {
							t.Fatalf("invalid large export: bytes=%d total=%d items=%d", len(raw), page.Total, len(page.Items))
						}
						for _, item := range page.Items {
							shape, ok := want[item.WorkItemID]
							if !ok || seen[item.WorkItemID] || len(item.Comments) != shape.count || item.State != shape.state || item.ProjectID != f.project {
								t.Fatal("large export lost or repeated discussion or changed scope")
							}
							for index, comment := range item.Comments {
								if comment.Body != shape.body || comment.ProjectID != f.project || comment.WorkItemID != item.WorkItemID || comment.Sequence != int64(index+1) {
									t.Fatal("large export changed stored discussion")
								}
							}
							seen[item.WorkItemID] = true
						}
						if page.NextCursor == "" {
							break
						}
						if calls >= len(want) {
							t.Fatal("export cursor did not terminate")
						}
						args["cursor"] = page.NextCursor
						query.Set("cursor", page.NextCursor)
					}
					if len(seen) != len(want) || totalBytes < observedResultBytes {
						t.Fatalf("exported %d of %d items in %d bytes", len(seen), len(want), totalBytes)
					}
					if (selection.state == "" || selection.state == "Backlog") && (calls < 2 || totalBytes <= operatortool.WorkBulkExportMaxBytes) {
						t.Fatalf("oversized project did not exercise bounded pagination: calls=%d bytes=%d", calls, totalBytes)
					}
					t.Logf("exported %d items in %d pages (%d bytes)", len(seen), calls, totalBytes)
				})
			}
		})
	}
}

func TestNativeBulkExportItemFailure(t *testing.T) {
	for _, test := range []struct {
		name, body, actor, cause, step string
	}{
		{"malformed comment", "Private discussion", "[]", "cannot unmarshal array", "read comment"},
		{"oversized item", strings.Repeat("Private discussion ", operatortool.WorkBulkExportMaxBytes/19+1), "{}", "operator tool result exceeds", "encode item"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "export-item-failure")
			item := f.create(t, "Export item")
			now := formatHubTime(time.Now())
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at) VALUES(?,?,?,?,1,1,?,?,?,?)", "comment_"+string(item.WorkItemID), item.OrganizationID, item.ProjectID, item.WorkItemID, test.body, test.actor, now, now); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
			for _, transport := range []string{"native", "stdio", "http"} {
				t.Run(transport, func(t *testing.T) {
					logs.Reset()
					var raw []byte
					if transport == "native" {
						response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/export", f.token, nil)
						requireNativeStatus(t, response, http.StatusInternalServerError)
						raw = response.Body.Bytes()
					} else {
						call := hostedContextProtocol(t, f.service, ctx, transport)
						reply := call("tools/call", operatortool.WorkBulkExport, map[string]any{"project_id": string(f.project.ID)})
						var result struct {
							IsError bool            `json:"isError"`
							Data    json.RawMessage `json:"structuredContent"`
						}
						if err := json.Unmarshal(reply.Result, &result); err != nil {
							t.Fatal(err)
						}
						if !result.IsError {
							t.Fatal("export accepted invalid item")
						}
						raw = result.Data
					}
					var failure struct {
						Code       string `json:"code"`
						Message    string `json:"message"`
						WorkItemID string `json:"work_item_id"`
					}
					if err := json.Unmarshal(raw, &failure); err != nil {
						t.Fatal(err)
					}
					if failure.Code != "bulk_export_item_failed" || failure.WorkItemID != string(item.WorkItemID) || strings.Contains(string(raw), "Private discussion") {
						t.Fatalf("export failure lost item identity: %s", raw)
					}
					if !strings.HasPrefix(failure.Message, "Work item could not be exported: "+test.step) || strings.Contains(failure.Message, test.cause) {
						t.Fatalf("export failure lost safe step: %s", raw)
					}
					var entry struct {
						Error string `json:"error"`
					}
					if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(entry.Error, string(item.WorkItemID)) || !strings.Contains(entry.Error, test.cause) {
						t.Fatalf("export log lost cause: %s", logs.Bytes())
					}
				})
			}
		})
	}
}

type exportTestImporter struct {
	client   *http.Client
	endpoint string
}

func (i exportTestImporter) FetchImportPage(ctx context.Context, _ GitHubImportRequest) (GitHubImportPage, error) {
	for _, path := range []string{"/repos/example-org/example-project/issues", "/graphql"} {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, i.endpoint+path, nil)
		if err != nil {
			return GitHubImportPage{}, err
		}
		response, err := i.client.Do(request)
		if err != nil {
			return GitHubImportPage{}, err
		}
		if err := response.Body.Close(); err != nil {
			return GitHubImportPage{}, err
		}
	}
	return GitHubImportPage{}, errors.New("unexpected export source refresh")
}

func TestNativeBulkExport(t *testing.T) {
	f := newNativeFixture(t, nil, "", "bulk-export")
	foreign := newNativeFixture(t, f.service, f.project.OrganizationID, "foreign-export")
	foreign.create(t, "Private other project")
	response := performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations", testHubAdminToken, map[string]any{"name": "Other export tenant"})
	requireNativeStatus(t, response, http.StatusCreated)
	var organization nativeOrganization
	decodeHubResponse(t, response, &organization)
	other := newNativeFixture(t, f.service, organization.ID, "other-export")
	other.create(t, "Private other organization")
	const count = 501
	expected := map[tracker.NativeWorkItemID]tracker.NativeIssue{}
	var first, second tracker.NativeIssue
	for index := range count {
		item := f.create(t, fmt.Sprintf("Export item %03d", index))
		expected[item.WorkItemID] = item
		if index == 0 {
			first = item
		}
		if index == 1 {
			second = item
		}
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(first.WorkItemID)+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "export-dependency"}, ExpectedRevision: first.Revision, RelatedWorkItemID: second.WorkItemID, Operation: "add"})
	requireNativeStatus(t, response, http.StatusOK)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO repositories(github_node_id,github_owner,github_name,created_at,updated_at) VALUES('R_export','example-org','example-project',?,?)", formatHubTime(now), formatHubTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE projects SET repository_id=(SELECT id FROM repositories WHERE github_node_id='R_export') WHERE id=?", f.project.ID); err != nil {
		t.Fatal(err)
	}
	for id := range expected {
		source := tracker.GitHubIssueSnapshot{URL: "https://github.com/example-org/example-project/issues/" + strconv.Itoa(expected[id].Number), Title: "Source title", Body: strings.Repeat("Full source snapshot ", 40), Provenance: tracker.Provenance{Provider: "github", ExternalID: "source_" + string(id), AuthorID: "example-author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}}
		raw, err := json.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO linked_issue_sources(work_item_id,source_url,title_supplied,body_supplied,snapshot_json) VALUES(?,?,1,1,?)", id, source.URL, string(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at) VALUES(?,?,?,?,1,1,?,'{}',?,?)", "comment_"+string(id), f.project.OrganizationID, f.project.ID, id, "Full comment for "+string(id), formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "UPDATE issues SET labels_json='[\"export\"]' WHERE native_id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var upstream atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { upstream.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	f.service.config.ImportBackend = exportTestImporter{client: server.Client(), endpoint: server.URL}
	ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	for _, transport := range []string{"stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			call := hostedContextProtocol(t, f.service, ctx, transport)
			args := map[string]any{"project_id": string(f.project.ID), "open": true, "states": []string{"Todo", "In Progress"}, "labels": []string{"export", "other"}}
			seen := map[tracker.NativeWorkItemID]bool{}
			calls := 0
			for {
				calls++
				var page operatortool.WorkBulkExportPage
				raw := hostedContextData(t, call("tools/call", operatortool.WorkBulkExport, args), false)
				if err := json.Unmarshal(raw, &page); err != nil {
					t.Fatal(err)
				}
				if page.Total != count || page.GeneratedAt.IsZero() || page.Freshness != explain.SourceAvailable || page.Truncated != (page.NextCursor != "") {
					t.Fatalf("incorrect export metadata: total=%d freshness=%s truncated=%t", page.Total, page.Freshness, page.Truncated)
				}
				for _, item := range page.Items {
					if seen[item.WorkItemID] {
						t.Fatalf("duplicate %s", item.WorkItemID)
					}
					if _, ok := expected[item.WorkItemID]; !ok {
						t.Fatalf("foreign item %s", item.WorkItemID)
					}
					seen[item.WorkItemID] = true
					var detail operatortool.WorkReadResult[operatortool.NativeItem]
					if err := json.Unmarshal(hostedContextData(t, call("tools/call", operatortool.WorkItem, map[string]any{"project_id": string(f.project.ID), "reference": string(item.WorkItemID)}), false), &detail); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(item.NativeIssue, detail.Data.NativeIssue) {
						t.Fatalf("export differs from work_item for %s", item.WorkItemID)
					}
					if item.IssueContract == nil || len(item.IssueContract.ConfirmedSections) == 0 || item.LinkedSource == nil || item.LinkedSource.Snapshot == nil || item.LinkedSource.Snapshot.Body != strings.Repeat("Full source snapshot ", 40) {
						t.Fatalf("missing contract or source content for %s", item.WorkItemID)
					}
					if len(item.Comments) != 1 || item.Comments[0].Revision != 1 || item.Comments[0].ProjectID != f.project.ID || item.Comments[0].Body != "Full comment for "+string(item.WorkItemID) {
						t.Fatalf("incomplete comments for %s", item.WorkItemID)
					}
				}
				if page.NextCursor == "" {
					break
				}
				if calls >= 2 {
					t.Fatal("export requires more than two calls")
				}
				args["cursor"] = page.NextCursor
			}
			if len(seen) != count || calls != 2 {
				t.Fatalf("export returned %d in %d calls", len(seen), calls)
			}
			for _, project := range []tracker.ProjectID{foreign.project.ID, other.project.ID} {
				hostedContextData(t, call("tools/call", operatortool.WorkBulkExport, map[string]any{"project_id": string(project)}), true)
			}
		})
	}
	if upstream.Load() != 0 {
		t.Fatalf("export issued %d upstream requests", upstream.Load())
	}
	for _, test := range []struct {
		name, path, token string
		want              int
	}{
		{"allowed", f.base, f.token, http.StatusOK},
		{"project grant", foreign.base, f.token, http.StatusNotFound},
		{"organization", other.base, f.token, http.StatusNotFound},
		{"unauthenticated", f.base, "", http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, test.path+"/work-items/export", test.token, nil)
			requireNativeStatus(t, response, test.want)
			if test.want == http.StatusOK {
				var page operatortool.WorkBulkExportPage
				decodeHubResponse(t, response, &page)
				if len(page.Items) != 500 || page.Total != count || !page.Truncated {
					t.Fatal("API export missing page")
				}
			}
		})
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET archived=1 WHERE native_id=?", first.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Done'), labels_json='[\"closed\"]' WHERE native_id=?", f.project.ID, second.WorkItemID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, query string
		total       int
	}{
		{"default archive", "", 500},
		{"all archives", "archived=all", 501},
		{"only archived", "archived=true", 1},
		{"open", "open=true", 499},
		{"closed", "open=false", 1},
		{"state OR", "state=Todo&state=Done", 500},
		{"label OR", "label=export&label=closed", 500},
		{"dimensions AND", "state=Todo&label=closed", 0},
		{"closed label", "state=Done&label=closed", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var export operatortool.WorkBulkExportPage
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/export?"+test.query, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &export)
			var list tracker.NativeIssuePage
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items?"+test.query, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &list)
			if export.Total != test.total || export.Total != list.Total {
				t.Fatalf("export=%d list=%d want=%d", export.Total, list.Total, test.total)
			}
			for _, item := range export.Items {
				if item.Archived && !strings.Contains(test.query, "archived=") {
					t.Fatal("default includes archived item")
				}
			}
		})
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(second.WorkItemID), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var before tracker.NativeIssue
	decodeHubResponse(t, response, &before)
	body := "Updated body after export"
	response = performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(second.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-export"}, ExpectedRevision: before.Revision, Body: &body})
	requireNativeStatus(t, response, http.StatusOK)
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/export?open=false", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var edited operatortool.WorkBulkExportPage
	decodeHubResponse(t, response, &edited)
	if len(edited.Items) != 1 || edited.Items[0].Revision <= before.Revision || edited.Items[0].Body != body {
		t.Fatal("export failed to expose later edit")
	}
	if upstream.Load() != 0 {
		t.Fatalf("read paths issued %d GitHub requests", upstream.Load())
	}

}
