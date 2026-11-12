package hubserver

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
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

func threadUnreadCredential(t *testing.T, viewer nativeFixture) apiCredential {
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
	principal := operatorIdentity(credential, string(organization)).PrincipalID
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
	principal := operatorIdentity(credential, string(f.project.OrganizationID)).PrincipalID
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
	scope := nativeScope{organization: f.project.OrganizationID, credential: credential}
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
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, threadBase(foreign)+"/unread", f.token, nil), http.StatusUnauthorized)
}

func TestTeamThreadUnreadScopeChurnCannotAmplifyWork(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign_peer_%t", foreign), func(t *testing.T) {
			fixture := newThreadUnreadFixture(t)
			f := fixture.viewer(t, "org_startup", "unread-churn")
			second := fixture.viewer(t, "org_startup", "unread-churn-second")
			organization := f.project.OrganizationID
			if foreign {
				organization = "org_other"
			}
			peer := fixture.viewer(t, organization, "unread-peer")
			worker := fixture.workers[f.project.OrganizationID]
			peerWorker := fixture.workers[peer.project.OrganizationID]
			credential := threadUnreadCredential(t, f)
			principal := operatorIdentity(credential, string(f.project.OrganizationID)).PrincipalID
			err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: f.project.OrganizationID, UserID: credential.Hosted.Subject, ProjectID: second.project.ID, CanWrite: true}); err != nil {
					return err
				}
				return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: f.project.OrganizationID, TokenID: credential.ID, ProjectID: second.project.ID})
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, viewer := range []nativeFixture{f, second} {
				thread := createTestThread(t, viewer, "project", "")
				requireNativeStatus(t, performHubAPIRequest(t, viewer.service, http.MethodPost, threadBase(viewer)+"/"+thread.ID+"/posts", viewer.token, teamThreadInput{Kind: "note", Body: "Project finding"}), 201)
			}
			for _, viewer := range []nativeFixture{f, peer} {
				thread := createTestThread(t, viewer, "general", "")
				requireNativeStatus(t, performHubAPIRequest(t, viewer.service, http.MethodPost, threadBase(viewer)+"/"+thread.ID+"/posts", viewer.token, teamThreadInput{Kind: "note", Body: "Finding"}), 201)
				seedThreadUnreadHistory(t, viewer, string(viewer.project.ID), 150, "")
			}
			read := func(viewer nativeFixture, projects []string, refresh bool) teamUnreadSummary {
				t.Helper()
				query := url.Values{}
				for _, project := range projects {
					query.Add("project_id", project)
				}
				if refresh {
					query.Set("refresh", "true")
				}
				response := performHubAPIRequest(t, viewer.service, http.MethodGet, threadBase(viewer)+"/unread?"+query.Encode(), viewer.token, nil)
				requireNativeStatus(t, response, 200)
				var result teamUnreadSummary
				decodeHubResponse(t, response, &result)
				return result
			}
			subsets := [][]string{nil, {string(f.project.ID)}, {string(second.project.ID)}, {string(f.project.ID), string(second.project.ID)}}
			selected := 0
			read(f, subsets[selected], true)
			read(peer, nil, true)
			peerCompleted := false
			superseded := 0
			for range 24 {
				interrupted := false
				ctx := context.WithValue(t.Context(), threadUnreadTraceKey{}, func(data pgx.TraceQueryStartData) {
					if strings.HasPrefix(data.SQL, threadUnreadPageQuery) && data.Args[4] == principal {
						selected = (selected + 1) % len(subsets)
						read(f, subsets[selected], true)
						interrupted = true
					}
				})
				more, rows, err := worker.advanceTeamUnread(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if interrupted {
					superseded++
					if !more || rows != 0 {
						t.Fatalf("superseded page returned progress: more=%t rows=%d", more, rows)
					}
					var scan, partial int64
					if err := fixture.admin.QueryRowContext(t.Context(), "SELECT scan_revision,partial_count FROM team_thread_unread_summaries WHERE organization_id=$1 AND principal_id=$2", f.project.OrganizationID, principal).Scan(&scan, &partial); err != nil {
						t.Fatal(err)
					}
					if scan != 0 || partial != 0 {
						t.Fatalf("superseded scan overwrote replacement scope: scan=%d partial=%d", scan, partial)
					}
				}
				if foreign {
					if _, _, err := peerWorker.advanceTeamUnread(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				result := read(peer, nil, false)
				peerCompleted = peerCompleted || !result.Pending && result.Count != nil && *result.Count > 0
			}
			if superseded == 0 {
				t.Fatal("scope churn did not supersede any PostgreSQL page")
			}
			var summaries, projects int
			if err := fixture.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM team_thread_unread_summaries").Scan(&summaries); err != nil {
				t.Fatal(err)
			}
			if err := fixture.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM team_thread_unread_projects").Scan(&projects); err != nil {
				t.Fatal(err)
			}
			if summaries != 2 || projects > 2 {
				t.Errorf("scope churn retained %d summaries and %d projects; want two members and at most two scope rows", summaries, projects)
			}
			if !peerCompleted {
				t.Error("scope churn starved another member's summary")
			}
			read(f, subsets[3], false)
			for range 8 {
				if err := worker.maintainTeamUnread(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			wantGeneral := int64(302)
			if foreign {
				wantGeneral = 151
			}
			if got := read(f, subsets[3], false); got.Pending || got.Count == nil || *got.Count != wantGeneral+2 {
				t.Fatalf("settled scope=%+v", got)
			}
			if got := read(f, nil, false); !got.Pending || got.Count != nil {
				t.Fatalf("scope change exposed previous count: %+v", got)
			}
			if _, _, err := worker.advanceTeamUnread(t.Context()); err != nil {
				t.Fatal(err)
			}
			for range 20 {
				read(f, nil, true)
			}
			var requested, scan int64
			if err := fixture.admin.QueryRowContext(t.Context(), "SELECT requested_revision,scan_revision FROM team_thread_unread_summaries WHERE organization_id=$1 AND principal_id=$2", f.project.OrganizationID, principal).Scan(&requested, &scan); err != nil {
				t.Fatal(err)
			}
			if requested > scan+1 {
				t.Errorf("refreshes queued revisions: requested=%d scan=%d", requested, scan)
			}
			for range 4 {
				if err := worker.maintainTeamUnread(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if got := read(f, nil, false); got.Pending || got.Count == nil || *got.Count != wantGeneral {
				t.Fatalf("coalesced refresh included old project scope: %+v", got)
			}
		})
	}
}
