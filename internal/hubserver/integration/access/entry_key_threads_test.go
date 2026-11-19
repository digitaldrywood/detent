package access_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threads"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEntryKeyThreadReads(t *testing.T) {
	t.Parallel()
	entryRevoked := false
	currentRole := "owner"
	currentPermission := apikey.ScopeRead
	var currentBlocked []string
	var cfg hubfixture.Config
	f := openEntryCatalog(t, func(configuration *hubfixture.Config) {
		if configuration.Hosted.OrganizationID != "org_startup" {
			return
		}
		configuration.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			if entryRevoked {
				return cloudassert.Claims{}, operatortool.ErrAccessDenied
			}
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: "org_startup", ProviderOrganization: "provider_org_startup", Subject: origin.Subject, Role: currentRole, Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: currentPermission, BlockedProjects: currentBlocked, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
		}
		cfg = *configuration
	})
	now := time.Now().UTC()
	organizations := []tracker.OrganizationID{"org_startup", "org_other"}
	for _, org := range organizations {
		if err := f.repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, user := range []string{"first", "second"} {
				id := "credential_" + user
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: user, Hash: apikey.HashToken(string(org) + user), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: user, MembershipID: "membership_" + user, KeyScope: "admin", ProjectAccess: "all", Kind: "personal"}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: user, Email: user + "@example.test", MembershipID: "membership_" + user, Role: "owner", Active: true, PrincipalID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			project := tracker.ProjectID("project_" + string(org))
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: hubfixture.States()}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "credential_first", ProjectID: project}); err != nil {
				return err
			}
			return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "first", ProjectID: project, CanWrite: true, ManageRunner: true})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range organizations {
		if err := f.repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			project := tracker.ProjectID("project_" + string(org))
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "hidden_project", Name: "Private example", Profile: "native"}, States: hubfixture.States()}); err != nil {
				return err
			}
			for index, name := range []string{"own", "foreign"} {
				actor := "credential_first"
				if name == "foreign" {
					actor = "credential_second"
				}
				id := tracker.NativeWorkItemID("wi_" + name)
				issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Number: index + 1, Revision: 1}, Title: "Example issue", State: "Todo", Actor: tracker.Actor{Kind: "human", PrincipalID: actor}, CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(issue)
				if err != nil {
					return err
				}
				version, err := eventstore.NewContent(cfg.RunDetails).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: 1}, raw)
				if err != nil {
					return err
				}
				state := "Todo"
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: id, Number: int64(index + 1), Title: issue.Title, WorkflowStateName: &state, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &issue.Actor.PrincipalID}); err != nil {
					return err
				}
				if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"general", "project", "own", "foreign", "hidden"} {
			scope, project, issue := "general", "", ""
			if name != "general" {
				scope, project = "project", "project_"+string(org)
			}
			if name == "hidden" {
				project = "hidden_project"
			}
			if name == "own" || name == "foreign" {
				scope, issue = "issue", "wi_"+name
			}
			stamp := now.Format("2006-01-02T15:04:05.000000000Z07:00")
			source := threadstore.Import{Thread: threadstore.Record{Thread: threadstore.Thread{ID: "th_" + string(org) + "_" + name, Scope: scope, ProjectID: project, IssueID: issue, Title: "Example " + name, Status: "open", Creator: "credential_first", CreatedAt: stamp, UpdatedAt: stamp, PostCount: 1}, RequestKey: "create_" + name, InputHash: strings.Repeat("a", 64)}, Posts: []threadstore.PostRecord{{Post: threadstore.Post{Sequence: 1, Author: "credential_first", Name: "Example", Kind: "note", Body: "Preserved body " + string(org) + " " + name, CreatedAt: stamp}, RequestKey: "post_" + name, InputHash: strings.Repeat("b", 64)}}}
			source.Thread.Participants = []threadstore.Participant{{PrincipalID: "credential_first", Name: "Example"}}
			if err := f.repository.ImportTeamThread(t.Context(), org, source, cfg.RunDetails); err != nil {
				t.Fatal(err)
			}
		}
	}
	authentication := hubauth.New(hubauth.Deps{Repository: f.repository, AccessRepository: f.repository, Organization: "org_startup", Hosted: &hubauth.Hosted{OrganizationID: "org_startup", Shared: true}})
	threadReader := threads.New(threads.Deps{
		Repository: f.repository, Details: func() attachment.DetailStore { return cfg.RunDetails },
		Now: time.Now, RecordError: hubapi.ProjectRecordError, IsNotFound: hubapi.IsNativeNotFound,
	})
	t.Run("hosted_service_grants", func(t *testing.T) {
		for _, tc := range []struct {
			name, user, access string
			grant              bool
			want               []string
		}{
			{name: "selected_granted", user: "first", access: "selected", grant: true, want: []string{"general", "project", "own", "foreign"}},
			{name: "selected_without_token_grant", user: "first", access: "selected", want: []string{"general"}},
			{name: "selected_owner_without_member_grant", user: "second", access: "selected", grant: true, want: []string{"general", "project", "own", "foreign"}},
			{name: "all_with_owner_role", user: "first", access: "all", want: []string{"general", "project", "own", "foreign", "hidden"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				id := "service_" + tc.name
				hash := apikey.HashToken(id)
				if err := f.repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: id, Name: tc.name, Hash: hash, Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: tc.user, MembershipID: "membership_" + tc.user, KeyScope: "read", ProjectAccess: tc.access, Kind: "service"}); err != nil {
						return err
					}
					if tc.grant {
						return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: id, ProjectID: "project_org_startup"})
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				credential := hubauth.Credential{ID: id, Hash: hash, Scope: hubauth.Operator, NativeOnly: true, HostedMembership: "membership_" + tc.user, HostedPrincipal: "credential_" + tc.user, HostedRole: "owner", HostedKeyKind: "service", HostedKeyScope: apikey.ScopeRead, HostedProjectAccess: tc.access, Hosted: &auth.HostedIdentity{Subject: tc.user, OrganizationID: "provider_org_startup"}}
				input := hubauth.Input{Organization: "org_startup", Credential: credential}
				page, err := threadReader.List(t.Context(), threads.NewCaller(authentication, input, threads.CallerDeps{}), operatortool.ThreadArguments{})
				if err != nil {
					t.Fatal(err)
				}
				requireEntryThreadIDs(t, page, tc.want...)
				for _, thread := range page.Threads {
					if thread.Preview == "" || len(thread.Participants) == 0 {
						t.Fatalf("authorized thread lost preview or participants: %+v", thread)
					}
				}
			})
		}
	})
	request := func(path, user string, arguments any, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		id, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		claims := cloudassert.Claims{Issuer: "entry", Audience: "org_startup", ProviderOrganization: "provider_org_startup", Subject: user, Role: "owner", Generation: 1, Kind: cloudassert.KindKey, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: time.Now(), ExpiresAt: time.Now().Add(20 * time.Second), ID: id, Key: &apikey.KeyAuthority{ID: "key_" + user, CredentialBinding: strings.Repeat("a", 64), Kind: "personal", Permission: currentPermission, ProjectContext: apikey.ProjectContext{Access: "all"}}}
		if mutate != nil {
			mutate(&claims)
		}
		assertion, err := cloudassert.Sign(f.key, claims)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(cloudassert.Header, assertion)
		response := httptest.NewRecorder()
		f.handlers["org_startup"].ServeHTTP(response, req)
		return response
	}
	call := func(name string, args any, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
		return request("/internal/v1/keys/call", "first", map[string]any{"name": name, "arguments": args, "connection_id": "thread_reads"}, mutate)
	}
	workItem := call(operatortool.WorkItem, map[string]any{"project_id": "project_org_startup", "reference": "wi_own"}, nil)
	hubfixture.RequireStatus(t, workItem, http.StatusOK)
	fileIssue := call(operatortool.FileIssue, map[string]any{"project_id": "project_org_startup", "title": "Read-only credential", "description": "Example", "state": "Todo", "request_id": "read-only-issue"}, nil)
	hubfixture.RequireStatus(t, fileIssue, http.StatusForbidden)
	foreignOrganizationItem := call(operatortool.WorkItem, map[string]any{"project_id": "project_org_other", "reference": "wi_foreign"}, nil)
	hubfixture.RequireStatus(t, foreignOrganizationItem, http.StatusForbidden)
	unavailableTool := call(operatortool.WorkPRComments, map[string]any{"project_id": "project_org_startup", "reference": "wi_own"}, nil)
	hubfixture.RequireStatus(t, unavailableTool, http.StatusForbidden)
	if !strings.Contains(unavailableTool.Body.String(), "tool_unavailable") {
		t.Fatalf("unsupported tool response = %s", unavailableTool.Body.String())
	}
	catalog := request("/internal/v1/keys/catalog", "first", map[string]any{}, nil)
	hubfixture.RequireStatus(t, catalog, http.StatusOK)
	var definitions []operatortool.Definition
	hubfixture.Decode(t, catalog, &definitions)
	for _, name := range []string{"list_threads", "get_thread"} {
		t.Run(name, func(t *testing.T) {
			found := false
			for _, d := range definitions {
				if d.Name == name {
					found = true
					if !d.Annotations.ReadOnly {
						t.Fatal("thread read marked as mutation")
					}
				}
			}
			if !found {
				t.Error("authorized thread read missing from catalog")
			}
			args := map[string]any{}
			if name == "get_thread" {
				args["thread_id"] = "th_org_startup_own"
			}
			response := call(name, args, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("thread read status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if t.Failed() {
		return
	}
	decodeThread := func(response *httptest.ResponseRecorder, target any) {
		t.Helper()
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		hubfixture.Decode(t, response, &envelope)
		if err := json.Unmarshal(envelope.Data, target); err != nil {
			t.Fatal(err)
		}
	}
	type snapshot struct {
		Records []threadstore.Record
		Posts   [][]threadstore.PostRecord
		Cursors []int64
	}
	capture := func() snapshot {
		t.Helper()
		var out snapshot
		for _, org := range organizations {
			if err := f.repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				for _, name := range []string{"general", "project", "own", "foreign", "hidden"} {
					id := "th_" + string(org) + "_" + name
					record, err := r.Thread(t.Context(), id, threadstore.Filter{AllProjects: true})
					if err != nil {
						return err
					}
					out.Records = append(out.Records, record)
					posts, err := r.ThreadPosts(t.Context(), id, 0, record.PostCount, 50)
					if err != nil {
						return err
					}
					out.Posts = append(out.Posts, posts)
					for _, principal := range []string{"credential_first", "credential_second"} {
						cursor, err := r.ThreadReadCursor(t.Context(), id, principal)
						if err != nil {
							return err
						}
						out.Cursors = append(out.Cursors, cursor)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	before := capture()
	read := call("get_thread", map[string]any{"thread_id": "th_org_startup_project"}, nil)
	hubfixture.RequireStatus(t, read, http.StatusOK)
	var body threadstore.Read
	decodeThread(read, &body)
	if len(body.Posts) != 1 || body.Posts[0].Body != "<transcript>\nPreserved body org_startup project\n</transcript>" {
		t.Fatalf("actual body missing: %+v", body)
	}
	after := capture()
	if !reflect.DeepEqual(before.Records, after.Records) || !reflect.DeepEqual(before.Posts, after.Posts) {
		t.Fatal("read changed thread metadata, visibility or posts")
	}
	expected := append([]int64(nil), before.Cursors...)
	expected[2] = 1
	if !reflect.DeepEqual(after.Cursors, expected) {
		t.Fatalf("read changed foreign cursor or missed own cursor: got %v want %v", after.Cursors, expected)
	}
	setRole := func(role string) {
		t.Helper()
		if err := f.repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(t.Context(), "first")
			if err != nil {
				return err
			}
			member.Role = role
			return w.UpdateAccessMember(t.Context(), member)
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, role := range []string{"owner", "admin", "member", "reporter", "viewer"} {
		t.Run(role, func(t *testing.T) {
			currentRole = role
			setRole(role)
			catalog := request("/internal/v1/keys/catalog", "first", map[string]any{}, nil)
			hubfixture.RequireStatus(t, catalog, http.StatusOK)
			hubfixture.Decode(t, catalog, &definitions)
			names := map[string]bool{}
			for _, d := range definitions {
				names[d.Name] = true
				if operatortool.IsThreadTool(d.Name) && !d.Annotations.ReadOnly {
					t.Fatalf("thread write exposed: %s", d.Name)
				}
			}
			if !names["list_threads"] || !names["get_thread"] {
				t.Fatalf("missing thread read descriptors: %v", names)
			}
			response := call("list_threads", map[string]any{}, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var page threadstore.Page
			decodeThread(response, &page)
			visible := []string{"general", "project", "own"}
			if role != "reporter" {
				visible = append(visible, "foreign")
			}
			if role == "owner" || role == "admin" {
				visible = append(visible, "hidden")
			}
			requireEntryThreadIDs(t, page, visible...)
			beforeList := capture()
			response = call("list_threads", map[string]any{"project_id": "project_org_startup", "status": "open", "limit": 5}, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			decodeThread(response, &page)
			projectThreads := []string{"project", "own"}
			if role != "reporter" {
				projectThreads = append(projectThreads, "foreign")
			}
			requireEntryThreadIDs(t, page, projectThreads...)
			for _, thread := range page.Threads {
				if thread.ProjectID != "project_org_startup" || thread.Status != "open" {
					t.Fatalf("project thread read lost filters: %+v", thread)
				}
			}
			if !reflect.DeepEqual(beforeList, capture()) {
				t.Fatal("listing project threads changed records, posts or read cursors")
			}
			response = call("get_thread", map[string]any{"thread_id": "th_org_startup_own"}, nil)
			hubfixture.RequireStatus(t, response, http.StatusOK)
			decodeThread(response, &body)
			if len(body.Posts) != 1 || body.Posts[0].Body != "<transcript>\nPreserved body org_startup own\n</transcript>" {
				t.Fatalf("read role lost actual body: %+v", body)
			}
			if role == "reporter" {
				hubfixture.RequireStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_foreign"}, nil), http.StatusUnprocessableEntity)
			}
		})
	}
	filtered := call("list_threads", map[string]any{"project_id": "project_org_startup", "scope": "issue", "issue_id": "wi_own", "status": "open"}, nil)
	hubfixture.RequireStatus(t, filtered, http.StatusOK)
	var filteredPage threadstore.Page
	decodeThread(filtered, &filteredPage)
	if len(filteredPage.Threads) != 1 || filteredPage.Threads[0].ID != "th_org_startup_own" {
		t.Fatalf("thread filters lost: %+v", filteredPage)
	}
	hubfixture.RequireStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_hidden"}, nil), http.StatusUnprocessableEntity)
	currentRole = "owner"
	setRole("owner")
	for _, name := range []string{"list_threads", "get_thread"} {
		args := map[string]any{"project_id": "project_org_startup"}
		if name == "get_thread" {
			args["thread_id"] = "th_org_startup_project"
		}
		response := call(name, args, func(c *cloudassert.Claims) {
			c.Key.ProjectContext = apikey.ProjectContext{Access: "selected", Projects: []string{"project_org_other"}}
		})
		hubfixture.RequireStatus(t, response, http.StatusForbidden)
		if !strings.Contains(response.Body.String(), "key_project_denied") {
			t.Fatalf("selected project denial changed: %s", response.Body.String())
		}
	}
	hubfixture.RequireStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_other_general"}, nil), http.StatusUnprocessableEntity)
	currentBlocked = []string{"project_org_startup"}
	hubfixture.RequireStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_project"}, nil), http.StatusUnprocessableEntity)
	currentBlocked = nil
	for _, name := range []string{"create_thread", "post_to_thread", "resolve_thread"} {
		hubfixture.RequireStatus(t, call(name, map[string]any{}, nil), http.StatusForbidden)
	}
	hubfixture.RequireStatus(t, call("post_to_thread", map[string]any{"thread_id": "th_org_startup_own", "kind": "note", "body": "Must be rejected"}, func(c *cloudassert.Claims) { c.Key.Permission = apikey.ScopeWrite }), http.StatusForbidden)
	entryRevoked = true
	hubfixture.RequireStatus(t, call("list_threads", map[string]any{}, nil), http.StatusForbidden)
	hubfixture.RequireStatus(t, call(operatortool.WorkItem, map[string]any{"project_id": "project_org_startup", "reference": "wi_own"}, nil), http.StatusForbidden)
	entryRevoked = false
	currentRole = "account"
	hubfixture.RequireStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_own"}, nil), http.StatusForbidden)
	hubfixture.RequireStatus(t, call(operatortool.WorkItem, map[string]any{"project_id": "project_org_startup", "reference": "wi_own"}, nil), http.StatusForbidden)
	currentRole = "member"
	setRole("member")
	if err := f.repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), "first", "project_org_startup")
	}); err != nil {
		t.Fatal(err)
	}
	hubfixture.RequireStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_project"}, nil), http.StatusUnprocessableEntity)
	if err := f.repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "credential_first", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	hubfixture.RequireStatus(t, call("list_threads", map[string]any{}, nil), http.StatusForbidden)
}

func requireEntryThreadIDs(t *testing.T, page threadstore.Page, names ...string) {
	t.Helper()
	got := make([]string, 0, len(page.Threads))
	for _, thread := range page.Threads {
		got = append(got, thread.ID)
	}
	want := make([]string, 0, len(names))
	for _, name := range names {
		want = append(want, "th_org_startup_"+name)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("visible thread IDs = %v, want %v", got, want)
	}
}
