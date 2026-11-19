package conversation_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type threadMCPResource struct {
	thread threadstore.Thread
	create operatortool.ThreadArguments
	body   string
}

type threadMCPActor struct {
	user hubfixture.HostedUser
	role string
}

type threadMCPFixture struct {
	hub   *hubfixture.ConversationHostedEntry
	owner threadMCPActor
}

func (f threadMCPFixture) actor(t *testing.T, name, role, grant string) threadMCPActor {
	t.Helper()
	return threadMCPActor{user: f.hub.User(t, "mcp-thread-"+name, role, name+"@example.test", grant, ""), role: role}
}

func (f threadMCPFixture) call(t *testing.T, actor threadMCPActor, tool string, args operatortool.ThreadArguments, allow bool) json.RawMessage {
	t.Helper()
	response := f.hub.BrowserThreadCall(t, actor.user, actor.role, tool, args)
	var envelope struct {
		Result struct {
			IsError bool `json:"isError"`
			Content struct {
				Data json.RawMessage `json:"data"`
			} `json:"structuredContent"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	hubfixture.Decode(t, response, &envelope)
	allowed := response.Code == http.StatusOK && len(envelope.Error) == 0 && !envelope.Result.IsError && len(envelope.Result.Content.Data) > 0
	if allowed != allow {
		t.Fatalf("%s allowed=%t, want %t: %s", tool, allowed, allow, response.Body.String())
	}
	if !allow {
		return response.Body.Bytes()
	}
	return envelope.Result.Content.Data
}

func (f threadMCPFixture) seed(t *testing.T, scope, name, issue string) threadMCPResource {
	t.Helper()
	args := operatortool.ThreadArguments{Scope: scope, Key: hubapi.NewID("seed"), Title: "sentinel-thread-title-" + name, IssueID: issue}
	if scope == "project" {
		args.ProjectID = string(f.hub.Project)
	}
	base := hubfixture.OrganizationBase(f.hub.Primary) + "/threads"
	response := f.hub.BrowserRequest(t, f.owner.user, f.owner.role, http.MethodPost, base, args)
	hubfixture.RequireStatus(t, response, http.StatusCreated)
	var thread threadstore.Thread
	hubfixture.Decode(t, response, &thread)
	body := "sentinel-thread-finding-" + name
	post := operatortool.ThreadArguments{Kind: "finding", Body: body, Key: hubapi.NewID("seed")}
	hubfixture.RequireStatus(t, f.hub.BrowserRequest(t, f.owner.user, f.owner.role, http.MethodPost, base+"/"+thread.ID+"/posts", post), http.StatusCreated)
	return threadMCPResource{thread: thread, create: args, body: body}
}

func (f threadMCPFixture) issue(t *testing.T, actor threadMCPActor, label string) tracker.NativeIssue {
	t.Helper()
	body := "## Steps to reproduce\nOpen the app and save.\n## Expected\nThe change saves.\n## Actual\nThe change disappears.\n## Where\nSettings screen.\n## Environment\nFirefox on Linux, build example."
	response := f.hub.BrowserRequest(t, actor.user, actor.role, http.MethodPost, f.hub.Base+"/work-items", tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: hubapi.NewID("issue")}, Type: "bug", Title: label, Body: body,
	})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	return issue
}

func (f threadMCPFixture) read(t *testing.T, actor threadMCPActor, target threadMCPResource) {
	t.Helper()
	raw := f.call(t, actor, "get_thread", operatortool.ThreadArguments{ThreadID: target.thread.ID}, true)
	var read threadstore.Read
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	if read.Thread.ID != target.thread.ID || read.Thread.IssueID != target.create.IssueID || !slices.ContainsFunc(read.Posts, func(post threadstore.Post) bool { return strings.Contains(post.Body, target.body) }) {
		t.Fatalf("thread content was lost: %s", raw)
	}
	if id := target.create.IssueID; id != "" && (read.Thread.IssueNumber == 0 || !slices.ContainsFunc(read.Thread.References, func(reference threadstore.Reference) bool { return reference.Kind == "issue" && reference.ID == id })) {
		t.Fatalf("thread lost its issue reference: %s", raw)
	}
}

func (f threadMCPFixture) write(t *testing.T, actor threadMCPActor, target threadMCPResource, tool string, allow bool) {
	t.Helper()
	args := operatortool.ThreadArguments{ThreadID: target.thread.ID, Key: hubapi.NewID("mcp")}
	switch tool {
	case "create_thread":
		args.Scope, args.ProjectID, args.IssueID, args.Title = target.create.Scope, target.create.ProjectID, target.create.IssueID, "New discussion"
		args.ThreadID = ""
	case "post_to_thread":
		args.Kind, args.Body = "note", "New finding"
	case "resolve_thread":
		args.Status = "resolved"
	}
	raw := f.call(t, actor, tool, args, allow)
	if !allow {
		target.requireHidden(t, raw)
	}
}

func (target threadMCPResource) requireHidden(t *testing.T, raw json.RawMessage) {
	t.Helper()
	for _, secret := range []string{target.thread.ID, target.thread.Title, target.create.IssueID, target.body} {
		if secret != "" && strings.Contains(string(raw), secret) {
			t.Fatalf("denied thread leaked %q: %s", secret, raw)
		}
	}
}

func TestTeamThreadAuthorizationMCPRoutes(t *testing.T) {
	t.Parallel()
	hub := hubfixture.OpenConversationHostedEntry(t)
	f := threadMCPFixture{hub: hub}
	f.owner = f.actor(t, "owner", "owner", "write")
	if err := hub.Repository.WriteTenant(t.Context(), hub.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), hub.Project)
		if err != nil {
			return err
		}
		project.States = append(project.States, tracker.NativeState{Name: "Triage", Transitions: []string{"Todo"}})
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	general, project := f.seed(t, "general", "general", ""), f.seed(t, "project", "project", "")
	reporter := f.actor(t, "reporter", "reporter", "write")
	otherReporter := f.actor(t, "other-reporter", "reporter", "write")
	ownIssue, otherIssue := f.issue(t, reporter, "Reporter issue"), f.issue(t, otherReporter, "Private issue")
	own := f.seed(t, "issue", "own", string(ownIssue.WorkItemID))
	other := f.seed(t, "issue", "other", string(otherIssue.WorkItemID))
	writeTools := []string{"create_thread", "post_to_thread", "resolve_thread"}

	t.Run("reporter search and paging", func(t *testing.T) {
		f.read(t, reporter, own)
		other.requireHidden(t, f.call(t, reporter, "get_thread", operatortool.ThreadArguments{ThreadID: other.thread.ID}, false))
		query := operatortool.ThreadArguments{IssueID: string(otherIssue.WorkItemID)}
		other.requireHidden(t, f.call(t, reporter, "list_threads", query, true))
		seen := map[string]bool{}
		cursors := map[string]bool{}
		for query := (operatortool.ThreadArguments{Limit: 1, Search: "sentinel-thread"}); ; {
			raw := f.call(t, reporter, "list_threads", query, true)
			other.requireHidden(t, raw)
			var page threadstore.Page
			if err := json.Unmarshal(raw, &page); err != nil {
				t.Fatal(err)
			}
			for _, thread := range page.Threads {
				if seen[thread.ID] {
					t.Fatalf("thread repeated across pages: %s", thread.ID)
				}
				seen[thread.ID] = true
				for _, target := range []threadMCPResource{general, project, own} {
					if thread.ID == target.thread.ID && thread.Preview != target.body {
						t.Fatalf("readable preview missing: %s", raw)
					}
				}
			}
			if page.NextCursor == "" {
				break
			}
			if cursors[page.NextCursor] {
				t.Fatal("thread pagination repeated its cursor")
			}
			cursors[page.NextCursor], query.Cursor = true, page.NextCursor
		}
		for _, target := range []threadMCPResource{general, project, own} {
			if !seen[target.thread.ID] {
				t.Fatalf("reporter could not find %s", target.thread.Title)
			}
		}
		for _, tool := range writeTools {
			f.write(t, reporter, own, tool, true)
			f.write(t, reporter, other, tool, false)
		}
	})

	t.Run("administrator implicit access and member grant", func(t *testing.T) {
		for _, actor := range []threadMCPActor{f.actor(t, "administrator", "admin", ""), f.actor(t, "member", "member", "write")} {
			f.read(t, actor, project)
			f.write(t, actor, project, "post_to_thread", true)
		}
	})
	t.Run("viewer and read grant", func(t *testing.T) {
		viewer, reader := f.actor(t, "viewer", "viewer", "write"), f.actor(t, "read-only", "member", "read")
		f.read(t, viewer, project)
		f.read(t, reader, project)
		for _, tool := range writeTools {
			f.write(t, viewer, project, tool, false)
			f.write(t, reader, project, tool, false)
		}
		f.write(t, reader, general, "post_to_thread", true)
	})
	t.Run("absent and revoked grant", func(t *testing.T) {
		ungranted, removed := f.actor(t, "ungranted", "member", ""), f.actor(t, "removed", "member", "write")
		f.read(t, ungranted, general)
		project.requireHidden(t, f.call(t, ungranted, "get_thread", operatortool.ThreadArguments{ThreadID: project.thread.ID}, false))
		for _, tool := range writeTools {
			f.write(t, ungranted, project, tool, false)
		}
		if err := hub.Repository.WriteTenant(t.Context(), hub.Primary, corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessMember(t.Context(), removed.user.Identity.Subject, hub.Now())
		}); err != nil {
			t.Fatal(err)
		}
		if err := hub.Provider.RevokeMembership(t.Context(), "membership_"+removed.user.Identity.Subject); err != nil {
			t.Fatal(err)
		}
		general.requireHidden(t, f.call(t, removed, "list_threads", operatortool.ThreadArguments{}, false))
		f.write(t, removed, general, "create_thread", false)
	})
	t.Run("owner route success", func(t *testing.T) {
		f.read(t, f.owner, general)
		for _, tool := range writeTools {
			f.write(t, f.owner, general, tool, true)
		}
	})
}
