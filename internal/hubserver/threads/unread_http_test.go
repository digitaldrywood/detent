package threads_test

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/threads"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threadstore"
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

func seedThreadUnreadHistory(t *testing.T, viewer nativeFixture, prefix string, count int, readBy string) {
	t.Helper()
	ctx := t.Context()
	organization := viewer.project.OrganizationID
	credential := threadUnreadCredential(t, viewer)
	principal := hubauth.Identity(credential, string(organization)).PrincipalID
	at := hubapi.ConversationTime(viewer.service.Now())
	post, err := threadstore.NewContent(viewer.details).PreparePost(ctx, organization, threadstore.PostRecord{
		Post: teamPost{Author: principal, Name: "Example member", Kind: "note", Body: "Finding", CreatedAt: at}, RequestKey: "history-post", InputHash: strings.Repeat("a", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = viewer.service.Repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
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
			principal := hubauth.Identity(credential, string(f.project.OrganizationID)).PrincipalID
			err := f.service.Repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
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
				more, rows, err := worker.AdvanceUnread(ctx)
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
					if _, _, err := peerWorker.AdvanceUnread(t.Context()); err != nil {
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
				if err := worker.MaintainUnread(t.Context()); err != nil {
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
			if _, _, err := worker.AdvanceUnread(t.Context()); err != nil {
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
				if err := worker.MaintainUnread(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if got := read(f, nil, false); got.Pending || got.Count == nil || *got.Count != wantGeneral {
				t.Fatalf("coalesced refresh included old project scope: %+v", got)
			}
		})
	}
}

type threadUnreadFixture struct {
	admin    *sql.DB
	services map[tracker.OrganizationID]*hubtest.HTTPHarness
	workers  map[tracker.OrganizationID]*threads.Service
	details  map[tracker.OrganizationID]*hubtest.Details
	clock    *hubtest.Clock
}

func newThreadUnreadFixture(t *testing.T) threadUnreadFixture {
	t.Helper()
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	traced := hubtest.RepositoryOn(t, admin, hubtest.WithPoolConfig(func(config *pgxpool.Config) { config.ConnConfig.Tracer = threadUnreadQueryTrace{} }))
	fixture := threadUnreadFixture{admin: admin, services: map[tracker.OrganizationID]*hubtest.HTTPHarness{}, workers: map[tracker.OrganizationID]*threads.Service{}, details: map[tracker.OrganizationID]*hubtest.Details{}, clock: hubtest.NewClock(time.Now().UTC())}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_other"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Now: fixture.clock.Now()})
		h, _, details := threadHTTP(t, repository, organization, fixture.clock, true)
		fixture.services[organization] = h
		fixture.details[organization] = details
		fixture.workers[organization] = threads.New(threads.Deps{Repository: traced, Details: func() attachment.DetailStore { return details }, Now: fixture.clock.Now, Organization: func() tracker.OrganizationID { return organization }, RecordError: hubauth.RecordError})
	}
	return fixture
}
func (fixture threadUnreadFixture) viewer(t *testing.T, organization tracker.OrganizationID, name string) nativeFixture {
	t.Helper()
	h := fixture.services[organization]
	project := hubtest.Project(t, h.Repository, organization, hubtest.ProjectOptions{Name: name})
	actor := h.MemberSession(t, hubtest.MemberOptions{Role: "owner", Projects: []tracker.ProjectID{project.ID}})
	return nativeFixture{service: h, project: project, token: actor.Member.Principal.Token, clock: fixture.clock, details: fixture.details[organization]}
}
func threadUnreadCredential(t *testing.T, viewer nativeFixture) hubauth.Credential {
	t.Helper()
	credential, status, err := viewer.service.Deps.Auth.AuthenticateRepositoryAPIHash(t.Context(), viewer.service.Repository, viewer.project.OrganizationID, apikey.HashToken(viewer.token), "", "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("thread fixture credential status=%d: %v", status, err)
	}
	return credential
}
