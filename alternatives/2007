package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReadableSlugMigration(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE organizations(id TEXT PRIMARY KEY,name TEXT NOT NULL); CREATE TABLE projects(id TEXT PRIMARY KEY,organization_id TEXT,name TEXT);
INSERT INTO organizations VALUES('o1','Example Organization'),('o2','Example Organization');
INSERT INTO projects VALUES('p1','o1','Example Project'),('p2','o1','Example Project!'),('p3','o2','Example Project');`); err != nil {
		t.Fatal(err)
	}
	reservedRoutes := []string{"work", "settings", "login", "projects", "organizations", "organization", "api", "app", "auth", "chat", "reports", "activity", "diagnostics", "fleet", "support", "invite", "logout", "health", "metrics", "static", "webhooks", "design-system", "setup", "usage", "billing", "account", "platform"}
	for _, route := range reservedRoutes {
		if _, err := db.ExecContext(t.Context(), "INSERT INTO organizations(id,name) VALUES(?,?)", "org_"+route, route); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name) VALUES(?,?,?)", "project_"+route, "o1", route); err != nil {
			t.Fatal(err)
		}
	}
	migration, err := migrationFiles.ReadFile("migrations/20261007221000_readable_urls.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(migration), "-- +goose Down")
	if _, err := db.ExecContext(t.Context(), up); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ table, id, want string }{{"organizations", "o1", "example-organization"}, {"organizations", "o2", "example-organization-2"}, {"projects", "p1", "example-project"}, {"projects", "p2", "example-project-2"}, {"projects", "p3", "example-project"}} {
		var got string
		if err := db.QueryRowContext(t.Context(), "SELECT slug FROM "+test.table+" WHERE id=?", test.id).Scan(&got); err != nil || got != test.want {
			t.Fatalf("%s %s slug=%q want=%q err=%v", test.table, test.id, got, test.want, err)
		}
	}
	for _, route := range reservedRoutes {
		t.Run(route, func(t *testing.T) {
			for _, scope := range []struct{ table, id, suffix string }{{"organizations", "org_", "-organization"}, {"projects", "project_", "-project"}} {
				var got string
				if err := db.QueryRowContext(t.Context(), "SELECT slug FROM "+scope.table+" WHERE id=?", scope.id+route).Scan(&got); err != nil || got != route+scope.suffix {
					t.Fatalf("migrated %s %s slug=%q err=%v", scope.table, route, got, err)
				}
			}
			if _, err := db.ExecContext(t.Context(), "INSERT INTO organizations(id,name) VALUES(?,?)", "new_org_"+route, route); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name) VALUES(?,?,?)", "new_project_"+route, "o1", route); err != nil {
				t.Fatal(err)
			}
			for _, scope := range []struct{ table, id, suffix string }{{"organizations", "new_org_", "-organization-2"}, {"projects", "new_project_", "-project-2"}} {
				var got string
				if err := db.QueryRowContext(t.Context(), "SELECT slug FROM "+scope.table+" WHERE id=?", scope.id+route).Scan(&got); err != nil || got != route+scope.suffix {
					t.Fatalf("created %s %s slug=%q err=%v", scope.table, route, got, err)
				}
			}
		})
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO organizations(id,name) VALUES('o4','Example Organization'),('o5','!!!'); INSERT INTO projects(id,organization_id,name) VALUES('p4','o1','Example Project')"); err != nil {
		t.Fatal(err)
	}
	var fallback string
	if err := db.QueryRowContext(t.Context(), "SELECT slug FROM organizations WHERE id='o5'").Scan(&fallback); err != nil || fallback != "organization-organization-3" {
		t.Fatalf("new fallback slug=%q err=%v", fallback, err)
	}
	var organization, project string
	if err := db.QueryRowContext(t.Context(), "SELECT o.slug,p.slug FROM organizations o CROSS JOIN projects p WHERE o.id='o4' AND p.id='p4'").Scan(&organization, &project); err != nil {
		t.Fatal(err)
	}
	if organization != "example-organization-3" || project != "example-project-3" {
		t.Fatalf("new slugs=%q %q", organization, project)
	}
}

func TestReadableIssueRoutesAndSlugEdits(t *testing.T) {
	useAppClientFS(t, appClientBundle())
	f := newHostedSecurityFixture(t)
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	hidden := f.user(t, "hidden", "member", "hidden@example.test", "", "")
	response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "readable"}, Title: "Readable", State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	changeResponse := f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "readable-change"}, Title: "Readable change"})
	requireNativeStatus(t, changeResponse, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, changeResponse, &change)
	canonical := "/local-organization/private-project-sentinel/issues/1"
	for _, test := range []struct {
		path     string
		user     hostedSecurityUser
		status   int
		location string
	}{
		{canonical, owner, http.StatusOK, ""},
		{"/work/p/" + string(f.project), owner, http.StatusPermanentRedirect, "/local-organization/private-project-sentinel"},
		{canonical + "/changes/" + change.ID, owner, http.StatusOK, ""},
		{canonical + "/changes/missing", owner, http.StatusNotFound, ""},
		{canonical + "/changes/" + change.ID, hidden, http.StatusNotFound, ""},
		{"/work/i/" + string(issue.WorkItemID) + "/changes/" + change.ID, owner, http.StatusPermanentRedirect, issue.WebURL + "/changes/" + change.ID},
		{"/local-organization/private-project-sentinel", owner, http.StatusOK, ""},
		{"/work/i/" + string(issue.WorkItemID), owner, http.StatusPermanentRedirect, issue.WebURL},
		{"/projects/" + string(f.project) + "/issues/1", owner, http.StatusPermanentRedirect, issue.WebURL},
		{canonical, hidden, http.StatusNotFound, ""},
		{"/local-organization/private-project-sentinel/issues/999", hidden, http.StatusNotFound, ""},
		{canonical, hostedSecurityUser{}, http.StatusNotFound, ""},
	} {
		response := f.request(t, test.user, http.MethodGet, test.path, nil)
		requireNativeStatus(t, response, test.status)
		if location := response.Header().Get("Location"); location != test.location {
			t.Fatalf("%s location=%q want=%q", test.path, location, test.location)
		}
	}
	slugPath := f.base + "/slug"
	orgPath := "/api/v2/organizations/org_security/slug"
	for _, slug := range []string{"work", "settings", "login", "projects", "organizations", "organization", "api", "app", "auth", "chat", "reports", "activity", "diagnostics", "fleet", "support", "invite", "logout", "health", "metrics", "static", "webhooks", "design-system", "setup", "usage", "billing", "account", "platform", "Bad Slug", "two--hyphens", "-leading"} {
		t.Run("reject slug "+slug, func(t *testing.T) {
			requireNativeStatus(t, f.request(t, owner, http.MethodPut, slugPath, map[string]string{"slug": slug, "expected_slug": "private-project-sentinel"}), http.StatusUnprocessableEntity)
			requireNativeStatus(t, f.request(t, owner, http.MethodPut, orgPath, map[string]string{"slug": slug, "expected_slug": "local-organization"}), http.StatusUnprocessableEntity)
		})
	}
	requireNativeStatus(t, f.request(t, hidden, http.MethodPut, slugPath, map[string]string{"slug": "hidden-change", "expected_slug": "private-project-sentinel"}), http.StatusNotFound)
	requireNativeStatus(t, f.request(t, owner, http.MethodPut, slugPath, map[string]string{"slug": "example-project", "expected_slug": "private-project-sentinel"}), http.StatusOK)
	requireNativeStatus(t, f.request(t, owner, http.MethodPut, orgPath, map[string]string{"slug": "example-organization", "expected_slug": "local-organization"}), http.StatusOK)
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, canonical, nil), http.StatusPermanentRedirect)
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, canonical+"/changes/"+change.ID, nil), http.StatusPermanentRedirect)
	requireNativeStatus(t, f.request(t, owner, http.MethodGet, "/local-organization/settings/general", nil), http.StatusPermanentRedirect)
	response = f.request(t, owner, http.MethodGet, "/example-organization/example-project/issues/1", nil)
	requireNativeStatus(t, response, http.StatusOK)
	response = f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID), nil)
	var current tracker.NativeIssue
	decodeHubResponse(t, response, &current)
	if current.WebURL != "http://127.0.0.1:7777/example-organization/example-project/issues/1" {
		t.Fatalf("current URL=%q", current.WebURL)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET duplicate_challenge=1 WHERE id=?", f.project); err != nil {
		t.Fatal(err)
	}
	response = f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "readable-duplicate"}, Title: "Readable", State: "Todo"})
	requireNativeStatus(t, response, http.StatusConflict)
	var challenge duplicateChallenge
	decodeHubResponse(t, response, &challenge)
	if len(challenge.PossibleDuplicates) != 1 || challenge.PossibleDuplicates[0].WebURL != "http://127.0.0.1:7777/example-organization/example-project/issues/1" {
		t.Fatalf("duplicate links do not use current slugs: %s", response.Body.String())
	}
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/readable-reference-fixture", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "readable-reference", "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, f.request(t, owner, http.MethodPost, "/readable-reference-fixture", nil), http.StatusOK)
	ctx := <-contexts
	for _, reference := range []string{issue.WebURL, current.WebURL, canonical, "/example-organization/example-project/issues/1"} {
		raw, err := json.Marshal(map[string]string{"project_id": string(f.project), "reference": reference})
		if err != nil {
			t.Fatal(err)
		}
		result, err := (hostedOperatorExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.WorkItem, Arguments: raw})
		if err != nil {
			t.Fatalf("reference %s: %v", reference, err)
		}
		if !strings.Contains(string(result.Content), current.WebURL) || strings.Contains(string(result.Content), "/local-organization/") {
			t.Fatalf("reference %s stale result=%s", reference, result.Content)
		}
	}

}

func TestMCPToolCatalogReadableIssueLinks(t *testing.T) {
	f := newHostedSecurityFixture(t, func(config *Config) { config.Conversation = &ConversationConfig{Enabled: true} })
	owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "catalog-issue"}, Title: "Catalog issue", State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	response = f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "catalog-change"}, Title: "Catalog change"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	response = f.request(t, owner, http.MethodPost, f.base+"/conversations", conversationCreateBody(map[string]any{"title": "Catalog conversation"}))
	requireNativeStatus(t, response, http.StatusCreated)
	var created conversationCreateResponse
	decodeHubResponse(t, response, &created)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversations SET work_item_id=?,linked_at=created_at WHERE id=?", issue.WorkItemID, created.Conversation.ID); err != nil {
		t.Fatal(err)
	}
	contexts := make(chan context.Context, 1)
	f.service.echo.POST("/readable-tools-fixture", func(c echo.Context) error {
		contexts <- operatortool.BindConnection(c.Request().Context(), "readable-tools", "test")
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	response = f.request(t, owner, http.MethodPost, "/readable-tools-fixture", nil)
	requireNativeStatus(t, response, http.StatusOK)
	ctx := <-contexts
	executor := hostedOperatorExecutor{f.service}
	catalog, err := executor.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{operatortool.WorkItem: false, operatortool.WorkList: false, operatortool.FileIssue: false, operatortool.EditItem: false, operatortool.MoveItem: false, operatortool.AddComment: false, operatortool.ListChanges: false, operatortool.GetChange: false, "get_work_item_conversation": false, "get_conversation": false, "list_project_conversations": false, "list_work_item_references": false, "create_conversation": false}
	for _, tool := range catalog {
		t.Run(tool.Name, func(t *testing.T) {
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
				Required   []string                   `json:"required"`
			}
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			var revision int64
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT revision FROM issues WHERE native_id=?", issue.WorkItemID).Scan(&revision); err != nil {
				t.Fatal(err)
			}
			values := map[string]any{"project_id": string(f.project), "reference": issue.WebURL, "identifier": issue.WebURL, "work_item_id": issue.WebURL, "conversation_id": created.Conversation.ID, "request_id": "catalog-" + tool.Name, "title": "Readable " + tool.Name, "description": "Catalog issue", "body": "Readable comment", "state": "Done", "target_state": "Done", "expected_revision": strconv.FormatInt(revision, 10), "change_id": change.ID, "expected_change_revision": strconv.FormatInt(int64(change.Revision), 10), "limit": 10}
			if tool.Name == "create_conversation" {
				values["input"] = map[string]string{"title": "Readable subject", "subject_work_item_id": issue.WebURL}
			}
			args := map[string]any{}
			for key := range schema.Properties {
				if value, ok := values[key]; ok {
					args[key] = value
				}
			}
			for _, key := range schema.Required {
				if _, ok := args[key]; !ok {
					return
				}
			}
			raw, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			result, err := executor.Execute(ctx, operatortool.Call{Name: tool.Name, Arguments: raw})
			if _, must := required[tool.Name]; must && err != nil {
				t.Fatalf("%s: %v", tool.Name, err)
			}
			if err != nil {
				return
			}
			if _, must := required[tool.Name]; must {
				required[tool.Name] = true
			}
			if tool.Name == "get_conversation" || tool.Name == "list_project_conversations" {
				want := `"web_url":"http://127.0.0.1:7777/local-organization/chat/c/` + created.Conversation.ID + `?project_id=` + string(f.project) + `"`
				if !strings.Contains(string(result.Content), want) {
					t.Fatalf("missing canonical conversation URL: %s", result.Content)
				}
			}
			var content any
			if err := json.Unmarshal(result.Content, &content); err != nil {
				t.Fatal(err)
			}
			var check func(any)
			check = func(value any) {
				switch value := value.(type) {
				case []any:
					for _, child := range value {
						check(child)
					}
				case map[string]any:
					for key, child := range value {
						if link, ok := child.(string); ok && (key == "url" || key == "web_url" || strings.HasSuffix(key, "_url")) && (strings.Contains(link, "/work/i/wi_") || strings.Contains(link, "/issues/wi_") || strings.Contains(link, "/work-items/wi_")) {
							t.Errorf("%s returned legacy %s=%q", tool.Name, key, link)
						}
						check(child)
					}
				}
			}
			check(content)
			if tool.Name == operatortool.WorkItem && !strings.Contains(string(result.Content), issue.WebURL) {
				t.Errorf("hosted work_item has no readable browser URL: %s", result.Content)
			}
		})
	}
	projection, err := f.service.canonicalToolResult(t.Context(), operatortool.Result{Content: json.RawMessage(`{"sequence":9223372036854775807,"url":"https://github.com/example/repo/issues/wi_external","manifest_url":"/api/v2/work-items/wi_external/download"}`)}, operatortool.WorkItem)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projection.Content), `"sequence":9223372036854775807`) || !strings.Contains(string(projection.Content), `"url":"https://github.com/example/repo/issues/wi_external"`) || !strings.Contains(string(projection.Content), `"manifest_url":"/api/v2/work-items/wi_external/download"`) {
		t.Fatalf("projection changed nonbrowser content: %s", projection.Content)
	}
	for tool, ran := range required {
		if !ran {
			t.Errorf("required tool %s did not return a response", tool)
		}
	}
}
