package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/emailtemplate"
	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDailySummarySchedule(t *testing.T) {
	zone, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, after, want, previous string
		weekdays                    bool
		hours                       float64
	}{
		{"spring change", "2026-03-07T06:00:00-06:00", "2026-03-08T06:00:00-05:00", "2026-03-07T06:00:00-06:00", false, 23},
		{"fall change", "2026-10-31T06:00:00-05:00", "2026-11-01T06:00:00-06:00", "2026-10-31T06:00:00-05:00", false, 25},
		{"weekend", "2026-10-23T06:00:00-05:00", "2026-10-26T06:00:00-05:00", "2026-10-23T06:00:00-05:00", true, 72},
		{"weekday DST", "2026-10-30T06:00:00-05:00", "2026-11-02T06:00:00-06:00", "2026-10-30T06:00:00-05:00", true, 73},
	} {
		t.Run(tt.name, func(t *testing.T) {
			parse := func(s string) time.Time {
				at, err := time.Parse(time.RFC3339, s)
				if err != nil {
					t.Fatal(err)
				}
				return at
			}
			next := summarySchedule(parse(tt.after), 360, zone, tt.weekdays, 1)
			prev := summarySchedule(next, 360, zone, tt.weekdays, -1)
			if !next.Equal(parse(tt.want)) || !prev.Equal(parse(tt.previous)) || next.Sub(prev).Hours() != tt.hours {
				t.Fatalf("window %s..%s", prev, next)
			}
			if again := summarySchedule(next, 360, zone, tt.weekdays, 1); !again.After(next) {
				t.Fatal("same window scheduled twice")
			}

			f := newDailySummaryFixture(t, 0)
			clock := parse(tt.after)
			f.service.config.now = func() time.Time { return clock }
			value := dailySummaryPreference{Enabled: true, SendTime: "06:00", TimeZone: "America/Chicago", Days: "every_day"}
			if tt.weekdays {
				value.Days = "weekdays"
			}
			if err := f.service.database.saveDailySummaryPreference(t.Context(), f.recipient.user, f.recipient.organization, value, clock); err != nil {
				t.Fatal(err)
			}
			sent := 0
			f.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(context.Context, auth.EmailMessage) error { sent++; return nil })
			clock = next.Add(-time.Second)
			if err := f.service.deliverDailySummaries(t.Context(), f.service.config.now()); err != nil {
				t.Fatal(err)
			}
			if sent != 0 {
				t.Fatal("summary fired before scheduled time")
			}
			clock = next.Add(3 * time.Minute)
			for range 2 {
				if err := f.service.deliverDailySummaries(t.Context(), f.service.config.now()); err != nil {
					t.Fatal(err)
				}
			}
			if sent != 1 {
				t.Fatalf("DST sends=%d", sent)
			}
			var receipts int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM daily_summary_receipts WHERE window_end=?", next.Unix()).Scan(&receipts); err != nil || receipts != 1 {
				t.Fatalf("receipts=%d error=%v", receipts, err)
			}
		})
	}
	for _, tt := range []struct {
		name, send, zone, days string
		valid                  bool
	}{{"half hour", "06:30", "America/Chicago", "weekdays", true}, {"hour", "23:00", "UTC", "every_day", true}, {"minute", "06:15", "UTC", "every_day", false}, {"invalid time", "24:00", "UTC", "every_day", false}, {"zone", "06:00", "Bad/Zone", "every_day", false}, {"local zone", "06:00", "Local", "every_day", false}, {"days", "06:00", "UTC", "sunday", false}} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := (dailySummaryPreference{SendTime: tt.send, TimeZone: tt.zone, Days: tt.days}).validate()
			if (err == nil) != tt.valid {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}

func TestDailySummaryPreferenceAPI(t *testing.T) {
	f := newHostedSharedFixture(t)
	member := f.user(t, "member", "member", "member@example.test", "write", "")
	support := f.user(t, "support", "owner", "support@example.test", "write", "support@example.test")
	path := "/api/v2/organizations/org_security/daily-summary-preference"
	preference := dailySummaryPreference{Enabled: true, SendTime: "06:30", TimeZone: "America/Chicago", Days: "weekdays"}
	body, _ := json.Marshal(map[string]any{"enabled": true, "send_time": preference.SendTime, "time_zone": preference.TimeZone, "days": preference.Days})
	for _, tt := range []struct {
		name, method, body, csrf, bearer string
		user                             *hostedSecurityUser
		status                           int
	}{
		{name: "default off", method: "GET", user: &member, status: 200},
		{name: "missing csrf", method: "PUT", body: string(body), user: &member, status: 403},
		{name: "bearer refused", method: "PUT", body: string(body), user: &member, bearer: "key", status: 401},
		{name: "support refused", method: "PUT", body: string(body), user: &support, csrf: cloudassert.CSRFToken("shared-user_support", "org_security"), status: 403},
		{name: "enabled", method: "PUT", body: string(body), user: &member, csrf: cloudassert.CSRFToken("shared-user_member", "org_security"), status: 200},
		{name: "persisted", method: "GET", user: &member, status: 200},
		{name: "invalid", method: "PUT", body: `{"enabled":true,"send_time":"06:15","time_zone":"UTC","days":"every_day"}`, user: &member, csrf: cloudassert.CSRFToken("shared-user_member", "org_security"), status: 422},
		{name: "disabled", method: "PUT", body: `{"enabled":false,"send_time":"07:00","time_zone":"UTC","days":"every_day"}`, user: &member, csrf: cloudassert.CSRFToken("shared-user_member", "org_security"), status: 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := f.serve(t, hostedSharedRequest{method: tt.method, target: path, user: tt.user, body: tt.body, csrf: tt.csrf, bearer: tt.bearer})
			requireNativeStatus(t, response, tt.status)
			if tt.status != 200 {
				return
			}
			var got dailySummaryPreference
			decodeHubResponse(t, response, &got)
			switch tt.name {
			case "default off":
				if got.Enabled || got.NextSend != nil || got.Recipient != "member@example.test" {
					t.Fatalf("default=%+v", got)
				}
			case "enabled", "persisted":
				if !got.Enabled || got.SendTime != "06:30" || got.TimeZone != "America/Chicago" || got.Days != "weekdays" || got.NextSend == nil {
					t.Fatalf("preference=%+v", got)
				}
			case "disabled":
				if got.Enabled || got.NextSend != nil {
					t.Fatalf("disabled=%+v", got)
				}
			}
		})
	}
	var token string
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT token FROM daily_summary_preferences WHERE user_id=?", member.identity.Subject).Scan(&token); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.saveDailySummaryPreference(t.Context(), member.identity.Subject, "org_security", preference, time.Now()); err != nil {
		t.Fatal(err)
	}
	other := f.user(t, "other", "member", "other@example.test", "write", "")
	if err := f.service.database.saveDailySummaryPreference(t.Context(), other.identity.Subject, "org_security", preference, time.Now()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		response := f.serve(t, hostedSharedRequest{kind: cloudassert.KindService, method: http.MethodPost, target: "/internal/v1/daily-summary/unsubscribe", body: `{"token":"` + token + `"}`})
		requireNativeStatus(t, response, http.StatusNoContent)
	}
	var enabled bool
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT enabled FROM daily_summary_preferences WHERE user_id=?", other.identity.Subject).Scan(&enabled); err != nil || !enabled {
		t.Fatalf("other preference changed: %v", err)
	}
}

func TestDailySummaryPreferenceIsolation(t *testing.T) {
	cfg := Config{DatabasePath: hostedTestDatabasePath(t), GitHubDisabled: true}
	s := openTestService(t, cfg)
	now := time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)
	for index, user := range []string{"user_one", "user_two"} {
		principal := "principal_" + user
		if _, err := s.database.db.ExecContext(t.Context(), `INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at) VALUES(?,?,?,'test','operator',?,?)`, principal, principal, fmt.Sprintf("%064d", index+1), formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.database.db.ExecContext(t.Context(), `INSERT INTO hosted_members(user_id,email,membership_id,role,principal_id,created_at,updated_at) VALUES(?,?,?,'member',?,?,?)`, user, user+"@example.test", "membership_"+user, principal, formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"org_one", "org_two"} {
		if _, err := s.database.db.ExecContext(t.Context(), `INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)`, org, org, formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	for _, user := range []string{"user_one", "user_two"} {
		for _, org := range []string{"org_one", "org_two"} {
			value := dailySummaryPreference{Enabled: user == "user_one" && org == "org_one", SendTime: "06:00", TimeZone: "America/Chicago", Days: "every_day"}
			if err := s.database.saveDailySummaryPreference(t.Context(), user, org, value, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, user := range []string{"user_one", "user_two"} {
		for _, org := range []string{"org_one", "org_two"} {
			var enabled bool
			var token string
			if err := s.database.reader.QueryRowContext(t.Context(), "SELECT enabled,token FROM daily_summary_preferences WHERE user_id=? AND organization_id=?", user, org).Scan(&enabled, &token); err != nil {
				t.Fatal(err)
			}
			if enabled != (user == "user_one" && org == "org_one") || !validNativeID(token, "unsub") {
				t.Fatalf("%s/%s enabled=%v token=%s", user, org, enabled, token)
			}
		}
	}
	value := dailySummaryPreference{Enabled: true, SendTime: "06:00", TimeZone: "America/Chicago", Days: "every_day"}
	if err := s.database.saveDailySummaryPreference(t.Context(), "user_one", "org_two", value, now); err != nil {
		t.Fatal(err)
	}
	var token string
	if err := s.database.reader.QueryRowContext(t.Context(), "SELECT token FROM daily_summary_preferences WHERE user_id='user_one' AND organization_id='org_one'").Scan(&token); err != nil {
		t.Fatal(err)
	}
	hosted := &Service{database: s.database, config: Config{Hosted: &HostedConfig{OrganizationID: "org_one"}}}
	for range 2 {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/unsubscribe", strings.NewReader(`{"token":"`+token+`"}`))
		ctx := echo.New().NewContext(request, response)
		if err := hosted.unsubscribeDailySummary(ctx); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, response, http.StatusNoContent)
	}
	for _, org := range []string{"org_one", "org_two"} {
		var enabled bool
		if err := s.database.reader.QueryRowContext(t.Context(), "SELECT enabled FROM daily_summary_preferences WHERE user_id='user_one' AND organization_id=?", org).Scan(&enabled); err != nil || enabled != (org == "org_two") {
			t.Fatalf("unsubscribe affected %s: enabled=%v err=%v", org, enabled, err)
		}
	}

}

type dailySummaryFixture struct {
	hostedSharedFixture
	user      hostedSecurityUser
	recipient summaryRecipient
	ids       map[string]string
}

func newDailySummaryFixture(t *testing.T, numberOffset int) dailySummaryFixture {
	t.Helper()
	f := newHostedSharedFixture(t)
	user := f.user(t, "owner", "owner", "owner@example.test", "write", "")
	zone, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	end := time.Date(2026, 10, 27, 6, 0, 0, 0, zone).UTC()
	f.service.config.now = func() time.Time { return end }
	r := summaryRecipient{user: user.identity.Subject, organization: "org_security", email: "owner@example.test", role: "owner", minute: 360, zone: zone, end: end, token: newNativeID("unsub")}
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id=?", r.user).Scan(&r.principal); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.service.database.db.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("UPDATE organizations SET name='Example Org' WHERE id='org_security'")
	exec("UPDATE projects SET name='example-project' WHERE id=?", f.project)
	for _, state := range []string{"Blocked", "Human Review", "In Progress", "Rework"} {
		exec(`INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,?,?,0,0,?,?)`, f.project, state, state, formatHubTime(end), formatHubTime(end))
	}
	exec(`INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT 'prj_second',organization_id,'example-api',profile,states_json,created_at FROM projects WHERE id=?`, f.project)
	exec(`INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) SELECT 'prj_second',source_name,detent_state,terminal,dispatchable,created_at,updated_at FROM workflow_states WHERE project_id=?`, f.project)
	exec(`INSERT INTO hosted_project_grants(user_id,organization_id,project_id) VALUES(?,'org_security','prj_second') ON CONFLICT DO NOTHING`, r.user)
	exec(`INSERT INTO token_grants(token_id,organization_id,project_id) VALUES(?,'org_security','prj_second') ON CONFLICT DO NOTHING`, r.principal)
	exec(`INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id) VALUES('machine_summary','example-runner',1,'test',?,?,?,'org_security')`, formatHubTime(end), formatHubTime(end), formatHubTime(end))
	ids := map[string]string{}
	create := func(number int, title, state, project string, age time.Duration) string {
		t.Helper()
		id := string(f.seedIssue(t, number+numberOffset))
		ids[title] = id
		if project != string(f.project) {
			exec(`INSERT INTO issues(native_id,organization_id,project_id,number,title,body,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at,workflow_state_id) SELECT ?||'_second',organization_id,?,number,title,body,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at,(SELECT id FROM workflow_states WHERE project_id=? AND detent_state=?) FROM issues WHERE native_id=?`, id, project, project, state, id)
			exec("DELETE FROM issues WHERE native_id=?", id)
			id += "_second"
			ids[title] = id
		}
		at := end.Add(-age)
		exec(`UPDATE issues SET title=?,body='private-body-sentinel',native_created_at=?,created_at=?,actor_json=?,actor_kind='human',actor_principal_id=?,workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state=?) WHERE native_id=?`, title, formatHubTime(end.Add(-72*time.Hour)), formatHubTime(end.Add(-72*time.Hour)), `{"kind":"human","principal_id":"`+r.principal+`"}`, r.principal, project, state, id)
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := appendNativeHistory(t.Context(), tx, nativeScope{organization: "org_security", project: tracker.ProjectID(project)}, id, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: state}, at); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return id
	}
	attempt := func(id, project string, index int, cost float64, human bool) {
		t.Helper()
		lease := fmt.Sprintf("lease_%s_%d", id, index)
		exec(`INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT ?,id,'machine_summary',?,?,?,?,?,?,? FROM issues WHERE native_id=?`, lease, "session_"+lease, formatHubTime(end), formatHubTime(end.Add(-3*time.Hour)), formatHubTime(end), formatHubTime(end), formatHubTime(end), formatHubTime(end), id)
		exec(`INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at,disposition_status,disposition_human_action) SELECT ?,'org_security',?,?,?,fencing_token,?,?,'succeeded','{}',?,?,?,? FROM leases WHERE lease_id=?`, "attempt_"+lease, project, id, lease, "run_"+lease, index+1, formatHubTime(end.Add(-3*time.Hour)), formatHubTime(end.Add(-time.Hour)), map[bool]string{true: "blocked", false: "complete"}[human], human, lease)
		exec(`INSERT INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,input,output,cost_estimate,updated_at) VALUES(?,'org_security',?,?,'provider','model',1000,200,?,?)`, "attempt_"+lease, project, end.Add(-time.Hour).Format(usagePeriodLayout), cost, formatHubTime(end))
	}
	for i := 1; i <= 14; i++ {
		project := string(f.project)
		if i > 8 {
			project = "prj_second"
		}
		id := create(i, fmt.Sprintf("Shipped issue %02d", i), "Done", project, time.Duration(i)*time.Minute)
		attempt(id, project, 0, 10, false)
	}
	blocked := create(15, "Answer needed", "Blocked", string(f.project), 9*time.Hour)
	attempt(blocked, string(f.project), 0, 2, true)
	create(16, "Review needed", "Human Review", string(f.project), 14*time.Hour)
	verify := create(17, "Verify your fix", "Done", string(f.project), 30*time.Hour)
	exec("UPDATE issues SET verification_state='pending' WHERE native_id=?", verify)
	active := create(18, "Long running", "In Progress", string(f.project), 31*time.Hour)
	for i := range 4 {
		attempt(active, string(f.project), i, 10, false)
	}
	rework := create(19, "Repeated rework", "Rework", string(f.project), time.Hour)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 2 {
		if err := appendNativeHistory(t.Context(), tx, nativeScope{organization: "org_security", project: f.project}, rework, "workflow.transitioned", tracker.CollaborationData{FromState: "In Progress", ToState: "Rework"}, end.Add(time.Duration(-3+i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		create(20+i, fmt.Sprintf("Watch extra %d", i), "In Progress", string(f.project), time.Duration(26+i)*time.Hour)
	}
	exec(`INSERT INTO health_findings(id,organization_id,fingerprint,signal,class,subject_json,projects_json,opened_at,last_seen_at,severity,summary,next_action,evidence_json) VALUES('finding_summary','org_security','summary-health','Runner offline','capacity','{"kind":"runner","id":"machine_summary"}','["prj_security"]',?,?,'attention','No heartbeat for 6h · 2 queued issues waiting for capacity','Check runner','{}')`, formatHubTime(end.Add(-6*time.Hour)), formatHubTime(end))
	exec(`INSERT INTO native_comments(id,organization_id,project_id,work_item_id,revision,sequence,body,actor_json,created_at,updated_at) VALUES(?,'org_security',?,?,1,2,'private-comment-text-sentinel','{"kind":"human","principal_id":"fixture"}',?,?)`, newNativeID("cmt"), f.project, blocked, formatHubTime(end.Add(-time.Hour)), formatHubTime(end.Add(-time.Hour)))
	drainTestNativeClosedTimes(t, f.service)
	drainTestNativeBoard(t, f.service)
	return dailySummaryFixture{f, user, r, ids}
}

func TestDailySummaryContentAndAccess(t *testing.T) {
	f := newDailySummaryFixture(t, 0)
	for _, tt := range []struct {
		name         string
		hidden, cost bool
	}{{"complete organization", false, true}, {"one project", true, true}, {"without cost", false, false}} {
		t.Run(tt.name, func(t *testing.T) {
			r := f.recipient
			if !tt.cost {
				r.role = "reporter"
			}
			if tt.hidden {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id='prj_second'", r.user); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO hosted_project_grants(user_id,organization_id,project_id) VALUES(?,'org_security','prj_second')", r.user); err != nil {
						t.Error(err)
					}
				}()
			}
			data, quiet, err := f.service.buildDailySummary(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if quiet {
				t.Fatal("nonempty fixture was quiet")
			}
			message, err := emailtemplate.RenderDailySummary(data)
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "complete organization" {
				var spend, shippedSpend, notShipped float64
				var tokens int64
				for _, projectID := range []string{string(f.project), "prj_second"} {
					scope := nativeScope{organization: "org_security", project: tracker.ProjectID(projectID)}
					w := operatortool.AnalyticsWindow{From: r.end.Add(-24 * time.Hour), To: r.end, Bucket: time.Hour}
					project, err := readNativeAnalytics(t.Context(), f.service.database.reader, scope, operatortool.AnalyticsRequest{Limit: hubanalytics.MaxPopulation}, w)
					if err != nil {
						t.Fatal(err)
					}
					report, err := f.service.readHostedReportsFrom(t.Context(), nativeAnalyticsReport{OrganizationID: r.organization, Window: w, Projects: []hubanalytics.Project{project}}, f.service.database.reader)
					if err != nil {
						t.Fatal(err)
					}
					spend += report.Cost
					tokens += report.Tokens
					shippedSpend += report.Completion.Cost
					for _, bucket := range report.Series {
						notShipped += bucket.NotShippedCost
					}
				}
				if data.Tiles[1].Value != summaryMoney(spend) || data.Tiles[2].Value != summaryMoney(shippedSpend/14) || data.Tiles[2].Detail != summaryTokens(tokens)+" tokens total" || data.Tiles[3].Value != summaryMoney(notShipped) {
					t.Fatalf("email and Cost report disagree: %v, report=%v,%v,%v,%d", data.Tiles, spend, shippedSpend, notShipped, tokens)
				}
				link, err := url.Parse(data.MoreURL)
				if err != nil {
					t.Fatal(err)
				}
				params := link.Query()
				params.Set("archived", "all")
				filter, args, err := nativeBoardFilter(nativeScope{organization: "org_security", project: f.project}, params, true)
				if err != nil {
					t.Fatal(err)
				}
				var count int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_items b WHERE "+filter, args...).Scan(&count); err != nil || count != 8 {
					t.Fatalf("linked completion window count=%d error=%v", count, err)
				}

				first, second := f.ids["Shipped issue 01"], f.ids["Shipped issue 02"]
				original := map[string]string{}
				for _, id := range []string{first, second} {
					var at string
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT closed_at FROM native_board_items WHERE work_item_id=?", id).Scan(&at); err != nil {
						t.Fatal(err)
					}
					original[id] = at
				}
				defer func() {
					for id, at := range original {
						if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_items SET closed_at=? WHERE work_item_id=?", at, id); err != nil {
							t.Error(err)
						}
					}
				}()
				for _, boundary := range []struct {
					name          string
					first, second time.Time
					want          int
				}{
					{"inclusive start and exclusive end", r.end.Add(-24 * time.Hour), r.end, 7},
					{"before start", r.end.Add(-24*time.Hour - time.Nanosecond), r.end, 6},
					{"before end", r.end.Add(-24*time.Hour - time.Nanosecond), r.end.Add(-time.Nanosecond), 7},
				} {
					t.Run(boundary.name, func(t *testing.T) {
						for id, at := range map[string]time.Time{first: boundary.first, second: boundary.second} {
							if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_board_items SET closed_at=? WHERE work_item_id=?", nativeBoardTime(at), id); err != nil {
								t.Fatal(err)
							}
						}
						if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM native_board_items b WHERE "+filter, args...).Scan(&count); err != nil || count != boundary.want {
							t.Fatalf("completion boundary count=%d want=%d err=%v", count, boundary.want, err)
						}
					})
				}

				if message.Subject != "Example Org · Tue Oct 27: 14 shipped, 4 need you, $182 spent" {
					t.Fatalf("subject=%s", message.Subject)
				}
				if data.ShippedCount != 14 || data.More != 9 || len(data.Watch) != 5 || len(data.Shipped) != 5 || len(data.Needs) != 4 {
					t.Fatalf("counts=%+v", data)
				}
				if !strings.Contains(data.Needs[0].Title, "Answer needed") || !strings.Contains(data.Needs[1].Title, "Verify your fix") || !strings.Contains(data.Needs[2].Title, "Review needed") || !strings.Contains(data.Needs[3].Title, "Runner offline") {
					t.Fatalf("needs order=%+v", data.Needs)
				}
				for _, part := range []string{message.HTMLBody, message.Body} {
					for _, want := range []string{"Last 24 hours, 6:00 AM Mon Oct 26 to 6:00 AM Tue Oct 27 (America/Chicago).", "Long running", "31h", "4 attempts", "$40.00 so far", "Repeated rework", "3 times", "and 9 more", "$182.00", "$10.00", "22.8K", "$42.00", "23% of spend", "Shipped issue 01"} {
						if !strings.Contains(part, want) {
							t.Errorf("part missing %q", want)
						}
					}
					for _, section := range []string{"Needs you", "Watch", "Shipped", "Spend by project"} {
						if !strings.Contains(part, section) {
							t.Errorf("section missing %q", section)
						}
					}
				}
				for i, row := range data.Shipped {
					if !strings.Contains(row.Title, fmt.Sprintf("Shipped issue %02d", i+1)) {
						t.Fatalf("shipped ordering=%+v", data.Shipped)
					}
				}
				if dir := os.Getenv("DETENT_SUMMARY_RENDER_DIR"); dir != "" {
					if err := os.WriteFile(filepath.Join(dir, "daily-summary.html"), []byte(message.HTMLBody), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, part := range []string{message.HTMLBody, message.Body} {
				for _, private := range []string{"private-body-sentinel", "private-comment-text-sentinel"} {
					if strings.Contains(part, private) {
						t.Fatal("private content escaped into email")
					}
				}
				if !tt.cost {
					for _, label := range []string{"Spend by project", "Did not ship", "tokens total", "Cost report", "$"} {
						if strings.Contains(part, label) {
							t.Errorf("cost hidden but found %q", label)
						}
					}
				}
				if tt.hidden && strings.Contains(part, "example-api") {
					t.Fatal("hidden project leaked")
				}
			}
			if tt.hidden && data.ShippedCount != 8 {
				t.Fatalf("hidden shipped=%d", data.ShippedCount)
			}
		})
	}
	for _, tt := range []struct {
		name, state  string
		age          time.Duration
		needs, watch int
	}{
		{"young work cannot hide a human action", "In Progress", time.Hour, 1, 0},
		{"watch cannot hide a human action", "In Progress", 30 * time.Hour, 1, 5},
		{"human actions precede review and preserve watch", "Human Review", 14 * time.Hour, 100, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newDailySummaryFixture(t, 1000)
			r := f.recipient
			r.end = r.end.AddDate(0, 0, 20)
			f.service.config.now = func() time.Time { return r.end }
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err := appendNativeHistory(t.Context(), tx, nativeScope{organization: "org_security", project: f.project}, f.ids["Answer needed"], "workflow.transitioned", tracker.CollaborationData{FromState: "In Progress", ToState: "Blocked"}, r.end.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			keepWatch := tt.watch > 0
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET archived=CASE WHEN native_id=? OR (? AND native_id=?) THEN 0 ELSE 1 END`, f.ids["Answer needed"], keepWatch, f.ids["Long running"]); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE health_findings SET resolved_at=?", formatHubTime(r.end)); err != nil {
				t.Fatal(err)
			}
			for i := range 100 {
				id := f.seedIssue(t, i+1)
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET title=?,native_created_at=?,workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state=?) WHERE native_id=?`, fmt.Sprintf("Attention filler %03d", i), formatHubTime(r.end.Add(-tt.age-time.Duration(i)*time.Minute)), f.project, tt.state, id); err != nil {
					t.Fatal(err)
				}
			}
			data, quiet, err := f.service.buildDailySummary(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			if quiet || data.ShippedCount != 0 || data.Tiles[1].Value != "$0.00" {
				t.Fatalf("attention-only window: quiet=%v shipped=%d tiles=%+v", quiet, data.ShippedCount, data.Tiles)
			}
			if len(data.Needs) != tt.needs || !strings.Contains(data.Needs[0].Title, "Answer needed") || len(data.Watch) != tt.watch {
				t.Fatalf("attention selection: needs=%d want=%d watch=%d want=%d", len(data.Needs), tt.needs, len(data.Watch), tt.watch)
			}
			if keepWatch && !strings.Contains(data.Watch[0].Title, "Long running") {
				t.Fatalf("high-attempt watch was lost beyond candidate bound: %+v", data.Watch)
			}
			if tt.needs == 100 && (!strings.Contains(data.Needs[1].Title, "Attention filler 099") || !strings.Contains(data.Needs[99].Title, "Attention filler 001")) {
				t.Fatalf("review wait ordering: first=%+v last=%+v", data.Needs[1], data.Needs[99])
			}
			if tt.watch == 5 && !strings.Contains(data.Watch[1].Title, "Attention filler 099") {
				t.Fatalf("watch wait ordering: %+v", data.Watch)
			}
			message, err := emailtemplate.RenderDailySummary(data)
			if err != nil {
				t.Fatal(err)
			}
			for _, part := range []string{message.HTMLBody, message.Body} {
				if !strings.Contains(part, "Answer needed") {
					t.Fatal("human action missing from rendered attention-only summary")
				}
			}
		})
	}
}

func TestDailySummaryDelivery(t *testing.T) {
	for _, tt := range []struct {
		name, outcome string
		quiet         bool
		want          int
	}{{"committed", "committed", false, 1}, {"restart after sent", "committed", false, 1}, {"definite rejection retries", "rejected", false, 2}, {"uncertain never retries", "uncertain", false, 1}, {"quiet window", "committed", true, 0}, {"restart after claim", "claimed", false, 0}} {
		t.Run(tt.name, func(t *testing.T) {
			f := newDailySummaryFixture(t, 0)
			r := f.recipient
			value := dailySummaryPreference{Enabled: true, SendTime: "06:00", TimeZone: r.zone.String(), Days: "every_day"}
			if err := f.service.database.saveDailySummaryPreference(t.Context(), r.user, r.organization, value, r.end.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if tt.quiet {
				r.end = r.end.AddDate(0, 0, -20)
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=?", r.user); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE health_findings SET resolved_at=?", formatHubTime(r.end)); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE daily_summary_preferences SET window_end=?,due_at=?", r.end.Unix(), r.end.Unix()); err != nil {
					t.Fatal(err)
				}
			}
			if tt.outcome == "claimed" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO daily_summary_receipts(user_id,organization_id,window_end,status,recorded_at) VALUES(?,?,?,'uncertain',?)", r.user, r.organization, r.end.Unix(), r.end.Unix()); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			f.service.config.Hosted.EmailSender = healthEmailSenderFunc(func(ctx context.Context, message auth.EmailMessage) error {
				calls++
				if message.To != r.email {
					t.Errorf("recipient=%s", message.To)
				}
				var status string
				if err := f.service.database.reader.QueryRowContext(ctx, "SELECT status FROM daily_summary_receipts WHERE user_id=? AND organization_id=? AND window_end=?", r.user, r.organization, r.end.Unix()).Scan(&status); err != nil || status != "uncertain" {
					t.Errorf("send without durable claim: %s %v", status, err)
				}
				if tt.outcome == "rejected" && calls == 1 {
					return &auth.EmailDeliveryError{Err: errors.New("definite rejection"), Outcome: "rejected"}
				}
				if tt.outcome == "uncertain" {
					return errors.New("ambiguous transport failure")
				}
				return nil
			})
			due := r.end.Add(3 * time.Minute)
			err := f.service.deliverDailySummaries(t.Context(), due)
			if (err != nil) != (tt.outcome == "rejected" || tt.outcome == "uncertain") {
				t.Fatalf("first delivery=%v", err)
			}
			firstCalls := calls

			if tt.name == "restart after sent" {
				sender := f.service.config.Hosted.EmailSender
				f.service.config.Hosted.EmailSender = nil
				cfg := f.service.config
				if err := f.service.Close(); err != nil {
					t.Fatal(err)
				}
				f.service = openTestService(t, cfg)
				f.service.config.Hosted.EmailSender = sender
			}
			if err := f.service.deliverDailySummaries(t.Context(), due); err != nil {
				t.Fatal(err)
			}
			if calls != firstCalls {
				t.Fatal("repeated tick sent again or ignored backoff")
			}
			if err := f.service.deliverDailySummaries(t.Context(), due.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if calls != tt.want {
				t.Fatalf("sends=%d want=%d", calls, tt.want)
			}
			var status string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT status FROM daily_summary_receipts WHERE user_id=? AND organization_id=? AND window_end=?", r.user, r.organization, r.end.Unix()).Scan(&status); err != nil {
				t.Fatal(err)
			}
			want := "sent"
			if tt.quiet {
				want = "quiet"
			}
			if tt.outcome == "uncertain" || tt.outcome == "claimed" {
				want = "uncertain"
			}
			if status != want {
				t.Fatalf("receipt=%s want=%s", status, want)
			}
		})
	}
}
