package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresThreadsPreservePostsPermissionsAndUnread(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	now := time.Now().UTC()
	for _, role := range []string{"owner", "reporter"} {
		if _, err := cfg.Hosted.Provider.CreateMembership(t.Context(), "user_"+role, "provider_org_startup", role); err != nil {
			t.Fatal(err)
		}
	}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_startup", "org_second"} {
		seedStartupOrganization(t, store.postgres, org)
		err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, role := range []string{"owner", "reporter"} {
				user := "user_" + role
				credential := "credential_" + role
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credential, Name: "Example " + role, Hash: apikey.HashToken("example_thread_" + string(org) + role), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: user, MembershipID: "membership_" + user, KeyScope: "write", ProjectAccess: "all", Kind: "personal"}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: user, Email: role + "@example.test", MembershipID: "membership_" + user, Role: role, Active: true, PrincipalID: credential, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			for _, id := range []tracker.ProjectID{"project_first", "project_second"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: id, Name: string(id), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				actor := "credential_reporter"
				if id == "project_second" {
					actor = "credential_owner"
				}
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: id, NativeID: tracker.NativeWorkItemID("wi_" + string(id)), Number: 1, Title: "Example issue", ActorPrincipalID: &actor}); err != nil {
					return err
				}
				for _, role := range []string{"owner", "reporter"} {
					if role == "reporter" && id == "project_second" {
						continue
					}
					if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "user_" + role, ProjectID: id, CanWrite: true, ManageRunner: role == "owner"}); err != nil {
						return err
					}
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "credential_" + role, ProjectID: id}); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, project := range []string{"project_first", "project_second"} {
			timestamp := "2026-11-01T12:00:00.123456789Z"
			source := threadstore.Import{Thread: threadstore.Record{Thread: teamThread{ID: "th_" + project, Scope: "issue", ProjectID: project, IssueID: "wi_" + project, Title: "Example imported research " + project, Status: "open", Creator: "credential_owner", CreatorName: "Example owner", CreatorClient: "Example client", PostCount: 2, Preview: "Preserved source post", CreatedAt: timestamp, UpdatedAt: timestamp, Participants: []teamParticipant{{PrincipalID: "credential_owner", Name: "Example owner", Client: "Example client"}}, References: []teamReference{{Kind: "issue", ID: "wi_" + project, ProjectID: project}}}, RequestKey: "create_" + project, InputHash: strings.Repeat("a", 64)}, Reads: []threadstore.Cursor{{Principal: "credential_reporter", LastSequence: 1}}}
			postHash, err := nativeCommandHash(struct {
				Kind, Body string
				References []teamReference
			}{"note", "Preserved source post", []teamReference{{Kind: "issue", ID: "wi_" + project, ProjectID: project}}})
			if err != nil {
				t.Fatal(err)
			}
			for seq := int64(1); seq <= 2; seq++ {
				source.Posts = append(source.Posts, threadstore.PostRecord{Post: teamPost{Sequence: seq, Author: "credential_owner", Name: "Example owner", ActingFor: "credential_owner", Client: "Example client", Kind: "note", Body: "Preserved source post", CreatedAt: timestamp, References: []teamReference{{Kind: "issue", ID: "wi_" + project, ProjectID: project}}}, RequestKey: "post_" + strconv.FormatInt(seq, 10), InputHash: postHash})
			}
			var firstBytes int64
			for attempt := range 2 {
				if err := store.postgres.ImportTeamThread(t.Context(), org, source, cfg.RunDetails); err != nil {
					t.Fatal(err)
				}
				var bytes int64
				if err := store.postgres.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error { var err error; bytes, err = r.ArchiveBytes(t.Context()); return err }); err != nil {
					t.Fatal(err)
				}
				if bytes <= 0 || attempt == 1 && bytes != firstBytes {
					t.Fatalf("import replay lost archive ownership or double charged: first=%d current=%d", firstBytes, bytes)
				}
				firstBytes = bytes
			}

			if org == "org_startup" && project == "project_first" {
				changed := source
				changed.Posts = append([]threadstore.PostRecord{}, source.Posts...)
				changed.Posts[0].Body = "Changed source must be refused"
				if err := store.postgres.ImportTeamThread(t.Context(), org, changed, cfg.RunDetails); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("changed retained source was accepted: %v", err)
				}
			}

		}
	}
	for _, org := range []string{"org_startup", "org_second"} {
		var owners int
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND owner_kind='team-thread-post'", org).Scan(&owners); err != nil {
			t.Fatal(err)
		}
		if owners != 4 {
			t.Fatalf("imported body ownership count=%d want4", owners)
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
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if service.database.db != nil || service.database.reader != nil {
		t.Fatal("PostgreSQL threads retained legacy SQL handles")
	}
	const owner = "example_thread_org_startupowner"
	const reporter = "example_thread_org_startupreporter"
	base := "/api/v2/organizations/org_startup/threads"
	request := func(method, suffix, token string, payload any) *httptest.ResponseRecorder {
		return performHubAPIRequest(t, service, method, base+suffix, token, payload)
	}
	list := func(suffix, token string) teamThreadPage {
		t.Helper()
		response := request(http.MethodGet, suffix, token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var page teamThreadPage
		decodeHubResponse(t, response, &page)
		return page
	}
	if page := list("?limit=1", owner); len(page.Threads) != 1 || page.NextCursor == "" || len(list("?limit=1&cursor="+url.QueryEscape(page.NextCursor), owner).Threads) != 1 {
		t.Fatalf("imported thread keyset page=%+v", page)
	}
	if page := list("?search=resear", owner); len(page.Threads) != 2 {
		t.Fatalf("prefix thread search omitted imports: %+v", page)
	}
	if page := list("", reporter); len(page.Threads) != 1 || page.Threads[0].ID != "th_project_first" || len(page.Threads[0].Participants) != 1 || len(page.Threads[0].References) != 1 || page.Threads[0].IssueNumber != 1 {
		t.Fatalf("reporter thread/reference visibility=%+v", page)
	}
	requireNativeStatus(t, request(http.MethodGet, "/th_project_second", reporter, nil), http.StatusNotFound)
	sourceReplay := request(http.MethodPost, "/th_project_first/posts", owner, teamThreadInput{Kind: "note", Body: "Preserved source post", Key: "post_1", References: []operatortool.ThreadReference{{Kind: "issue", ID: "wi_project_first"}}})
	requireNativeStatus(t, sourceReplay, http.StatusCreated)
	var replayed teamPost
	decodeHubResponse(t, sourceReplay, &replayed)
	if replayed.Sequence != 1 || replayed.Name != "Example owner" || replayed.CreatedAt != "2026-11-01T12:00:00.123456789Z" {
		t.Fatalf("imported idempotency identity was not preserved: %+v", replayed)
	}
	readResponse := request(http.MethodGet, "/th_project_first?limit=1", owner, nil)
	requireNativeStatus(t, readResponse, http.StatusOK)
	var read teamThreadRead
	decodeHubResponse(t, readResponse, &read)
	if len(read.Posts) != 1 || read.Posts[0].Body != "Preserved source post" || read.Posts[0].CreatedAt != "2026-11-01T12:00:00.123456789Z" || read.Cursor != "1" || !read.HasMore || len(read.Posts[0].References) != 1 {
		t.Fatalf("imported post page=%+v", read)
	}
	readResponse = request(http.MethodGet, "/th_project_first?since=last_read", owner, nil)
	requireNativeStatus(t, readResponse, http.StatusOK)
	decodeHubResponse(t, readResponse, &read)
	if len(read.Posts) != 1 || read.Posts[0].Sequence != 2 || read.HasMore || read.Thread.Unread != 0 {
		t.Fatalf("thread last-read continuation=%+v", read)
	}
	create := teamThreadInput{Scope: "general", Title: "Example shared note", Key: "example_create", References: []operatortool.ThreadReference{{Kind: "issue", ID: "wi_project_second"}}}
	var created teamThread
	for range 2 {
		response := request(http.MethodPost, "", owner, create)
		requireNativeStatus(t, response, http.StatusCreated)
		decodeHubResponse(t, response, &created)
	}
	var newPostBytes int64
	for attempt := range 2 {
		response := request(http.MethodPost, "/"+created.ID+"/posts", owner, teamThreadInput{Kind: "decision", Body: "Example decision", Key: "example_post"})
		requireNativeStatus(t, response, http.StatusCreated)
		var post teamPost
		decodeHubResponse(t, response, &post)
		if post.Sequence != 1 {
			t.Fatalf("replayed post advanced sequence: %+v", post)
		}
		var bytes int64
		if err := service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error { var err error; bytes, err = r.ArchiveBytes(t.Context()); return err }); err != nil {
			t.Fatal(err)
		}
		if bytes <= 0 || attempt == 1 && bytes != newPostBytes {
			t.Fatalf("post replay lost archive ownership or double charged: first=%d current=%d", newPostBytes, bytes)
		}
		newPostBytes = bytes
	}

	for i := 2; i <= 5; i++ {
		body := fmt.Sprintf("Example decision %d", i)
		if i == 5 {
			body = strings.Repeat("Example ", 40)
		}
		requireNativeStatus(t, request(http.MethodPost, "/"+created.ID+"/posts", owner, teamThreadInput{Kind: "note", Body: body, Key: fmt.Sprintf("example_post_%d", i)}), http.StatusCreated)
	}
	details := cfg.RunDetails.(*memoryRunDetails)
	originalRepository := service.config.TenantRepository
	bodyRepository := &threadBodyReadRepository{Repository: originalRepository}
	service.config.TenantRepository = bodyRepository
	entered := make(chan struct{}, 5)
	releaseBodies := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBodies) }) }
	var reads, active atomic.Int32
	details.beforeRead = func(ctx context.Context) error {
		if reads.Add(1) == 1 {
			return nil
		}
		active.Add(1)
		defer active.Add(-1)
		entered <- struct{}{}
		select {
		case <-releaseBodies:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var requestWorkers sync.WaitGroup
	requestWorkers.Add(1)
	t.Cleanup(func() { release(); requestWorkers.Wait(); details.beforeRead = nil })
	hydrated := make(chan *httptest.ResponseRecorder, 1)
	hydrateRequest := newHubAPIRequest(t, http.MethodGet, base+"/"+created.ID, owner, nil)
	go func() {
		defer requestWorkers.Done()
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, hydrateRequest)
		hydrated <- response
	}()
	for range 4 {
		select {
		case <-entered:
		case response := <-hydrated:
			t.Fatalf("post hydration returned before four bounded reads: %d %s", response.Code, response.Body.String())
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		}
	}
	if active.Load() != 4 || bodyRepository.active.Load() != 0 {
		t.Fatalf("body reads held a database connection or exceeded concurrency: active=%d database=%d", active.Load(), bodyRepository.active.Load())
	}
	select {
	case <-entered:
		t.Fatal("fifth post body read exceeded concurrency four")
	default:
	}
	release()
	hydrateResponse := <-hydrated
	requestWorkers.Wait()
	details.beforeRead = nil
	service.config.TenantRepository = originalRepository
	requireNativeStatus(t, hydrateResponse, http.StatusOK)
	var hydratedPage teamThreadRead
	decodeHubResponse(t, hydrateResponse, &hydratedPage)
	if len(hydratedPage.Posts) != 5 || hydratedPage.Cursor != "5" || !strings.HasSuffix(hydratedPage.Thread.Preview, "…") {
		t.Fatalf("bounded immutable post hydration lost body/preview: %+v", hydratedPage)
	}
	if page := list("?scope=general", reporter); len(page.Threads) != 1 || len(page.Threads[0].References) != 0 {
		t.Fatalf("general thread leaked hidden reference: %+v", page)
	}
	requireNativeStatus(t, request(http.MethodPatch, "/"+created.ID, owner, teamThreadInput{Status: "resolved"}), http.StatusOK)
	if page := list("?status=resolved", owner); len(page.Threads) != 1 || page.Threads[0].ID != created.ID {
		t.Fatalf("resolved thread filter=%+v", page)
	}
	requireNativeStatus(t, request(http.MethodPost, "/"+created.ID+"/posts", owner, teamThreadInput{Kind: "decision", Body: "Changed decision", Key: "example_post"}), http.StatusConflict)
	unreadResponse := request(http.MethodGet, "/unread?project_id=project_first&refresh=true", reporter, nil)
	requireNativeStatus(t, unreadResponse, http.StatusOK)
	if err := service.maintainTeamUnread(t.Context()); err != nil {
		t.Fatal(err)
	}
	unreadResponse = request(http.MethodGet, "/unread?project_id=project_first", reporter, nil)
	requireNativeStatus(t, unreadResponse, http.StatusOK)
	var unread teamUnreadSummary
	decodeHubResponse(t, unreadResponse, &unread)
	if unread.Pending || unread.Count == nil || *unread.Count != 2 {
		t.Fatalf("typed unread summary=%+v", unread)
	}

	for i := range 51 {
		id := fmt.Sprintf("th_page_%03d", i)
		post, err := threadstore.NewContent(cfg.RunDetails).PreparePost(t.Context(), "org_startup", threadstore.PostRecord{Post: teamPost{Author: "credential_owner", Name: "Example owner", Kind: "note", Body: "Example unread", CreatedAt: conversationTime(now)}, RequestKey: id, InputHash: strings.Repeat("d", 64)})
		if err != nil {
			t.Fatal(err)
		}
		err = service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			timestamp := conversationTime(now)
			if err := w.CreateThread(t.Context(), threadstore.Record{Thread: teamThread{ID: id, Scope: "general", Title: "Example continuation", Status: "open", Creator: "credential_owner", CreatorName: "Example owner", CreatedAt: timestamp, UpdatedAt: timestamp}, RequestKey: id, InputHash: strings.Repeat("c", 64)}); err != nil {
				return err
			}
			_, err := w.AppendThreadPost(t.Context(), id, post)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	requireNativeStatus(t, request(http.MethodGet, "/unread?project_id=project_first&refresh=true", reporter, nil), http.StatusOK)
	more, rows, err := service.advanceTeamUnreadRecord(t.Context())
	if err != nil || !more || rows != 50 {
		t.Fatalf("bounded unread first page: more=%t rows=%d err=%v", more, rows, err)
	}
	if err := service.maintainTeamUnread(t.Context()); err != nil {
		t.Fatal(err)
	}
	unreadResponse = request(http.MethodGet, "/unread?project_id=project_first", reporter, nil)
	requireNativeStatus(t, unreadResponse, http.StatusOK)
	decodeHubResponse(t, unreadResponse, &unread)
	if unread.Pending || unread.Count == nil || *unread.Count != 53 {
		t.Fatalf("unread continuation lost or duplicated threads: %+v", unread)
	}
	for _, actor := range []string{"credential_owner", "credential_reporter"} {
		err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
			item, err := w.WorkItem(t.Context(), "project_first", "wi_project_first")
			if err != nil {
				return err
			}
			item.ActorPrincipalID = &actor
			_, err = w.UpdateWorkItem(t.Context(), item)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		want := http.StatusNotFound
		if actor == "credential_reporter" {
			want = http.StatusOK
		}
		requireNativeStatus(t, request(http.MethodGet, "/th_project_first", reporter, nil), want)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/org_second/threads", owner, nil), http.StatusNotFound)
	if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), "user_reporter", "project_first")
	}); err != nil {
		t.Fatal(err)
	}
	if page := list("", reporter); len(page.Threads) != 25 || page.Threads[0].Scope != "general" {
		t.Fatalf("revoked project remained visible: %+v", page)
	}
}

type threadBodyReadRepository struct {
	tenantstore.Repository
	active atomic.Int32
}

func (r *threadBodyReadRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.active.Add(1)
	defer r.active.Add(-1)
	return r.Repository.ReadTenant(ctx, organization, workload, read)
}

func TestThreadPostHydrationReturnsCanceledContext(t *testing.T) {
	service := &Service{config: Config{RunDetails: &memoryRunDetails{}}}
	for _, tc := range []struct {
		name  string
		posts []threadstore.PostRecord
	}{{name: "empty"}, {name: "pending", posts: []threadstore.PostRecord{{}}}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := service.hydrateThreadPosts(ctx, "org_example", tc.posts); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled body hydration returned success: %v", err)
			}
		})
	}
}
