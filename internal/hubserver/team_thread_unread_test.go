package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type threadUnreadTraceKey struct{}
type threadUnreadQueryTrace struct{}

func (threadUnreadQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if observe, ok := ctx.Value(threadUnreadTraceKey{}).(func(pgx.TraceQueryStartData)); ok {
		observe(data)
	}
	return ctx
}

func (threadUnreadQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

const threadUnreadPageQuery = "WITH page AS (SELECT id,scope,project_id,work_item_id,post_count FROM team_threads"

type threadUnreadFixture struct {
	admin    *sql.DB
	services map[tracker.OrganizationID]*Service
	workers  map[tracker.OrganizationID]*Service
}

func newThreadUnreadFixture(t *testing.T) threadUnreadFixture {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	organizations := []tracker.OrganizationID{"org_startup", "org_other"}
	for _, organization := range organizations {
		binding := *cfg.Hosted
		binding.OrganizationID, binding.WorkOSOrganizationID = string(organization), "provider_"+string(organization)
		seedStartupOrganization(t, store.postgres, organization, &binding)
	}
	poolConfig, err := cfg.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	pgtransport.Configure(poolConfig)
	poolConfig.MaxConns = int32(cfg.Postgres.Pool.MaxConnections)
	poolConfig.ConnConfig.Tracer = threadUnreadQueryTrace{}
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, cfg.Postgres.Pool)
	if err != nil {
		t.Fatal(err)
	}
	fixture := threadUnreadFixture{admin: admin, services: make(map[tracker.OrganizationID]*Service), workers: make(map[tracker.OrganizationID]*Service)}
	for _, organization := range organizations {
		tenantConfig := cfg
		hosted := *cfg.Hosted
		hosted.OrganizationID, hosted.WorkOSOrganizationID = string(organization), "provider_"+string(organization)
		tenantConfig.Hosted = &hosted
		service := openTestService(t, tenantConfig)
		if err := service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		fixture.services[organization] = service
		workerConfig := service.config
		workerConfig.TenantRepository = repository
		fixture.workers[organization] = &Service{config: workerConfig}
	}
	return fixture
}

func (fixture threadUnreadFixture) viewer(t *testing.T, organization tracker.OrganizationID, name string) nativeFixture {
	t.Helper()
	service := fixture.services[organization]
	ctx := t.Context()
	user := "example_" + name
	id := "credential_" + name
	token := "token_" + name
	member, err := service.config.Hosted.Provider.CreateMembership(ctx, user, service.config.Hosted.WorkOSOrganizationID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	project := tracker.NativeProject{OrganizationID: organization, ID: tracker.ProjectID("prj_" + name), Name: name, Profile: "native", States: nativeFixtureStates()}
	now := service.config.now().UTC()
	err = service.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project.ID, Name: name, Profile: "native"}, States: project.States}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: organization, ID: id, Name: name, Hash: apikey.HashToken(token), Scope: "operator", NativeOnly: true, Kind: "personal", KeyScope: "write", ProjectAccess: "selected", UserID: user, MembershipID: member.ID, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(ctx, accessstore.Member{OrganizationID: organization, UserID: user, MembershipID: member.ID, PrincipalID: id, Role: "owner", Active: true, Email: user + "@example.test", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(ctx, accessstore.ProjectGrant{OrganizationID: organization, UserID: user, ProjectID: project.ID, CanWrite: true}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: organization, TokenID: id, ProjectID: project.ID})
	})
	if err != nil {
		t.Fatal(err)
	}
	return nativeFixture{service: service, project: project, token: token}
}

func threadUnreadCredential(t *testing.T, viewer nativeFixture) hubapi.APICredential {
	t.Helper()
	credential, status, err := viewer.service.authenticateTenantAPIHash(t.Context(), viewer.project.OrganizationID, apikey.HashToken(viewer.token), "", "")
	if err != nil {
		t.Fatalf("thread fixture credential status=%d: %v", status, err)
	}
	return credential
}

func seedThreadUnreadHistory(t *testing.T, viewer nativeFixture, prefix string, count int, readBy string) {
	t.Helper()
	ctx := t.Context()
	organization := viewer.project.OrganizationID
	credential := threadUnreadCredential(t, viewer)
	principal := hubscope.OperatorIdentity(credential, string(organization)).PrincipalID
	at := conversationTime(viewer.service.config.now())
	post, err := threadstore.NewContent(viewer.service.runDetailStore()).PreparePost(ctx, organization, threadstore.PostRecord{
		Post: teamPost{Author: principal, Name: "Example member", Kind: "note", Body: "Finding", CreatedAt: at}, RequestKey: "history-post", InputHash: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = viewer.service.config.TenantRepository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		for i := range count {
			id := fmt.Sprintf("th_%s_%03d", prefix, i)
			record := threadstore.Record{Thread: teamThread{ID: id, Scope: "general", Title: "Shared research", Status: "open", Creator: principal, CreatorName: "Example member", CreatedAt: at, UpdatedAt: at}, RequestKey: id, InputHash: strings.Repeat("b", 64)}
			if err := w.CreateThread(ctx, record); err != nil {
				return err
			}
			if _, err := w.AppendThreadPost(ctx, id, post); err != nil {
				return err
			}
			if readBy != "" {
				if i%2 == 0 {
					if err := w.ResolveThread(ctx, id, "resolved"); err != nil {
						return err
					}
				}
				if err := w.AdvanceThreadRead(ctx, id, readBy, 1); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTeamThreadUnreadSummaryBoundsHistoryWork(t *testing.T) {
	t.Parallel()
	fixture := newThreadUnreadFixture(t)
	f := fixture.viewer(t, "org_startup", "unread-first")
	second := fixture.viewer(t, "org_startup", "unread-second")
	foreign := fixture.viewer(t, "org_other", "unread-foreign")
	worker := fixture.workers[f.project.OrganizationID]
	post := func(viewer nativeFixture, scope string) teamThread {
		t.Helper()
		thread := createTestThread(t, viewer, scope, "")
		requireNativeStatus(t, performHubAPIRequest(t, viewer.service, http.MethodPost, threadBase(viewer)+"/"+thread.ID+"/posts", viewer.token, teamThreadInput{Kind: "note", Body: "Finding"}), 201)
		return thread
	}
	general := post(f, "general")
	unread := post(f, "project")
	post(second, "project")
	post(foreign, "general")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, threadBase(f)+"/"+general.ID, f.token, nil), 200)
	credential := threadUnreadCredential(t, f)
	principal := hubscope.OperatorIdentity(credential, string(f.project.OrganizationID)).PrincipalID
	seedThreadUnreadHistory(t, f, "read", 253, principal)
	read := func(viewer nativeFixture, project tracker.ProjectID, refresh bool) teamUnreadSummary {
		t.Helper()
		path := threadBase(viewer) + "/unread"
		if project != "" {
			path += "?project_id=" + string(project)
		} else {
			path += "?"
		}
		if refresh {
			path += "&refresh=true"
		}
		response := performHubAPIRequest(t, viewer.service, http.MethodGet, path, viewer.token, nil)
		requireNativeStatus(t, response, 200)
		var result teamUnreadSummary
		decodeHubResponse(t, response, &result)
		return result
	}
	if got := read(f, f.project.ID, true); !got.Pending || got.Count != nil {
		t.Fatalf("new summary=%+v", got)
	}
	var statements []string
	var pageLimits []string
	ctx := context.WithValue(t.Context(), threadUnreadTraceKey{}, func(data pgx.TraceQueryStartData) {
		statements = append(statements, data.SQL)
		if strings.HasPrefix(data.SQL, threadUnreadPageQuery) {
			pageLimits = append(pageLimits, fmt.Sprint(data.Args[3]))
		}
	})
	if err := worker.maintainTeamUnread(ctx); err != nil {
		t.Fatal(err)
	}
	pages := len(pageLimits)
	for _, limit := range pageLimits {
		if limit != "51" {
			t.Fatalf("unread page limit=%s, want 50 rows plus one lookahead", limit)
		}
	}
	if pages != 4 {
		t.Fatalf("one maintenance tick scanned %d pages, want fixed budget 4", pages)
	}
	if got := read(f, f.project.ID, false); !got.Pending || got.Count != nil {
		t.Fatalf("large history completed beyond tick budget: %+v", got)
	}
	if err := worker.maintainTeamUnread(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := read(f, f.project.ID, false); got.Pending || got.Count == nil || *got.Count != 1 {
		t.Fatalf("older unread thread missing: %+v", got)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Credential: credential}
	statements = nil
	if _, err := worker.requestTeamUnread(ctx, scope, []string{string(f.project.ID)}, false); err != nil {
		t.Fatal(err)
	}
	if len(statements) == 0 {
		t.Fatal("unread request did not execute traced PostgreSQL statements")
	}
	for _, query := range statements {
		if strings.Contains(query, "FROM team_threads") || strings.Contains(query, "FROM team_thread_reads") || strings.Contains(query, "team_thread_participants") {
			t.Fatalf("summary request walked history or metadata: %s", query)
		}
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, threadBase(f)+"/"+unread.ID, f.token, nil), 200)
	if got := read(f, f.project.ID, true); !got.Pending || got.Count == nil || *got.Count != 1 {
		t.Fatalf("refresh lost last completed count: %+v", got)
	}
	for range 2 {
		if err := worker.maintainTeamUnread(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if got := read(f, f.project.ID, false); got.Pending || got.Count == nil || *got.Count != 0 {
		t.Fatalf("read cursor not projected: %+v", got)
	}
	for _, tc := range []struct {
		name    string
		viewer  nativeFixture
		project tracker.ProjectID
		want    int64
	}{
		{"organization general", second, "", 254},
		{"second project", second, second.project.ID, 255},
		{"foreign organization", foreign, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read(tc.viewer, tc.project, true)
			worker := fixture.workers[tc.viewer.project.OrganizationID]
			for range 2 {
				if err := worker.maintainTeamUnread(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			got := read(tc.viewer, tc.project, false)
			if got.Pending || got.Count == nil || *got.Count != tc.want {
				t.Fatalf("scope summary=%+v want %d", got, tc.want)
			}
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, threadBase(f)+"/unread?project_id="+string(second.project.ID), f.token, nil), 403)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, threadBase(foreign)+"/unread", f.token, nil), http.StatusNotFound)
}
