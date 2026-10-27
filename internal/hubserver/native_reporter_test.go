package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReporterReadIsolation(t *testing.T) {
	v := newVerificationFixture(t)
	issues := map[string]tracker.NativeIssue{}
	for _, user := range []string{"reporter", "tester"} {
		r := v.request(t, v.users[user], http.MethodPost, v.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "file-" + user}, Type: "bug", Title: "Shared search phrase " + user, Body: reporterBugSections})
		requireNativeStatus(t, r, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, r, &issue)
		issues[user] = issue
	}
	own, other := issues["reporter"], issues["tester"]
	operatorSQL(t, v.hostedSecurityFixture, "UPDATE issues SET actor_kind=NULL,actor_principal_id=NULL WHERE native_id IN (?,?)", own.WorkItemID, other.WorkItemID)
	contexts := make(chan context.Context, 1)
	v.service.echo.POST("/reporter-read-context", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "reporter-read", "test")
		return c.NoContent(http.StatusOK)
	}, v.service.operatorAuthority)
	requireNativeStatus(t, v.request(t, v.users["reporter"], http.MethodPost, "/organizations/org_security/reporter-read-context", nil), http.StatusOK)
	ctx := <-contexts
	call := hostedContextProtocol(t, v.service, ctx, "http")
	for _, role := range []string{"owner", "admin", "viewer"} {
		v.users[role] = v.user(t, role, role, role+"@example.test", "read", "")
		v.grant(t, v.users[role], true, false)
	}
	drainTestNativeBoard(t, v.service)
	for _, user := range []string{"reporter", "tester", "member", "owner", "admin", "viewer"} {
		for _, suffix := range []string{"", "?include=work", "?include=summary", "?include=board", "?fields=picker", "?q=Shared", "?include=board&q=Shared", "?created_by=me"} {
			t.Run(user+suffix, func(t *testing.T) {
				r := v.request(t, v.users[user], http.MethodGet, v.base+"/work-items"+suffix, nil)
				requireNativeStatus(t, r, http.StatusOK)
				var page tracker.NativeIssuePage
				decodeHubResponse(t, r, &page)
				want := 1
				if user != "reporter" && user != "tester" {
					want = 2
					if suffix == "?created_by=me" {
						want = 0
					}
				}
				if page.Total != want || len(page.Items) != want {
					t.Fatalf("page=%s", r.Body.String())
				}
				for _, item := range page.Items {
					if (user == "reporter" || user == "tester") && (item.WorkItemID != issues[user].WorkItemID || item.ReporterStatus != "submitted") {
						t.Fatalf("item=%+v", item)
					}
				}
				if page.Work != nil {
					total := 0
					for _, lane := range page.Work.Lanes {
						total += lane.Total
					}
					if total != want {
						t.Fatalf("lanes=%+v", page.Work.Lanes)
					}
				}
			})
		}
	}
	for _, item := range []tracker.NativeIssue{own, other} {
		expected := http.StatusOK
		if item.WorkItemID == other.WorkItemID {
			expected = http.StatusNotFound
		}
		for _, suffix := range []string{"", "?include=change", "/comments", "/history", "/attempts", "/versions/1", "/runtime"} {
			r := v.request(t, v.users["reporter"], http.MethodGet, v.base+"/work-items/"+string(item.WorkItemID)+suffix, nil)
			if expected == http.StatusNotFound || suffix != "/runtime" {
				requireNativeStatus(t, r, expected)
			}
		}
		for _, reference := range []string{string(item.WorkItemID), strconv.Itoa(item.Number)} {
			reply := call("tools/call", operatortool.WorkItem, map[string]any{"project_id": v.project, "reference": reference})
			var result struct {
				IsError bool `json:"isError"`
			}
			if err := json.Unmarshal(reply.Result, &result); err != nil {
				t.Fatal(err)
			}
			if (reply.Error == nil && !result.IsError) != (expected == http.StatusOK) {
				t.Fatalf("%s: %+v", reference, reply)
			}
		}
		for _, name := range []string{operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkRelationships, operatortool.WorkRuns, operatortool.WorkReferences, operatortool.WorkExport} {
			reply := call("tools/call", name, map[string]any{"project_id": v.project, "reference": item.WorkItemID})
			var result struct {
				IsError bool `json:"isError"`
			}
			if err := json.Unmarshal(reply.Result, &result); err != nil {
				t.Fatal(err)
			}
			if (reply.Error == nil && !result.IsError) != (expected == http.StatusOK) {
				t.Fatalf("%s %s: %+v", name, item.WorkItemID, reply)
			}
		}
	}
	for _, name := range []string{operatortool.WorkList, operatortool.WorkBulkExport, operatortool.BoardState, operatortool.Dashboard} {
		reply := call("tools/call", name, map[string]any{"project_id": v.project})
		if reply.Error != nil {
			t.Fatalf("%s: %+v", name, reply.Error)
		}
		if !strings.Contains(string(reply.Result), string(own.WorkItemID)) || strings.Contains(string(reply.Result), string(other.WorkItemID)) {
			t.Fatalf("%s: %s", name, reply.Result)
		}
	}
	r := v.request(t, v.users["reporter"], http.MethodGet, v.base+"/work-items?created_by="+url.QueryEscape(other.Actor.PrincipalID), nil)
	requireNativeStatus(t, r, http.StatusOK)
	var empty tracker.NativeIssuePage
	decodeHubResponse(t, r, &empty)
	if empty.Total != 0 || len(empty.Items) != 0 {
		t.Fatalf("created_by bypass: %s", r.Body.String())
	}
	r = v.request(t, v.users["reporter"], http.MethodGet, "/api/v2/organizations/org_security/work-items/"+string(other.WorkItemID), nil)
	requireNativeStatus(t, r, http.StatusNotFound)
}

func TestReporterTriageEdits(t *testing.T) {
	v := newVerificationFixture(t)
	for _, user := range []string{"reporter", "tester"} {
		r := v.request(t, v.users[user], http.MethodPost, v.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: user}, Type: "bug", Title: "Editable report " + user, Body: reporterBugSections})
		requireNativeStatus(t, r, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, r, &issue)
		path := v.base + "/work-items/" + string(issue.WorkItemID)
		if user == "tester" {
			requireNativeStatus(t, v.request(t, v.users["reporter"], http.MethodPatch, path, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "other"}, ExpectedRevision: issue.Revision, Title: new("Changed")}), http.StatusNotFound)
			continue
		}
		for _, field := range []string{"labels", "priority", "assignees", "type", "state"} {
			patch := map[string]any{"idempotency_key": "restricted-" + field, "expected_revision": fmt.Sprint(issue.Revision), "title": "Changed"}
			switch field {
			case "labels", "assignees":
				patch[field] = []string{}
			case "priority":
				patch[field] = nil
			default:
				patch[field] = "Todo"
			}
			r := v.request(t, v.users[user], http.MethodPatch, path, patch)
			if r.Code < 400 {
				t.Fatalf("%s allowed: %s", field, r.Body.String())
			}
		}
		body := reporterBugSections + issueContractTestSections
		r = v.request(t, v.users[user], http.MethodPatch, path, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "triage-edit"}, ExpectedRevision: issue.Revision, Title: new("Corrected title"), Body: &body})
		requireNativeStatus(t, r, http.StatusOK)
		decodeHubResponse(t, r, &issue)
		if issue.Title != "Corrected title" || issue.Body != body || issue.ReporterStatus != "submitted" {
			t.Fatalf("edit=%+v", issue)
		}
		if issue.IssueContract != nil && len(issue.IssueContract.ConfirmedSections) != 0 {
			t.Fatalf("confirmed engineering contract: %+v", issue.IssueContract)
		}
		issue = v.move(t, issue, "Backlog")
		r = v.request(t, v.users[user], http.MethodPatch, path, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "late-edit"}, ExpectedRevision: issue.Revision, Title: new("Late title")})
		requireNativeStatus(t, r, http.StatusForbidden)
		if !strings.Contains(r.Body.String(), "already with the team") || !strings.Contains(r.Body.String(), "comment") {
			t.Fatalf("message=%s", r.Body.String())
		}
	}
}

func TestReporterLinksAttachmentsAndBoardStream(t *testing.T) {
	v := newVerificationFixture(t)
	items := map[string]tracker.NativeIssue{}
	records := map[string]attachment.Metadata{}
	for _, user := range []string{"reporter", "tester"} {
		record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "example.png", ContentType: "image/png", Size: 100, Width: 1, Height: 1, SHA256: strings.Repeat("a", 64)}
		r := v.request(t, v.users[user], http.MethodPost, v.base+"/attachment-metadata", record)
		requireNativeStatus(t, r, http.StatusCreated)
		decodeHubResponse(t, r, &record)
		records[user] = record
		r = v.request(t, v.users[user], http.MethodPost, v.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: user}, Type: "bug", Title: "Linked report " + user, Body: reporterBugSections + "\n![Image](attachment:" + record.ID + ")"})
		requireNativeStatus(t, r, http.StatusOK)
		var item tracker.NativeIssue
		decodeHubResponse(t, r, &item)
		items[user] = item
	}
	own, other := items["reporter"], items["tester"]
	for _, relationship := range []string{"dependency", "related"} {
		r := v.request(t, v.users["member"], http.MethodPost, v.base+"/work-items/"+string(own.WorkItemID)+"/dependencies", tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: relationship}, ExpectedRevision: own.Revision, RelatedWorkItemID: other.WorkItemID, RelationshipType: relationship, Operation: "add"})
		requireNativeStatus(t, r, http.StatusOK)
		decodeHubResponse(t, r, &own)
	}
	r := v.request(t, v.users["reporter"], http.MethodGet, v.base+"/work-items/"+string(own.WorkItemID), nil)
	requireNativeStatus(t, r, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, r, &issue)
	if len(issue.Dependencies)+len(issue.Blockers)+len(issue.Dependents)+len(issue.Related) != 0 {
		t.Fatalf("links=%s", r.Body.String())
	}
	for _, suffix := range []string{"/history", "/versions/1"} {
		r := v.request(t, v.users["reporter"], http.MethodGet, v.base+"/work-items/"+string(own.WorkItemID)+suffix, nil)
		requireNativeStatus(t, r, http.StatusOK)
		if strings.Contains(r.Body.String(), string(other.WorkItemID)) {
			t.Fatalf("%s leaked: %s", suffix, r.Body.String())
		}
	}
	for _, user := range []string{"reporter", "tester"} {
		r := v.request(t, v.users["reporter"], http.MethodGet, v.base+"/attachment-metadata/"+records[user].ID, nil)
		want := http.StatusOK
		if user == "tester" {
			want = http.StatusNotFound
		}
		requireNativeStatus(t, r, want)
	}

	for index, primary := range []tracker.NativeIssue{other, own} {
		r := v.request(t, v.users["member"], http.MethodPost, v.base+"/work-items/"+string(primary.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("shared-change-%d", index)}, Title: "Shared change", Body: "Change details", LinkedIssues: []tracker.NativeWorkItemID{own.WorkItemID, other.WorkItemID}})
		requireNativeStatus(t, r, http.StatusOK)
	}
	r = v.request(t, v.users["reporter"], http.MethodGet, v.base+"/work-items/"+string(own.WorkItemID)+"/changes", nil)
	requireNativeStatus(t, r, http.StatusOK)
	if strings.Contains(r.Body.String(), string(other.WorkItemID)) {
		t.Fatalf("change references leaked: %s", r.Body.String())
	}
	var changes []tracker.ChangeRequest
	decodeHubResponse(t, r, &changes)
	if len(changes) != 1 || changes[0].WorkItemID != own.WorkItemID || len(changes[0].LinkedIssues) != 1 {
		t.Fatalf("changes=%+v", changes)
	}
	contexts := make(chan nativeScope, 1)
	v.service.echo.GET(nativeBase+"/reporter-board-scope", func(c echo.Context) error { contexts <- nativeRequestScope(c); return c.NoContent(http.StatusOK) }, v.service.requireNativeScope(apiScopeOperator))
	requireNativeStatus(t, v.request(t, v.users["reporter"], http.MethodGet, v.base+"/reporter-board-scope", nil), http.StatusOK)
	scope := <-contexts
	scope.organization, scope.project = "org_security", v.project
	drainTestNativeBoard(t, v.service)
	frame, err := readNativeBoardFrame(t.Context(), v.service.database.reader, scope, 0)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), string(other.WorkItemID)) {
		t.Fatalf("stream leak: %s", raw)
	}
	seen := false
	for _, delta := range frame.Deltas {
		for _, card := range []*tracker.NativeBoardCard{delta.Previous, delta.Current} {
			if card != nil {
				seen = true
				if card.Issue.WorkItemID != own.WorkItemID || card.Issue.ReporterStatus != "submitted" {
					t.Fatalf("card=%+v", card)
				}
			}
		}
	}
	if !seen {
		t.Fatalf("own issue missing: %s", raw)
	}
}

func TestReporterStatus(t *testing.T) {
	for _, test := range []struct{ state, want string }{{"Triage", "submitted"}, {"Backlog", "planned"}, {"Todo", "planned"}, {"In Progress", "in_progress"}, {"In Review", "in_progress"}, {"Rework", "in_progress"}, {"Merging", "in_progress"}, {"Blocked", "in_progress"}, {"Done", "done"}, {"Cancelled", "cancelled"}} {
		t.Run(test.state, func(t *testing.T) {
			if got := reporterStatus(test.state); got != test.want {
				t.Fatalf("got %q, want %q", got, test.want)
			}
		})
	}
}

func TestReporterIdentityAcrossEntryPoints(t *testing.T) {
	for _, deployment := range []string{"dedicated", "shared"} {
		t.Run(deployment, func(t *testing.T) {
			f, ctx := newHostedKeyMCPFixture(t, deployment, "reporter")
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyNativeProjectStates(t.Context(), tx, nativeScope{organization: "org_security", project: f.project}, nativeFixtureStates(), f.service.config.now()); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			operatorSQL(t, f.hostedSecurityFixture, "UPDATE projects SET duplicate_challenge=0 WHERE id=?", f.project)
			catalog, err := operatorCatalog(ctx)
			if err != nil {
				t.Fatal(err)
			}
			key, err := f.service.createHostedAPIKeyFor(ctx, catalog.credential, hostedKeyRequest{Name: "Reporter key", Scope: apikey.ScopeWrite, Days: 1, Projects: []string{string(f.project)}})
			if err != nil {
				t.Fatal(err)
			}
			record := attachment.Metadata{ID: conversation.NewAttachmentID(), Name: "legacy.png", ContentType: "image/png", Size: 10, Width: 1, Height: 1, SHA256: strings.Repeat("a", 64)}
			upload, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			attachmentResponse := reporterEntryRequest(t, f, deployment, key.Token, "REST-key", http.MethodPost, f.base+"/attachment-metadata", upload, nil)
			if deployment == "shared" {
				requireNativeStatus(t, attachmentResponse, http.StatusCreated)
				operatorSQL(t, f.hostedSecurityFixture, "UPDATE attachments SET uploader=? WHERE id=?", key.ID, record.ID)
				requireNativeStatus(t, reporterEntryRequest(t, f, deployment, "", "web-session", http.MethodGet, f.base+"/attachment-metadata/"+record.ID, nil, nil), http.StatusOK)
			} else {
				requireNativeStatus(t, attachmentResponse, http.StatusNotFound)
			}
			var own tracker.NativeIssue
			for _, transport := range []string{"web-session", "REST-key"} {
				raw, err := json.Marshal(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: transport}, Type: "bug", Title: "Report from " + transport, Body: reporterBugSections})
				if err != nil {
					t.Fatal(err)
				}
				r := reporterEntryRequest(t, f, deployment, key.Token, transport, http.MethodPost, f.base+"/work-items", raw, nil)
				requireNativeStatus(t, r, http.StatusOK)
				decodeHubResponse(t, r, &own)
				if own.Actor.PrincipalID != (nativeScope{organization: "org_security", credential: catalog.credential}).actor().PrincipalID || own.ReporterStatus != "submitted" {
					t.Fatalf("identity=%+v principal=%q", own.Actor, catalog.credential.HostedPrincipal)
				}
				raw, err = json.Marshal(tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "edit-" + transport}, ExpectedRevision: own.Revision, Title: new("Corrected report")})
				if err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, reporterEntryRequest(t, f, deployment, key.Token, transport, http.MethodPatch, f.base+"/work-items/"+string(own.WorkItemID), raw, nil), http.StatusOK)
			}
			otherFixture := f
			otherFixture.user = f.hostedSecurityFixture.user(t, "second-reporter", "reporter", "second@example.test", "write", "")
			raw, err := json.Marshal(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "second-reporter"}, Type: "bug", Title: "Private second report", Body: reporterBugSections})
			if err != nil {
				t.Fatal(err)
			}
			r := reporterEntryRequest(t, otherFixture, deployment, "", "web-session", http.MethodPost, f.base+"/work-items", raw, nil)
			requireNativeStatus(t, r, http.StatusOK)
			var other tracker.NativeIssue
			decodeHubResponse(t, r, &other)
			for _, transport := range []string{"web-session", "REST-key"} {
				r := reporterEntryRequest(t, f, deployment, key.Token, transport, http.MethodGet, f.base+"/work-items", nil, nil)
				requireNativeStatus(t, r, http.StatusOK)
				var page tracker.NativeIssuePage
				decodeHubResponse(t, r, &page)
				if page.Total != 2 || len(page.Items) != 2 || strings.Contains(r.Body.String(), string(other.WorkItemID)) {
					t.Fatalf("identity list=%s", r.Body.String())
				}
				requireNativeStatus(t, reporterEntryRequest(t, f, deployment, key.Token, transport, http.MethodGet, f.base+"/work-items/"+string(other.WorkItemID), nil, nil), http.StatusNotFound)
				args := map[string]any{"project_id": f.project, "reference": other.WorkItemID}
				raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.WorkItem, "arguments": args, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]string{"name": "Reporter test", "version": "1"}}}})
				if err != nil {
					t.Fatal(err)
				}
				response := reporterEntryRequest(t, f, deployment, key.Token, transport, http.MethodPost, "/api/v2/organizations/org_security/mcp", raw, map[string]string{"Mcp-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/call", "Mcp-Name": operatortool.WorkItem})
				requireNativeStatus(t, response, http.StatusOK)
				if !strings.Contains(response.Body.String(), "work item not found in this project") {
					t.Fatalf("MCP hidden item=%s", response.Body.String())
				}
			}
			if deployment == "dedicated" {
				drainTestNativeBoard(t, f.service)
				scanner := openHostedTestStream(t, f.hostedSecurityFixture, f.user, "?board=true")
				kind, data := readHostedEvent(t, scanner)
				if kind != "activity" || strings.Contains(data, string(other.WorkItemID)) || !strings.Contains(data, string(own.WorkItemID)) {
					t.Fatalf("SSE=%s %s", kind, data)
				}
				requireNativeStatus(t, f.request(t, f.user, http.MethodGet, "/projects/"+string(f.project)+"/events?work_item="+string(other.WorkItemID), nil), http.StatusNotFound)
			}
		})
	}
}

func TestReporterAggregateReads(t *testing.T) {
	v := newVerificationFixture(t)
	own := v.file(t, "reporter", "own", nil, "")
	other := v.file(t, "tester", "other", nil, "")
	now := v.service.config.now().UTC()
	operatorSQL(t, v.hostedSecurityFixture, "INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id) VALUES ('reporter-fixture','reporter-fixture',1,'test',?,?,?,'org_security')", formatHubTime(now), formatHubTime(now), formatHubTime(now))
	for index, item := range []tracker.NativeIssue{own, other} {
		label := "own-label"
		if index == 1 {
			label = "private-label"
		}
		labels, err := json.Marshal([]string{label})
		if err != nil {
			t.Fatal(err)
		}
		operatorSQL(t, v.hostedSecurityFixture, "UPDATE issues SET labels_json=? WHERE native_id=?", string(labels), item.WorkItemID)
		key := fmt.Sprintf("reporter-attempt-%d", index)
		lease, err := v.service.database.db.ExecContext(t.Context(), "INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT ?,id,'reporter-fixture',?,?,?,?,?,?,? FROM issues WHERE native_id=?", key, key, formatHubTime(now), formatHubTime(now.Add(-time.Minute)), formatHubTime(now), formatHubTime(now), formatHubTime(now.Add(-time.Minute)), formatHubTime(now), item.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		fence, err := lease.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		operatorSQL(t, v.hostedSecurityFixture, "INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) VALUES (?,?,?,?,?,?,?,1,'succeeded','{}',?,?)", key, "org_security", v.project, item.WorkItemID, key, fence, key, formatHubTime(now.Add(-time.Minute)), formatHubTime(now))
	}
	contexts := make(chan context.Context, 1)
	v.service.echo.POST("/reporter-aggregate-context", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "reporter-aggregate", "test")
		return c.NoContent(http.StatusOK)
	}, v.service.operatorAuthority)
	requireNativeStatus(t, v.request(t, v.users["reporter"], http.MethodPost, "/organizations/org_security/reporter-aggregate-context", nil), http.StatusOK)
	ctx := <-contexts
	catalog, err := operatorCatalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: "org_security", project: v.project, credential: catalog.credential}
	w := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Minute), Bucket: time.Hour}
	activity, err := v.service.readActivityReport(ctx, catalog.credential, operatortool.ActivityRequest{ProjectID: string(v.project), Limit: 100}, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(activity.Finished) != 1 || activity.Finished[0].WorkItemID != string(own.WorkItemID) {
		t.Fatalf("activity=%+v", activity)
	}
	report, err := v.service.readAnalyticsReport(ctx, catalog.credential, operatortool.AnalyticsRequest{ProjectID: string(v.project), Limit: 100}, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Projects) != 1 || len(report.Projects[0].Attempts.Items) != 1 || report.Projects[0].Attempts.Items[0].WorkItemID != string(own.WorkItemID) {
		t.Fatalf("analytics=%+v", report)
	}
	scoped := reporterReadContext(ctx, scope)
	facets, err := v.service.readReportsFacets(scoped, []string{string(v.project)}, w)
	if err != nil {
		t.Fatal(err)
	}
	if len(facets.Labels) != 1 || facets.Labels[0] != "own-label" || len(facets.FiledBy) != 1 || facets.FiledBy[0].ID != own.Actor.PrincipalID {
		t.Fatalf("facets=%+v", facets)
	}
	labels, err := readNativeLabels(t.Context(), v.service.database.reader, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(labels) != 1 || labels[0].Name != "own-label" {
		t.Fatalf("labels=%+v", labels)
	}
	if _, err := v.service.readRunnerProjectDiagnostics(ctx, scope, operatortool.LocalProjectArguments{ProjectID: string(v.project), IssueID: string(other.WorkItemID)}); !isNativeNotFound(err) {
		t.Fatalf("hidden diagnostic=%v", err)
	}
}
