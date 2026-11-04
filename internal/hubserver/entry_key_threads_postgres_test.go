package hubserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func TestPostgresEntryKeyThreadReads(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	key := hostedSharedKey(38)
	cfg := startupConfig(connection)
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	entryRevoked := false
	currentRole := "owner"
	currentPermission := apikey.ScopeRead
	var currentBlocked []string
	cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
		if entryRevoked {
			return cloudassert.Claims{}, operatortool.ErrAccessDenied
		}
		return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: "org_startup", ProviderOrganization: "provider_org_startup", Subject: origin.Subject, Role: currentRole, Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: currentPermission, BlockedProjects: currentBlocked, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
	}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
		seedStartupOrganization(t, store.postgres, org)
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			envelope, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x35}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				return err
			}
			if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
				return err
			}
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
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
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
	for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			project := tracker.ProjectID("project_" + string(org))
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "hidden_project", Name: "Private example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
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
			source := threadstore.Import{Thread: threadstore.Record{Thread: teamThread{ID: "th_" + string(org) + "_" + name, Scope: scope, ProjectID: project, IssueID: issue, Title: "Example " + name, Status: "open", Creator: "credential_first", CreatedAt: stamp, UpdatedAt: stamp, PostCount: 1}, RequestKey: "create_" + name, InputHash: strings.Repeat("a", 64)}, Posts: []threadstore.PostRecord{{Post: teamPost{Sequence: 1, Author: "credential_first", Name: "Example", Kind: "note", Body: "Preserved body " + string(org) + " " + name, CreatedAt: stamp}, RequestKey: "post_" + name, InputHash: strings.Repeat("b", 64)}}}
			if err := store.postgres.ImportTeamThread(t.Context(), org, source, cfg.RunDetails); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.database.reader != nil || service.database.db != nil {
		t.Fatal("fixture must use PostgreSQL without the legacy database")
	}
	requestHandler := service.Handler()
	var requestReadCounter *entryKeyReadCounter
	request := func(path, user, connection string, arguments any, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
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
		assertion, err := cloudassert.Sign(key, claims)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		if requestReadCounter != nil {
			req = req.WithContext(context.WithValue(req.Context(), entryKeyReadCounterContextKey{}, requestReadCounter))
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(cloudassert.Header, assertion)
		response := httptest.NewRecorder()
		requestHandler.ServeHTTP(response, req)
		return response
	}
	call := func(name string, args any, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
		return request("/internal/v1/keys/call", "first", "thread_reads", map[string]any{"name": name, "arguments": args, "connection_id": "thread_reads"}, mutate)
	}
	credential := apiCredential{ID: "credential_first", Hash: "key-hash-first", HostedPrincipal: "credential_first", Scope: apiScopeOperator, HostedRole: "owner", HostedKeyScope: apikey.ScopeRead, HostedProjectAccess: hostedProjectsAll, Hosted: &auth.HostedIdentity{Subject: "first", OrganizationID: "provider_org_startup"}}
	identity := operatorIdentity(credential, "org_startup")
	catalogContext := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity})
	catalogContext = context.WithValue(catalogContext, operatorCatalogKey{}, operatorCatalogAuthority{identity: identity, credential: credential})
	counter := &entryKeyReadCounter{}
	catalogContext = context.WithValue(catalogContext, entryKeyReadCounterContextKey{}, counter)
	counted := &entryKeyReadCountingRepository{Repository: service.config.TenantRepository}
	isolatedService := &Service{config: service.config, database: service.database}
	isolatedService.config.TenantRepository = counted
	available, err := isolatedService.entryKeyToolAvailable(catalogContext, operatortool.WorkItem)
	if err != nil || !available || counted.readCalls != 0 {
		t.Fatalf("selected work_item availability = (%t, %v), unrelated tenant reads = %d; want available with no reads", available, err, counted.readCalls)
	}
	available, err = isolatedService.entryKeyToolAvailable(catalogContext, operatortool.FileIssue)
	if err != nil || available || counted.readCalls != 0 {
		t.Fatalf("read-scoped FileIssue availability = (%t, %v), tenant reads = %d; want denied with no reads", available, err, counted.readCalls)
	}
	counted.readCalls = 0
	if _, err := isolatedService.entryKeyTools(catalogContext); err != nil {
		t.Fatalf("full catalog read: %v", err)
	}
	if counter.projectAccessPageReads.Load() == 0 {
		t.Fatal("full catalog path did not read unrelated runner grants")
	}
	isolatedService.echo = echo.New()
	isolatedService.ready.Store(true)
	isolatedService.operatorChat = service.operatorChat
	isolatedService.echo.POST("/internal/v1/keys/call", isolatedService.entryKeyCall, isolatedService.hostedSharedEntry, isolatedService.operatorAuthority)
	requestHandler = isolatedService.echo
	selectedReadCounter := &entryKeyReadCounter{}
	requestReadCounter = selectedReadCounter
	workItem := call(operatortool.WorkItem, map[string]any{"project_id": "project_org_startup", "reference": "wi_own"}, nil)
	requestReadCounter = nil
	requestHandler = service.Handler()
	requireNativeStatus(t, workItem, http.StatusOK)
	if reads := selectedReadCounter.projectAccessPageReads.Load(); reads != 0 {
		t.Fatalf("selected work_item call read unrelated runner grants %d times", reads)
	}
	foreignOrganizationItem := call(operatortool.WorkItem, map[string]any{"project_id": "project_org_other", "reference": "wi_foreign"}, nil)
	requireNativeStatus(t, foreignOrganizationItem, http.StatusForbidden)
	unavailableTool := call(operatortool.WorkHistory, map[string]any{"project_id": "project_org_startup", "reference": "wi_own"}, nil)
	requireNativeStatus(t, unavailableTool, http.StatusForbidden)
	if !strings.Contains(unavailableTool.Body.String(), "tool_unavailable") {
		t.Fatalf("unsupported tool response = %s", unavailableTool.Body.String())
	}
	catalog := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
	requireNativeStatus(t, catalog, http.StatusOK)
	var definitions []operatortool.Definition
	decodeHubResponse(t, catalog, &definitions)
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
		decodeHubResponse(t, response, &envelope)
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
		for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
			if err := service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
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
	requireNativeStatus(t, read, http.StatusOK)
	var body teamThreadRead
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
		if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
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
			catalog := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
			requireNativeStatus(t, catalog, http.StatusOK)
			decodeHubResponse(t, catalog, &definitions)
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
			requireNativeStatus(t, response, http.StatusOK)
			var page teamThreadPage
			decodeThread(response, &page)
			want := 4
			if role == "reporter" {
				want = 3
			}
			if len(page.Threads) != want {
				t.Fatalf("actual visible threads=%+v want %d", page, want)
			}
			for _, thread := range page.Threads {
				if strings.Contains(thread.ID, "org_other") || thread.ID == "th_org_startup_hidden" || role == "reporter" && thread.ID == "th_org_startup_foreign" {
					t.Fatalf("invisible thread listed: %+v", thread)
				}
			}
			response = call("get_thread", map[string]any{"thread_id": "th_org_startup_own"}, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeThread(response, &body)
			if len(body.Posts) != 1 || body.Posts[0].Body != "<transcript>\nPreserved body org_startup own\n</transcript>" {
				t.Fatalf("read role lost actual body: %+v", body)
			}
			if role == "reporter" {
				requireNativeStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_foreign"}, nil), http.StatusUnprocessableEntity)
			}
		})
	}
	filtered := call("list_threads", map[string]any{"project_id": "project_org_startup", "scope": "issue", "issue_id": "wi_own", "status": "open"}, nil)
	requireNativeStatus(t, filtered, http.StatusOK)
	var filteredPage teamThreadPage
	decodeThread(filtered, &filteredPage)
	if len(filteredPage.Threads) != 1 || filteredPage.Threads[0].ID != "th_org_startup_own" {
		t.Fatalf("thread filters lost: %+v", filteredPage)
	}
	requireNativeStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_hidden"}, nil), http.StatusUnprocessableEntity)
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
		requireNativeStatus(t, response, http.StatusForbidden)
		if !strings.Contains(response.Body.String(), "key_project_denied") {
			t.Fatalf("selected project denial changed: %s", response.Body.String())
		}
	}
	requireNativeStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_other_general"}, nil), http.StatusUnprocessableEntity)
	currentBlocked = []string{"project_org_startup"}
	requireNativeStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_project"}, nil), http.StatusUnprocessableEntity)
	currentBlocked = nil
	for _, name := range []string{"create_thread", "post_to_thread", "resolve_thread"} {
		requireNativeStatus(t, call(name, map[string]any{}, nil), http.StatusForbidden)
	}
	requireNativeStatus(t, call("post_to_thread", map[string]any{"thread_id": "th_org_startup_own", "kind": "note", "body": "Must be rejected"}, func(c *cloudassert.Claims) { c.Key.Permission = apikey.ScopeWrite }), http.StatusForbidden)
	entryRevoked = true
	requireNativeStatus(t, call("list_threads", map[string]any{}, nil), http.StatusForbidden)
	requireNativeStatus(t, call(operatortool.WorkItem, map[string]any{"project_id": "project_org_startup", "reference": "wi_own"}, nil), http.StatusForbidden)
	entryRevoked = false
	currentRole = "account"
	requireNativeStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_own"}, nil), http.StatusForbidden)
	requireNativeStatus(t, call(operatortool.WorkItem, map[string]any{"project_id": "project_org_startup", "reference": "wi_own"}, nil), http.StatusForbidden)
	currentRole = "owner"
	setRole("owner")
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), "first", "project_org_startup")
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, call("get_thread", map[string]any{"thread_id": "th_org_startup_project"}, nil), http.StatusUnprocessableEntity)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessCredential(t.Context(), "credential_first", time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, call("list_threads", map[string]any{}, nil), http.StatusForbidden)
}
