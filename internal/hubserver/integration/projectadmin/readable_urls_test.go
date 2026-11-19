package projectadmin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReadableIssueRoutesAndSlugEdits(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t, func(cfg *hubfixture.Config) {
		cfg.StaticFS = fstest.MapFS{
			"app/conversation/index.html": {Data: []byte(`<!doctype html><html><head><script type="module" src="/static/app/conversation/app.js"></script></head><body><div id="root"></div></body></html>`)},
			"app/conversation/app.js":     {Data: []byte("console.log(1)")},
		}
	})
	owner := f.User(t, "owner", "owner", "owner@example.test", "write", "")
	hidden := f.User(t, "hidden", "member", "hidden@example.test", "", "")
	response := f.Request(t, owner, http.MethodPost, f.Base+"/work-items", tracker.CreateIssue{Body: hubfixture.IssueBody(), Mutation: tracker.Mutation{IdempotencyKey: "readable"}, Title: "Readable", State: "Todo"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	changeResponse := f.Request(t, owner, http.MethodPost, f.Base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "readable-change"}, Title: "Readable change"})
	hubfixture.RequireStatus(t, changeResponse, http.StatusOK)
	var change tracker.ChangeRequest
	hubfixture.Decode(t, changeResponse, &change)
	canonical := "/local-organization/private-project-sentinel/issues/1"
	for _, test := range []struct {
		path     string
		user     hubfixture.HostedUser
		status   int
		location string
	}{
		{canonical, owner, http.StatusOK, ""},
		{"/work/p/" + string(f.Project), owner, http.StatusPermanentRedirect, "/local-organization/private-project-sentinel"},
		{canonical + "/changes/" + change.ID, owner, http.StatusOK, ""},
		{canonical + "/changes/missing", owner, http.StatusNotFound, ""},
		{canonical + "/changes/" + change.ID, hidden, http.StatusNotFound, ""},
		{"/work/i/" + string(issue.WorkItemID) + "/changes/" + change.ID, owner, http.StatusPermanentRedirect, issue.WebURL + "/changes/" + change.ID},
		{"/local-organization/private-project-sentinel", owner, http.StatusOK, ""},
		{"/work/i/" + string(issue.WorkItemID), owner, http.StatusPermanentRedirect, issue.WebURL},
		{"/projects/" + string(f.Project) + "/issues/1", owner, http.StatusPermanentRedirect, issue.WebURL},
		{canonical, hidden, http.StatusNotFound, ""},
		{"/local-organization/private-project-sentinel/issues/999", hidden, http.StatusNotFound, ""},
		{canonical, hubfixture.HostedUser{}, http.StatusNotFound, ""},
	} {
		response := f.Request(t, test.user, http.MethodGet, test.path, nil)
		hubfixture.RequireStatus(t, response, test.status)
		if location := response.Header().Get("Location"); location != test.location {
			t.Fatalf("%s location=%q want=%q", test.path, location, test.location)
		}
	}
	slugPath := f.Base + "/slug"
	orgPath := "/api/v2/organizations/org_security/slug"
	for _, slug := range []string{"work", "settings", "login", "projects", "organizations", "organization", "api", "app", "auth", "chat", "reports", "activity", "diagnostics", "fleet", "support", "invite", "logout", "health", "metrics", "static", "webhooks", "design-system", "setup", "usage", "billing", "account", "platform", "Bad Slug", "two--hyphens", "-leading"} {
		t.Run("reject slug "+slug, func(t *testing.T) {
			hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodPut, slugPath, map[string]string{"slug": slug, "expected_slug": "private-project-sentinel"}), http.StatusUnprocessableEntity)
			hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodPut, orgPath, map[string]string{"slug": slug, "expected_slug": "local-organization"}), http.StatusUnprocessableEntity)
		})
	}
	hubfixture.RequireStatus(t, f.Request(t, hidden, http.MethodPut, slugPath, map[string]string{"slug": "hidden-change", "expected_slug": "private-project-sentinel"}), http.StatusNotFound)
	hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodPut, slugPath, map[string]string{"slug": "example-project", "expected_slug": "private-project-sentinel"}), http.StatusOK)
	hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodPut, orgPath, map[string]string{"slug": "example-organization", "expected_slug": "local-organization"}), http.StatusOK)
	hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodGet, canonical, nil), http.StatusPermanentRedirect)
	hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodGet, canonical+"/changes/"+change.ID, nil), http.StatusPermanentRedirect)
	hubfixture.RequireStatus(t, f.Request(t, owner, http.MethodGet, "/local-organization/settings/general", nil), http.StatusPermanentRedirect)
	response = f.Request(t, owner, http.MethodGet, "/example-organization/example-project/issues/1", nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	response = f.Request(t, owner, http.MethodGet, f.Base+"/work-items/"+string(issue.WorkItemID), nil)
	var current tracker.NativeIssue
	hubfixture.Decode(t, response, &current)
	if current.WebURL != "http://127.0.0.1:7777/example-organization/example-project/issues/1" {
		t.Fatalf("current URL=%q", current.WebURL)
	}
	if err := f.Repository.WriteTenant(t.Context(), f.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.Project)
		if err != nil {
			return err
		}
		record.Settings.DuplicateChallenge = true
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.RefreshPages(t, f.Primary)
	response = f.Request(t, owner, http.MethodPost, f.Base+"/work-items", tracker.CreateIssue{Body: hubfixture.IssueBody(), Mutation: tracker.Mutation{IdempotencyKey: "readable-duplicate"}, Title: "Readable", State: "Todo"})
	hubfixture.RequireStatus(t, response, http.StatusConflict)
	var challenge struct {
		PossibleDuplicates []hubscope.PossibleDuplicate `json:"possible_duplicates"`
	}
	hubfixture.Decode(t, response, &challenge)
	if len(challenge.PossibleDuplicates) == 0 {
		t.Fatalf("duplicate response has no links: %s", response.Body.String())
	}
	for _, duplicate := range challenge.PossibleDuplicates {
		if duplicate.WebURL != current.WebURL {
			t.Fatalf("duplicate URL = %q, want %q", duplicate.WebURL, current.WebURL)
		}
	}
	call := readableLinksMCP(t, f, owner)
	for _, reference := range []string{issue.WebURL, current.WebURL, canonical, "/example-organization/example-project/issues/1"} {
		result := hubfixture.MCPData(t, call("tools/call", operatortool.WorkItem, map[string]string{"project_id": string(f.Project), "reference": reference}), false)
		if !strings.Contains(string(result), current.WebURL) || strings.Contains(string(result), "/local-organization/") {
			t.Fatalf("reference %s stale result=%s", reference, result)
		}
	}
}

func TestMCPToolCatalogReadableIssueLinks(t *testing.T) {
	t.Parallel()
	f := hubfixture.NewHostedSecurity(t, func(cfg *hubfixture.Config) {
		cfg.Conversation = &conversations.ConversationConfig{Enabled: true, ArchiveContents: readableLinkArchives{hubtest.NewDetails()}}
	})
	owner := f.User(t, "owner", "owner", "owner@example.test", "write", "")
	response := f.Request(t, owner, http.MethodPost, f.Base+"/work-items", tracker.CreateIssue{Body: hubfixture.IssueBody(), Mutation: tracker.Mutation{IdempotencyKey: "catalog-issue"}, Title: "Catalog issue", State: "Todo"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	response = f.Request(t, owner, http.MethodPost, f.Base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "catalog-change"}, Title: "Catalog change"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	hubfixture.Decode(t, response, &change)
	response = f.Request(t, owner, http.MethodPost, f.Base+"/conversations", map[string]string{"title": "Catalog conversation", "key": "catalog-conversation"})
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var created struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	}
	hubfixture.Decode(t, response, &created)
	if err := f.Repository.WriteTenant(t.Context(), f.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.Conversation(t.Context(), f.Project, created.Conversation.ID)
		if err != nil {
			return err
		}
		record.WorkItemID = string(issue.WorkItemID)
		record.LinkedAt = &record.CreatedAt
		_, err = w.UpdateConversation(t.Context(), record, record.Revision)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	call := readableLinksMCP(t, f, owner)
	var catalog []operatortool.Definition
	var cursor string
	for {
		reply := call("tools/list", "", cursor)
		if len(reply.Error) > 0 {
			t.Fatalf("list tools: %s", reply.Error)
		}
		var page struct {
			Tools []operatortool.Definition `json:"tools"`
			Next  string                    `json:"nextCursor"`
		}
		if err := json.Unmarshal(reply.Result, &page); err != nil {
			t.Fatal(err)
		}
		catalog = append(catalog, page.Tools...)
		if page.Next == "" {
			break
		}
		if page.Next == cursor {
			t.Fatalf("catalog cursor did not advance: %q", page.Next)
		}
		cursor = page.Next
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
			if err := f.Repository.ReadTenant(t.Context(), f.Primary, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.WorkItem(t.Context(), f.Project, issue.WorkItemID)
				revision = row.Revision
				return err
			}); err != nil {
				t.Fatal(err)
			}
			values := map[string]any{"project_id": string(f.Project), "reference": issue.WebURL, "identifier": issue.WebURL, "work_item_id": issue.WebURL, "conversation_id": created.Conversation.ID, "request_id": "catalog-" + tool.Name, "title": "Readable " + tool.Name, "description": testenv.DistinctText("catalog-file-issue", 24) + hubfixture.IssueContractSections, "body": "Readable comment", "state": "Done", "target_state": "Done", "expected_revision": strconv.FormatInt(revision, 10), "change_id": change.ID, "expected_change_revision": strconv.FormatInt(int64(change.Revision), 10), "limit": 10}
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
			reply := call("tools/call", tool.Name, args)
			var result struct {
				IsError bool            `json:"isError"`
				Content json.RawMessage `json:"structuredContent"`
			}
			if len(reply.Result) != 0 {
				if err := json.Unmarshal(reply.Result, &result); err != nil {
					t.Fatal(err)
				}
			}
			if len(reply.Error) != 0 || result.IsError || len(result.Content) == 0 {
				if _, must := required[tool.Name]; must {
					t.Fatalf("%s: result=%s error=%s", tool.Name, reply.Result, reply.Error)
				}
				return
			}
			if _, must := required[tool.Name]; must {
				required[tool.Name] = true
			}
			if tool.Name == "get_conversation" || tool.Name == "list_project_conversations" {
				want := `"web_url":"http://127.0.0.1:7777/local-organization/chat/c/` + created.Conversation.ID + `?project_id=` + string(f.Project) + `"`
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
	for tool, ran := range required {
		if !ran {
			t.Errorf("required tool %s did not return a response", tool)
		}
	}
}

type readableLinkArchives struct{ *hubtest.Details }

func (a readableLinkArchives) PublishContent(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	return a.StoreDetail(ctx, organization, content)
}

func (a readableLinkArchives) ReadContent(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return a.ReadDetail(ctx, organization, ref)
}

func readableLinksMCP(t *testing.T, f *hubfixture.Hosted, user hubfixture.HostedUser) func(string, string, any) hubfixture.MCPReply {
	t.Helper()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(&http.Cookie{Name: hubfixture.HostedCookie, Value: user.Token})
		r.Header.Set("X-CSRF-Token", hubfixture.HostedCSRF(user.Token))
		f.Handler().ServeHTTP(w, r)
	})
	return hubfixture.MCP(t, handler, hubfixture.MCPPath(f.Primary), "")
}
