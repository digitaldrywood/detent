package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestParseTestPlanChecklist(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, body string
		want       []string
	}{
		{"empty", "", []string{}},
		{"plain text", "Quote builder\n- not a checkbox\n[ ] missing bullet", []string{}},
		{"bullets", "## Screens\n- [ ] Quote builder: create, edit, send\n* [x] Login\n+ [X] Settings  \n", []string{"Quote builder: create, edit, send", "Login", "Settings"}},
		{"nested and numbered", "1. [ ] Checkout\n  - [ ] Pay by card\r\n2) [x] Receipt", []string{"Checkout", "Pay by card", "Receipt"}},
		{"fenced examples are skipped", "- [ ] Before\n```md\n- [ ] Example only\n```\n~~~\n- [ ] Also example\n~~~\n- [ ] After", []string{"Before", "After"}},
		{"empty item text", "- [ ] \n- [ ]\n- [ ] Real", []string{"Real"}},
		{"longer fence closes only on an equal or longer run", "````\n- [ ] Inside\n```\n- [ ] Still inside\n````\n- [ ] After", []string{"After"}},
		{"closing fence takes no info string", "```\n- [ ] Inside\n``` md\n- [ ] Still inside\n```\n- [ ] After", []string{"After"}},
		{"tilde fence ignores backtick runs", "~~~\n```\n- [ ] Inside\n~~~\n- [ ] After", []string{"After"}},
		{"indented code is not a checklist", "    - [ ] Code sample\n\t- [ ] Tab code\n   - [ ] Three spaces", []string{"Three spaces"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := parseTestPlanChecklist(test.body); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("items=%q, want %q", got, test.want)
			}
		})
	}
}

func testPlanFixture(t *testing.T, name string) (nativeFixture, func(string, map[string]any) (json.RawMessage, error)) {
	t.Helper()
	f := newNativeFixture(t, nil, "", name)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	states := []tracker.NativeState{
		{Name: "Todo", Dispatchable: true, Transitions: []string{"Backlog", "Done"}},
		{Name: "Backlog", Transitions: []string{"Todo", "Blocked", "Done"}},
		{Name: "Blocked", Transitions: []string{"Backlog", "Todo"}},
		{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
	}
	if err := applyNativeProjectStates(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, states, f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/api/v2/organizations/:organization/"+name+"-test", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), name, "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/"+name+"-test", f.token, map[string]any{}), http.StatusOK)
	ctx := <-contexts
	call := func(tool string, arguments map[string]any) (json.RawMessage, error) {
		t.Helper()
		arguments["project_id"] = f.project.ID
		raw, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{Name: tool, Arguments: raw})
		return result.Content, err
	}
	return f, call
}

func requireRequestRefusal(t *testing.T, err error, phrase string) {
	t.Helper()
	var refusal *operatortool.RequestError
	var conflict *operatortool.ConflictError
	switch {
	case errors.As(err, &refusal) && refusal.Code == "invalid_request" && strings.Contains(refusal.Message, phrase):
	case errors.As(err, &conflict) && conflict.Code == "test_item_changed" && strings.Contains(conflict.Message, phrase):
	default:
		t.Fatalf("refusal=%v, want message containing %q", err, phrase)
	}
}

func TestNativeTestPlanLanes(t *testing.T) {
	f, call := testPlanFixture(t, "test-plan-lanes")
	create := func(t *testing.T, key, state string, labels []string) *tracker.NativeIssue {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Lane " + key, Body: "- [ ] Quote builder\n" + issueContractTestSections, State: state, Labels: labels})
		if response.Code != http.StatusOK {
			var failure apiErrorResponse
			decodeHubResponse(t, response, &failure)
			if response.Code != http.StatusUnprocessableEntity || failure.Code != "invalid_request" || !strings.Contains(failure.Message, "non-dispatchable, nonterminal lane") {
				t.Fatalf("create=%d %s", response.Code, response.Body.String())
			}
			return nil
		}
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		return &issue
	}
	plan := []string{"test-plan"}
	for _, test := range []struct {
		name, state string
		labels      []string
		want        string
	}{
		{"plan defaults to Backlog", "", plan, "Backlog"},
		{"plan filed in Blocked", "Blocked", plan, "Blocked"},
		{"plan refused in active lane", "Todo", plan, ""},
		{"plan refused in terminal lane", "Done", plan, ""},
		{"ordinary issue keeps default lane", "", nil, "Todo"},
		{"ordinary issue keeps requested lane", "Todo", []string{"qa"}, "Todo"},
	} {
		t.Run("create/"+test.name, func(t *testing.T) {
			issue := create(t, test.name, test.state, test.labels)
			if (issue == nil) != (test.want == "") || issue != nil && issue.State != test.want {
				t.Fatalf("issue=%#v, want state %q", issue, test.want)
			}
		})
	}
	raw, err := call(operatortool.FileIssue, map[string]any{"request_id": "mcp-plan", "title": "MCP plan", "description": "- [ ] Login\n" + issueContractTestSections, "labels": plan})
	var filed operatortool.WorkReadResult[tracker.NativeIssue]
	if err != nil || json.Unmarshal(raw, &filed) != nil || filed.Data.State != "Backlog" {
		t.Fatalf("MCP plan=%s err=%v", raw, err)
	}

	for _, test := range []struct {
		name, state string
		allowed     bool
	}{
		{"label refused in active lane", "Todo", false},
		{"label applied in Backlog", "Backlog", true},
	} {
		t.Run("label/"+test.name, func(t *testing.T) {
			issue := create(t, "label "+test.name, test.state, nil)
			response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "label " + test.name}, ExpectedRevision: issue.Revision, Labels: &plan})
			if !test.allowed {
				requireNativeStatus(t, response, http.StatusUnprocessableEntity)
				var failure apiErrorResponse
				decodeHubResponse(t, response, &failure)
				if !strings.Contains(failure.Message, "before labeling it test-plan") {
					t.Fatalf("failure=%#v", failure)
				}
				_, err := call(operatortool.EditItem, map[string]any{"request_id": "mcp label " + test.name, "identifier": string(issue.WorkItemID), "expected_revision": "1", "labels": plan})
				requireRequestRefusal(t, err, "before labeling it test-plan")
				return
			}
			requireNativeStatus(t, response, http.StatusOK)
		})
	}

	issue := create(t, "transitions", "", plan)
	move := func(t *testing.T, state string) *httptest.ResponseRecorder {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "move-" + state + "-" + strconv.FormatInt(int64(issue.Revision), 10)}, ExpectedRevision: issue.Revision, State: state, Reason: "user_requested"})
		if response.Code == http.StatusOK {
			decodeHubResponse(t, response, issue)
		}
		return response
	}
	refused := move(t, "Todo")
	requireNativeStatus(t, refused, http.StatusUnprocessableEntity)
	var failure apiErrorResponse
	decodeHubResponse(t, refused, &failure)
	if !strings.Contains(failure.Message, "remove the test-plan label before moving it to Todo") {
		t.Fatalf("transition failure=%#v", failure)
	}
	_, err = call(operatortool.MoveItem, map[string]any{"request_id": "mcp-move", "identifier": string(issue.WorkItemID), "target_state": "Todo", "expected_revision": "1"})
	requireRequestRefusal(t, err, "remove the test-plan label")
	requireNativeStatus(t, move(t, "Done"), http.StatusUnprocessableEntity)
	requireNativeStatus(t, move(t, "Blocked"), http.StatusOK)
	if issue.State != "Blocked" {
		t.Fatalf("state=%s", issue.State)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(issue.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "remove-label"}, ExpectedRevision: issue.Revision, Labels: &[]string{}})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, issue)
	requireNativeStatus(t, move(t, "Todo"), http.StatusOK)
	if issue.State != "Todo" {
		t.Fatalf("ordinary workflow not restored: state=%s", issue.State)
	}
}

func TestNativeTestPlanItemResults(t *testing.T) {
	f, call := testPlanFixture(t, "test-plan-results")
	body := "Round one.\n- [ ] Quote builder: create\n- [ ] Quote builder: send\n- [ ] Invoices\n- [ ] Settings\n" + issueContractTestSections
	raw, err := call(operatortool.FileIssue, map[string]any{"request_id": "plan", "title": "PyroApex plan", "description": body, "labels": []string{"test-plan"}})
	var plan operatortool.WorkReadResult[tracker.NativeIssue]
	if err != nil || json.Unmarshal(raw, &plan) != nil {
		t.Fatalf("plan=%s err=%v", raw, err)
	}
	raw, err = call(operatortool.FileIssue, map[string]any{"request_id": "finding", "title": "Send fails", "description": issueContractTestSections})
	var finding operatortool.WorkReadResult[tracker.NativeIssue]
	if err != nil || json.Unmarshal(raw, &finding) != nil {
		t.Fatalf("finding=%s err=%v", raw, err)
	}
	ordinary := f.create(t, "Ordinary checklist")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(ordinary.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "ordinary-body"}, ExpectedRevision: ordinary.Revision, Body: new("- [ ] Looks like a plan\n" + issueContractTestSections)}), http.StatusOK)

	identifier := string(plan.Data.WorkItemID)
	texts := []string{"Quote builder: create", "Quote builder: send", "Invoices", "Settings"}
	for _, test := range []struct {
		name      string
		arguments map[string]any
		refusal   string
	}{
		{"pass", map[string]any{"item": 1, "result": "pass"}, ""},
		{"fail links finding", map[string]any{"item": 2, "result": "fail", "note": "Send button does nothing", "related_issue": finding.Data.Number}, ""},
		{"note of 2000 characters", map[string]any{"item": 3, "result": "blocked", "note": strings.Repeat("é", 2000)}, ""},
		{"note over 2000 characters", map[string]any{"item": 3, "result": "blocked", "note": strings.Repeat("é", 2001)}, "at most 2000 characters"},
		{"blocked", map[string]any{"item": 3, "result": "blocked", "note": "Needs invoice data"}, ""},
		{"stale item text", map[string]any{"item": 1, "text": "Quote builder: send", "result": "pass"}, "changed since it was read"},
		{"related issue only with fail", map[string]any{"item": 3, "result": "blocked", "related_issue": finding.Data.Number}, "only with a fail result"},
		{"later result replaces earlier", map[string]any{"item": 1, "result": "fail"}, ""},
		{"latest result wins", map[string]any{"item": 1, "result": "pass"}, ""},
		{"item beyond checklist", map[string]any{"item": 5, "result": "pass"}, "between 1 and 4"},
		{"unknown related issue", map[string]any{"item": 2, "result": "fail", "related_issue": 999}, "does not name an issue"},
		{"plan related to itself", map[string]any{"item": 2, "result": "fail", "related_issue": plan.Data.Number}, "cannot be related to itself"},
		{"ordinary issue", map[string]any{"identifier": string(ordinary.WorkItemID), "item": 1, "result": "pass"}, "labeled test-plan"},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := map[string]any{"request_id": test.name, "identifier": identifier}
			if index, _ := test.arguments["item"].(int); index >= 1 && index <= len(texts) {
				arguments["text"] = texts[index-1]
			} else {
				arguments["text"] = "Looks like a plan"
			}
			for key, value := range test.arguments {
				arguments[key] = value
			}
			raw, err := call(operatortool.UpdateTestItem, arguments)
			if test.refusal != "" {
				requireRequestRefusal(t, err, test.refusal)
				return
			}
			var recorded operatortool.WorkReadResult[tracker.NativeIssue]
			if err != nil || json.Unmarshal(raw, &recorded) != nil || recorded.Data.TestPlan == nil {
				t.Fatalf("record=%s err=%v", raw, err)
			}
		})
	}

	raw, err = call(operatortool.WorkItem, map[string]any{"reference": identifier})
	var item operatortool.WorkReadResult[operatortool.NativeItem]
	if err != nil || json.Unmarshal(raw, &item) != nil || item.Data.TestPlan == nil {
		t.Fatalf("work_item=%s err=%v", raw, err)
	}
	progress := item.Data.TestPlan
	if progress.Passed != 1 || progress.Failed != 1 || progress.Blocked != 1 || progress.Untested != 1 || len(progress.Items) != 4 {
		t.Fatalf("progress=%#v", progress)
	}
	for _, test := range []struct {
		index        int
		text, result string
		note         string
		linked       []tracker.NativeWorkItemID
		recorded     bool
	}{
		{1, "Quote builder: create", "pass", "", nil, true},
		{2, "Quote builder: send", "fail", "Send button does nothing", []tracker.NativeWorkItemID{finding.Data.WorkItemID}, true},
		{3, "Invoices", "blocked", "Needs invoice data", nil, true},
		{4, "Settings", "", "", nil, false},
	} {
		entry := progress.Items[test.index-1]
		linked := []tracker.NativeWorkItemID{}
		for _, link := range entry.LinkedIssues {
			linked = append(linked, link.WorkItemID)
		}
		if entry.Index != test.index || entry.Text != test.text || entry.Result != test.result || entry.Note != test.note || len(linked) != len(test.linked) || len(test.linked) > 0 && linked[0] != test.linked[0] {
			t.Fatalf("item %d=%#v", test.index, entry)
		}
		if (entry.Tester != nil && entry.Tester.PrincipalID != "" && entry.RecordedAt != nil) != test.recorded {
			t.Fatalf("item %d tester=%#v at=%v", test.index, entry.Tester, entry.RecordedAt)
		}
	}
	raw, err = call(operatortool.WorkRelationships, map[string]any{"reference": identifier})
	var relationships operatortool.WorkReadResult[struct {
		Related []tracker.NativeDependency `json:"related"`
	}]
	if err != nil || json.Unmarshal(raw, &relationships) != nil || len(relationships.Data.Related) != 1 || relationships.Data.Related[0].ID != finding.Data.WorkItemID {
		t.Fatalf("relationships=%s err=%v", raw, err)
	}
	var detail tracker.NativeIssue
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(ordinary.WorkItemID), f.token, nil), &detail)
	if detail.TestPlan != nil {
		t.Fatalf("ordinary checklist issue has a test plan: %#v", detail.TestPlan)
	}

	path := f.base + "/work-items/" + identifier
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &detail)
	if detail.TestPlan == nil || !reflect.DeepEqual(detail.TestPlan.Items[1].LinkedIssues, progress.Items[1].LinkedIssues) {
		t.Fatalf("detail progress=%#v", detail.TestPlan)
	}
	edited := strings.Replace(body, "Quote builder: create", "Quote builder: create and copy", 1)
	response := performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-plan"}, ExpectedRevision: detail.Revision, Body: &edited})
	requireNativeStatus(t, response, http.StatusOK)
	detail = tracker.NativeIssue{}
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &detail)
	if detail.TestPlan.Items[0].Result != "" || detail.TestPlan.Items[1].Result != "fail" || detail.TestPlan.Untested != 2 {
		t.Fatalf("edited item kept a stale result: %#v", detail.TestPlan)
	}
	reordered := "- [ ] Login\n- [ ] Quote builder: create and copy\n- [ ] Settings\n- [ ] Quote builder: send\n- [ ] Invoices\n- [ ] Invoices\n" + issueContractTestSections
	response = performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reorder-plan"}, ExpectedRevision: detail.Revision, Body: &reordered})
	requireNativeStatus(t, response, http.StatusOK)
	detail = tracker.NativeIssue{}
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &detail)
	for _, test := range []struct {
		index  int
		result string
		links  int
	}{
		{1, "", 0},
		{2, "", 0},
		{3, "", 0},
		{4, "fail", 1},
		{5, "", 0},
		{6, "", 0},
	} {
		entry := detail.TestPlan.Items[test.index-1]
		if entry.Result != test.result || len(entry.LinkedIssues) != test.links {
			t.Fatalf("reordered item %d=%#v", test.index, entry)
		}
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "unlink-finding"}, ExpectedRevision: detail.Revision, RelatedWorkItemID: finding.Data.WorkItemID, RelationshipType: "related", Operation: "remove"})
	requireNativeStatus(t, response, http.StatusOK)
	detail = tracker.NativeIssue{}
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &detail)
	if entry := detail.TestPlan.Items[3]; entry.Result != "fail" || len(entry.LinkedIssues) != 0 {
		t.Fatalf("removed relationship still linked: %#v", entry)
	}
	response = performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "remove-label"}, ExpectedRevision: detail.Revision, Labels: &[]string{}})
	requireNativeStatus(t, response, http.StatusOK)
	detail = tracker.NativeIssue{}
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &detail)
	if detail.TestPlan != nil {
		t.Fatalf("unlabeled issue kept a test plan: %#v", detail.TestPlan)
	}
	var history tracker.Page[tracker.CollaborationEvent]
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/history", f.token, nil), &history)
	results := slices.DeleteFunc(slices.Clone(history.Items), func(event tracker.CollaborationEvent) bool { return event.Data.TestItem == nil })
	if len(results) != 6 || results[1].Data.TestItem.Result != "fail" || results[1].Data.RelatedWorkItemID != finding.Data.WorkItemID || results[1].Actor.PrincipalID == "" {
		t.Fatalf("recorded results missing from history: %#v", results)
	}
}
