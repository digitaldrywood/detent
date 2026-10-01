package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
	var ctx context.Context
	f.service.echo.POST("/api/v2/organizations/:organization/operator-read-fixture", func(c echo.Context) error { ctx = c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	response = performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/operator-read-fixture", f.token, map[string]any{})
	requireNativeStatus(t, response, http.StatusOK)
	executor := nativeOperatorExecutor{service: f.service}
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
	for _, tool := range []string{operatortool.WorkItem, operatortool.WorkRelationships, operatortool.WorkHistory, operatortool.WorkRuns, operatortool.WorkReferences, operatortool.WorkExport, operatortool.BoardActivity} {
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
		if _, err := call(tool, foreignArgs); !errors.Is(err, explain.ErrNotFound) {
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
	if _, err := call(operatortool.BoardReceipt, args()); !errors.Is(err, operatortool.ErrReadUnavailable) {
		t.Fatalf("absent service=%v", err)
	}
	listArgs = map[string]any{"project_id": string(f.project.ID), "query": "absent"}
	result, err = call(operatortool.WorkList, listArgs)
	if err != nil || !strings.Contains(string(result.Content), `"items":[]`) {
		t.Fatalf("empty=%s err=%v", result.Content, err)
	}
}
