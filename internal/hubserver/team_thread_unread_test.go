package hubserver

import (
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTeamThreadUnreadSummaryBoundsHistoryWork(t *testing.T) {
	f := newNativeFixture(t, nil, "", "unread-first")
	second := newNativeFixture(t, f.service, f.project.OrganizationID, "unread-second")
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,created_at) VALUES('org_unread_other','Other',?)", conversationTime(f.service.config.now())); err != nil {
		t.Fatal(err)
	}
	foreign := newNativeFixture(t, f.service, "org_unread_other", "unread-foreign")
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
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
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	principal := operatorIdentity(credential, string(f.project.OrganizationID)).PrincipalID
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 253 {
		id := fmt.Sprintf("th_read_%06d", i)
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO team_threads(id,organization_id,scope,title,status,creator_principal,creator_name,creator_client,request_key,input_hash,post_count,created_at,updated_at) SELECT ?,organization_id,scope,title,CASE WHEN ?%2=0 THEN 'resolved' ELSE 'open' END,creator_principal,creator_name,creator_client,?,input_hash,post_count,created_at,updated_at FROM team_threads WHERE id=?`, id, i, id, general.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO team_thread_posts SELECT ?,seq,author_principal,author_name,acting_for,client_name,kind,body,request_key,input_hash,created_at FROM team_thread_posts WHERE thread_id=?`, id, general.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO team_thread_reads(thread_id,principal_id,last_seq) VALUES(?,?,1)", id, principal); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
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
	ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, args []driver.NamedValue) {
		statements = append(statements, query)
		if strings.Contains(query, "FROM (SELECT id,scope,project_id,post_count FROM team_threads") {
			pageLimits = append(pageLimits, fmt.Sprint(args[4].Value))
		}
	}})
	if err := f.service.maintainTeamUnread(ctx); err != nil {
		t.Fatal(err)
	}
	pages := 0
	for _, query := range statements {
		if strings.Contains(query, "FROM (SELECT id,scope,project_id,post_count FROM team_threads") {
			pages++
		}
	}
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
	if err := f.service.maintainTeamUnread(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := read(f, f.project.ID, false); got.Pending || got.Count == nil || *got.Count != 1 {
		t.Fatalf("older unread thread missing: %+v", got)
	}
	scope := nativeScope{organization: f.project.OrganizationID, credential: credential}
	statements = nil
	if _, err := f.service.requestTeamUnread(ctx, scope, []string{string(f.project.ID)}, false); err != nil {
		t.Fatal(err)
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
		if err := f.service.maintainTeamUnread(t.Context()); err != nil {
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
			for range 2 {
				if err := f.service.maintainTeamUnread(t.Context()); err != nil {
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
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, threadBase(foreign)+"/unread", f.token, nil), 403)
}

func TestTeamThreadUnreadScopeChurnCannotAmplifyWork(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign_peer_%t", foreign), func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "unread-churn")
			second := newNativeFixture(t, f.service, f.project.OrganizationID, "unread-churn-second")
			organization := f.project.OrganizationID
			if foreign {
				organization = "org_unread_peer"
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)", organization, "Example peer", conversationTime(f.service.config.now())); err != nil {
					t.Fatal(err)
				}
			}
			peer := newNativeFixture(t, f.service, organization, "unread-peer")
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES(?,?,?)", credential.ID, f.project.OrganizationID, second.project.ID); err != nil {
				t.Fatal(err)
			}
			for _, viewer := range []nativeFixture{f, second} {
				thread := createTestThread(t, viewer, "project", "")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, threadBase(viewer)+"/"+thread.ID+"/posts", viewer.token, teamThreadInput{Kind: "note", Body: "Project finding"}), 201)
			}
			for _, viewer := range []nativeFixture{f, peer} {
				thread := createTestThread(t, viewer, "general", "")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, threadBase(viewer)+"/"+thread.ID+"/posts", viewer.token, teamThreadInput{Kind: "note", Body: "Finding"}), 201)
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				for i := range 150 {
					id := fmt.Sprintf("th_churn_%t_%s_%03d", foreign, viewer.project.ID, i)
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO team_threads(id,organization_id,scope,title,status,creator_principal,creator_name,creator_client,request_key,input_hash,post_count,created_at,updated_at) SELECT ?,organization_id,scope,title,status,creator_principal,creator_name,creator_client,?,input_hash,post_count,created_at,updated_at FROM team_threads WHERE id=?`, id, id, thread.ID); err != nil {
						t.Fatal(err)
					}
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
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
				response := performHubAPIRequest(t, f.service, http.MethodGet, threadBase(viewer)+"/unread?"+query.Encode(), viewer.token, nil)
				requireNativeStatus(t, response, 200)
				var result teamUnreadSummary
				decodeHubResponse(t, response, &result)
				return result
			}
			read(peer, nil, true)
			subsets := [][]string{nil, {string(f.project.ID)}, {string(second.project.ID)}, {string(f.project.ID), string(second.project.ID)}}
			principal := operatorIdentity(credential, string(f.project.OrganizationID)).PrincipalID
			peerCompleted := false
			for i := range 24 {
				read(f, subsets[i%len(subsets)], true)
				interrupted := false
				ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, args []driver.NamedValue) {
					if strings.Contains(query, "FROM (SELECT id,scope,project_id,post_count FROM team_threads") && args[5].Value == principal {
						read(f, subsets[(i+1)%len(subsets)], true)
						interrupted = true
					}
				}})
				if _, _, err := f.service.database.advanceTeamUnread(ctx); err != nil {
					t.Fatal(err)
				}
				if interrupted {
					var scan, partial int64
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT scan_revision,partial_count FROM team_thread_unread_summaries WHERE organization_id=? AND principal_id=?", f.project.OrganizationID, principal).Scan(&scan, &partial); err != nil {
						t.Fatal(err)
					}
					if scan != 0 || partial != 0 {
						t.Fatalf("superseded scan overwrote replacement scope: scan=%d partial=%d", scan, partial)
					}
				}
				result := read(peer, nil, false)
				peerCompleted = peerCompleted || !result.Pending && result.Count != nil && *result.Count > 0
			}
			var summaries, projects int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM team_thread_unread_summaries").Scan(&summaries); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM team_thread_unread_projects").Scan(&projects); err != nil {
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
				if err := f.service.maintainTeamUnread(t.Context()); err != nil {
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
			if _, _, err := f.service.database.advanceTeamUnread(t.Context()); err != nil {
				t.Fatal(err)
			}
			for range 20 {
				read(f, nil, true)
			}
			var requested, scan int64
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT requested_revision,scan_revision FROM team_thread_unread_summaries WHERE organization_id=? AND principal_id=?", f.project.OrganizationID, principal).Scan(&requested, &scan); err != nil {
				t.Fatal(err)
			}
			if requested > scan+1 {
				t.Errorf("refreshes queued revisions: requested=%d scan=%d", requested, scan)
			}
			for range 4 {
				if err := f.service.maintainTeamUnread(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if got := read(f, nil, false); got.Pending || got.Count == nil || *got.Count != wantGeneral {
				t.Fatalf("coalesced refresh included old project scope: %+v", got)
			}
		})
	}
}
