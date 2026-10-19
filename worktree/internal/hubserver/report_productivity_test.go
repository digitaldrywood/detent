package hubserver

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationProductivity(t *testing.T) {
	f := newUsageHostedFixture(t)
	to := f.service.config.now().UTC().Truncate(time.Hour)
	org := tracker.OrganizationID("org_browser_preview")
	projects := []tracker.ProjectID{tracker.ProjectID(f.project), "prj_productivity_second", "prj_productivity_secret"}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, project := range projects[1:] {
		exec(`INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT ?,organization_id,?,profile,states_json,created_at FROM projects WHERE id=?`, project, string(project), f.project)
		exec(`INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) SELECT ?,source_name,detent_state,terminal,dispatchable,created_at,updated_at FROM workflow_states WHERE project_id=?`, project, f.project)
	}
	exec(`INSERT INTO hosted_project_grants SELECT user_id,organization_id,?,can_write,manage_runner FROM hosted_project_grants WHERE project_id=? ON CONFLICT DO NOTHING`, projects[1], f.project)
	exec(`DELETE FROM hosted_project_grants WHERE project_id NOT IN (?,?)`, projects[0], projects[1])
	for _, project := range projects {
		exec(`INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) SELECT ?,'Rework','Rework',0,1,?,? WHERE NOT EXISTS(SELECT 1 FROM workflow_states WHERE project_id=? AND detent_state='Rework')`, project, formatHubTime(to), formatHubTime(to), project)
	}
	type seed struct {
		name            string
		project         int
		created, closed int
		state, kind     string
		priority        int
		actor           tracker.Actor
	}
	seeds := []seed{
		{"human completed", 0, -20, -2, "Done", "bug", 1, tracker.Actor{Kind: "human", PrincipalID: "person_one"}},
		{"runner completed", 1, -30, -3, "Done", "change_request", 2, tracker.Actor{Kind: "human", PrincipalID: "person_two", Agent: "assistant"}},
		{"cancelled", 0, -18, -4, "Cancelled", "bug", 1, tracker.Actor{Kind: "integration", PrincipalID: "automation_one"}},
		{"open", 1, -10, 0, "Todo", "bug", 1, tracker.Actor{Kind: "human", PrincipalID: "person_one"}},
		{"prior done", 0, -40, -26, "Done", "bug", 1, tracker.Actor{Kind: "human", PrincipalID: "person_one"}},
		{"prior cancelled", 1, -45, -27, "Cancelled", "change_request", 2, tracker.Actor{Kind: "human", PrincipalID: "person_two"}},
		{"secret", 2, -20, -2, "Done", "bug", 1, tracker.Actor{Kind: "human", PrincipalID: "secret"}},
	}
	ids := map[string]string{}
	for _, seed := range seeds {
		scope := nativeScope{organization: org, project: projects[seed.project], sourceActor: &seed.actor}
		created := to.Add(time.Duration(seed.created) * time.Hour)
		issue, err := createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: seed.name, Body: nativeContractTestBody, State: "Todo", Type: seed.kind, Priority: new(seed.priority), Labels: []string{"productivity", seed.kind}}, created)
		if err != nil {
			t.Fatal(err)
		}
		id := string(issue.WorkItemID)
		ids[seed.name] = id
		if seed.closed != 0 {
			issue.State = "In Progress"
			if issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, created.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			if seed.name == "runner completed" {
				for _, step := range []struct {
					hour     int
					from, to string
				}{{-6, "In Progress", "Rework"}, {-5, "Rework", "In Progress"}} {
					issue.State = step.to
					if issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: step.from, ToState: step.to, ReasonDetail: "missing criterion"}, to.Add(time.Duration(step.hour)*time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
				scope.sourceActor = &tracker.Actor{Kind: "runner", PrincipalID: "runner_browser_preview"}
			}
			issue.State = seed.state
			if _, err := persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: "In Progress", ToState: seed.state}, to.Add(time.Duration(seed.closed)*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, seed := range []struct {
		name  string
		costs map[int]float64
	}{{"human completed", map[int]float64{-25: 3, -1: 4}}, {"runner completed", map[int]float64{-28: 5}}, {"prior done", map[int]float64{-28: 2, -1: 10}}} {
		id := ids[seed.name]
		project := projects[0]
		if seed.name == "runner completed" {
			project = projects[1]
		}
		lease, attempt := "lease_"+id, "attempt_"+id
		exec(`INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT ?, (SELECT id FROM issues WHERE native_id=?),machine_id,?,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at FROM leases WHERE lease_id='lease_browser_usage_0_0'`, lease, id, "session_"+id)
		exec(`INSERT INTO lease_runners(lease_id,runner_id) SELECT ?,runner_id FROM lease_runners WHERE lease_id='lease_browser_usage_0_0'`, lease)
		exec(`INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) VALUES(?,?,?,?,?,(SELECT max(fencing_token)+1 FROM native_attempts),?,1,'succeeded','{"runtime":{"identity":{"role":"code"}}}',?,?)`, attempt, org, project, id, lease, "run_"+id, formatHubTime(to.Add(-29*time.Hour)), formatHubTime(to.Add(-3*time.Hour)))
		for hour, cost := range seed.costs {
			exec(`INSERT INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,cost_estimate,updated_at) VALUES(?,?,?,?,?,?,?,?)`, attempt, org, project, to.Add(time.Duration(hour)*time.Hour).Format(usagePeriodLayout), "provider", "model", cost, formatHubTime(to))
		}
	}
	for _, name := range []string{"human completed", "runner completed", "prior done"} {
		id := ids[name]
		project := projects[0]
		hour := -2
		if name == "runner completed" {
			project = projects[1]
			hour = -3
		}
		if name == "prior done" {
			hour = -26
		}
		change, version := "change_"+id, "version_"+id
		exec(`INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES(?,?,?,?,json_object('landed',json_object('version_id',?,'head_sha',?,'merge_sha',?,'landed_at',?)))`, change, org, project, id, version, strings.Repeat("a", 40), strings.Repeat("b", 40), formatHubTime(to.Add(time.Duration(hour)*time.Hour)))
		exec(`INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,1,json_object('head_sha',?))`, version, change, strings.Repeat("a", 40))
	}
	if err := f.service.database.commit(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	drainTestNativeBoard(t, f.service)
	for _, project := range projects {
		rebuildTestAnalyticsRollups(t, f.service, nativeScope{organization: org, project: project})
	}
	read := func(params url.Values) organizationProductivity {
		t.Helper()
		params.Set("project", "all")
		params.Set("scope", "organization")
		var out organizationProductivity
		usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/reports?"+params.Encode(), http.StatusOK), &out)
		return out
	}
	for _, tt := range []struct {
		name                                                                            string
		params                                                                          url.Values
		filed, completed, cancelled, landed, priorFiled, priorCompleted, priorCancelled int
		cost                                                                            float64
		bucket                                                                          time.Duration
	}{
		{"24h", url.Values{"range": {"24h"}}, 3, 2, 1, 2, 3, 1, 1, 12, time.Hour},
		{"7d", url.Values{"range": {"7d"}}, 6, 3, 2, 3, 0, 0, 0, 24, 24 * time.Hour},
		{"30d", url.Values{"range": {"30d"}}, 6, 3, 2, 3, 0, 0, 0, 24, 24 * time.Hour},
		{"90d", url.Values{"range": {"90d"}}, 6, 3, 2, 3, 0, 0, 0, 24, 24 * time.Hour},
		{"custom", url.Values{"from": {to.AddDate(0, 0, -6).Format(time.DateOnly)}, "to": {to.Format(time.DateOnly)}}, 6, 3, 2, 3, 0, 0, 0, 24, 24 * time.Hour},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.params.Set("labels", "productivity")
			out := read(tt.params)
			if out.Current.Filed != tt.filed || out.Current.Completed != tt.completed || out.Current.Cancelled != tt.cancelled || out.Current.ChangesLanded != tt.landed || out.Current.NetBacklog != tt.filed-tt.completed-tt.cancelled || out.Current.CompletionCost != tt.cost {
				t.Fatalf("current=%+v", out.Current)
			}
			if out.Previous.Filed != tt.priorFiled || out.Previous.Completed != tt.priorCompleted || out.Previous.Cancelled != tt.priorCancelled {
				t.Fatalf("previous=%+v", out.Previous)
			}
			if len(out.Projects) != 2 || out.Window.Bucket != tt.bucket || out.PreviousWindow.To.Sub(out.PreviousWindow.From) != out.Window.To.Sub(out.Window.From) {
				t.Fatalf("projects=%d window=%+v previous=%+v", len(out.Projects), out.Window, out.PreviousWindow)
			}
			for _, row := range out.Projects {
				if row.ProjectID != string(projects[0]) && row.ProjectID != string(projects[1]) {
					t.Fatalf("unreadable project: %s", row.ProjectID)
				}
			}
			assertProductivitySums(t, out)
			if tt.name == "24h" {
				for _, row := range out.Projects {
					var project hostedReportsReport
					usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/reports?project="+row.ProjectID+"&range=24h&labels=productivity", http.StatusOK), &project)
					if row.Current.Completed != project.Completion.Done || row.Current.Cancelled != project.Completion.Cancelled || row.Current.CompletionCost != project.Completion.Cost || row.Current.Worked != project.Analytics.Quality.WorkedItems || row.Current.Reworked != project.Analytics.Quality.ReworkedItems {
						t.Fatalf("summary=%+v project completion=%+v quality=%+v", row.Current, project.Completion, project.Analytics.Quality)
					}
				}
				if out.Current.CostPerCompleted == nil || *out.Current.CostPerCompleted != 6 || out.Previous.CostPerCompleted == nil || *out.Previous.CostPerCompleted != 2 || out.Current.CycleP50 == nil || *out.Current.CycleP50 != 22.5*3600 {
					t.Fatalf("completion metrics: %+v / %+v", out.Current, out.Previous)
				}
				if out.Current.Worked != 3 || out.Current.Reworked != 1 || out.Current.ReworkRate == nil || math.Abs(*out.Current.ReworkRate-100.0/3) > 1e-9 {
					t.Fatalf("quality=%+v", out.Current)
				}
				if !slices.ContainsFunc(out.CompletedBy, func(a productivityActor) bool {
					return a.Kind == "runner" && a.ID == "runner_browser_preview" && a.Name != "" && a.Current == 1
				}) || !slices.ContainsFunc(out.CompletedBy, func(a productivityActor) bool {
					return a.Kind == "people" && a.ID == "person_one" && a.Current == 1 && a.Previous == 1
				}) {
					t.Fatalf("completed by=%+v", out.CompletedBy)
				}
				for kind, want := range map[string]int{"people": 2, "automation": 1} {
					if !slices.ContainsFunc(out.FiledBy, func(a productivityActor) bool { return a.Kind == kind && a.Current == want }) {
						t.Fatalf("filed by=%+v", out.FiledBy)
					}
				}
			}
		})
	}
	for _, tt := range []struct {
		name, key, value            string
		filed, completed, cancelled int
	}{
		{"label", "labels", "change_request", 0, 1, 0},
		{"priority", "priorities", "2", 0, 1, 0},
		{"type", "types", "change_request", 0, 1, 0},
		{"filer", "filed_by", "person_one", 2, 1, 0},
		{"runner", "runners", "runner_browser_preview", 1, 2, 0},
		{"project", "projects", string(projects[1]), 1, 1, 0},
		{"unreadable project", "projects", string(projects[2]), 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			params := url.Values{"range": {"24h"}, "labels": {"productivity"}}
			params.Set(tt.key, tt.value)
			out := read(params)
			if out.Current.Filed != tt.filed || out.Current.Completed != tt.completed || out.Current.Cancelled != tt.cancelled {
				t.Fatalf("filtered=%+v", out.Current)
			}
			assertProductivitySums(t, out)
		})
	}
	for _, tt := range []struct {
		name   string
		filter reportsFilter
	}{
		{"unfiltered", reportsFilter{}},
		{"filtered", reportsFilter{Labels: []string{"productivity"}, Priorities: []string{"1", "2"}, Types: []string{"bug", "change_request"}, FiledBy: []string{"person_one", "person_two"}, Runners: []string{"runner_browser_preview"}}},
	} {
		t.Run("rollup query path/"+tt.name, func(t *testing.T) {
			var summaryRead bool
			ctx := withRequestTiming(t.Context(), &requestTiming{trace: func(query string, _ []driver.NamedValue) {
				tables := sqlTableAliases(query)
				summaryRead = summaryRead || tables["report_issue_summaries"] != ""
				for _, table := range []string{"issues", "native_attempts", "collaboration_events", "attempt_usage", "leases", "lease_runners", "runner_identities", "change_requests", "change_versions", "change_evidence", "quality_landings"} {
					if tables[table] != "" {
						t.Errorf("organization read queried raw table %s: %s", table, query)
					}
				}
			}})
			window := operatortool.AnalyticsWindow{From: to.Add(-24 * time.Hour), To: to, Bucket: time.Hour}
			if _, err := f.service.readOrganizationProductivity(ctx, []string{string(projects[0]), string(projects[1])}, window, tt.filter); err != nil {
				t.Fatal(err)
			}
			if !summaryRead {
				t.Fatal("organization read did not query productivity summaries")
			}
		})
	}
	before := read(url.Values{"range": {"24h"}, "labels": {"productivity"}})
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json='{}'`); err != nil {
		t.Fatal(err)
	}
	after := read(url.Values{"range": {"24h"}, "labels": {"productivity"}})
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("read depends on live attempt payloads")
	}
	for _, tt := range []struct {
		name, runner, kind, displayName string
	}{
		{"resolved final runner", "runner_browser_preview_shared", "runner", "Shared runner"},
		{"unknown final runner", "", "unknown", ""},
	} {
		t.Run("completion attribution/"+tt.name, func(t *testing.T) {
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := tx.ExecContext(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"runner completed", "human completed"} {
				id := ids[name]
				lease := "lease_final_" + id
				exec(`INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT ?,issue_id,machine_id,?,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at FROM leases WHERE lease_id=?`, lease, lease, "lease_"+id)
				if tt.runner != "" {
					exec(`INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)`, lease, tt.runner)
				}
				exec(`INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) SELECT ?,organization_id,project_id,work_item_id,?,(SELECT max(fencing_token)+1 FROM native_attempts),run_id,2,'succeeded','{}',?,? FROM native_attempts WHERE id=?`, "attempt_final_"+id, lease, formatHubTime(to.Add(-4*time.Hour)), formatHubTime(to.Add(-3*time.Hour)), "attempt_"+id)
				project := projects[1]
				wantKind, wantID, wantName := tt.kind, tt.runner, tt.displayName
				if name == "human completed" {
					project = projects[0]
					wantKind, wantID, wantName = "people", "person_one", ""
				}
				if err := refreshProductivitySummary(t.Context(), tx, nativeScope{organization: org, project: project}, id); err != nil {
					t.Fatal(err)
				}
				var kind, runner, displayName string
				if err := tx.QueryRowContext(t.Context(), `SELECT completed_kind,completed_id,completed_name FROM report_issue_summaries WHERE organization_id=? AND project_id=? AND work_item_id=?`, org, project, id).Scan(&kind, &runner, &displayName); err != nil {
					t.Fatal(err)
				}
				if kind != wantKind || runner != wantID || displayName != wantName {
					t.Fatalf("%s attribution=(%q,%q,%q) want=(%q,%q,%q)", name, kind, runner, displayName, wantKind, wantID, wantName)
				}
			}
		})
	}
	for _, tt := range []struct {
		name          string
		createdHours  [2]int
		selected      string
		priorRecorded bool
	}{
		{"new projects", [2]int{-12, -12}, "", false},
		{"prior end boundary", [2]int{-24, -24}, "", false},
		{"project in prior window", [2]int{-25, -12}, "", true},
		{"second project in prior window", [2]int{-12, -25}, "", true},
		{"old project without activity", [2]int{-72, -72}, "", true},
		{"selected new project", [2]int{-72, -12}, string(projects[1]), false},
		{"unreadable old project", [2]int{-12, -12}, string(projects[2]), false},
	} {
		t.Run("prior availability/"+tt.name, func(t *testing.T) {
			for i, project := range projects {
				hours := -72
				if i < len(tt.createdHours) {
					hours = tt.createdHours[i]
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE projects SET created_at=? WHERE id=?`, formatHubTime(to.Add(time.Duration(hours)*time.Hour)), project); err != nil {
					t.Fatal(err)
				}
			}
			out := read(url.Values{"range": {"24h"}, "labels": {"no-matching-activity"}, "projects": {tt.selected}})
			if out.PriorRecorded != tt.priorRecorded || out.Previous.Filed != 0 || out.Previous.Completed != 0 {
				t.Fatalf("prior_recorded=%t want=%t previous=%+v", out.PriorRecorded, tt.priorRecorded, out.Previous)
			}
		})
	}
}

func TestProductivityRunnerIntervals(t *testing.T) {
	f := newUsageHostedFixture(t)
	now := f.service.config.now().UTC().Truncate(time.Hour)
	org := tracker.OrganizationID("org_browser_preview")
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	intervals := []struct{ from, to time.Time }{
		{now.AddDate(-10, 0, 0), now.Add(-100 * time.Hour)},
		{now.AddDate(-5, 0, 0), now.Add(-90 * time.Hour)},
		{now.Add(-2*time.Hour + 10*time.Minute), now.Add(-2*time.Hour + 20*time.Minute)},
		{now.Add(-2*time.Hour + 40*time.Minute), now.Add(-2*time.Hour + 50*time.Minute)},
	}
	for i := range 2 {
		issue, err := createNativeFixtureIssueTx(t.Context(), tx, nativeScope{organization: org, project: tracker.ProjectID(f.project)}, tracker.CreateIssue{Title: "Interval issue", Body: nativeContractTestBody, State: "Todo"}, now.Add(-2*time.Hour+30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		id := string(issue.WorkItemID)
		for j, span := range intervals {
			lease := fmt.Sprintf("lease_interval_%d_%d", i, j)
			exec(`INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT ?, (SELECT id FROM issues WHERE native_id=?),machine_id,?,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at FROM leases WHERE lease_id='lease_browser_usage_0_0'`, lease, id, lease)
			exec(`INSERT INTO lease_runners(lease_id,runner_id) SELECT ?,runner_id FROM lease_runners WHERE lease_id='lease_browser_usage_0_0'`, lease)
			exec(`INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) VALUES(?,?,?,?,?,(SELECT max(fencing_token)+1 FROM native_attempts),?,?,'succeeded','{}',?,?)`, "attempt_"+lease, org, f.project, id, lease, "run_"+id, j+1, formatHubTime(span.from), formatHubTime(span.to))
		}
		for range 2 {
			if err := refreshProductivitySummary(t.Context(), tx, nativeScope{organization: org, project: tracker.ProjectID(f.project)}, id); err != nil {
				t.Fatal(err)
			}
			var count int
			var first, last int64
			if err := tx.QueryRowContext(t.Context(), `SELECT count(*),min(started_ns),max(updated_ns) FROM report_issue_runners WHERE work_item_id=?`, id).Scan(&count, &first, &last); err != nil {
				t.Fatal(err)
			}
			if count != 3 || first != intervals[0].from.UnixNano() || last != intervals[3].to.UnixNano() {
				t.Fatalf("runner intervals: count=%d first=%d last=%d", count, first, last)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name  string
		end   time.Duration
		filed int
	}{
		{"gap", 35 * time.Minute, 0},
		{"overlap", 45 * time.Minute, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := operatortool.AnalyticsWindow{From: now.Add(-2*time.Hour + 30*time.Minute), To: now.Add(-2*time.Hour + tt.end)}
			report, err := f.service.readOrganizationProductivity(t.Context(), []string{f.project}, w, reportsFilter{Runners: []string{"runner_browser_preview"}})
			if err != nil {
				t.Fatal(err)
			}
			if report.Current.Filed != tt.filed {
				t.Fatalf("filed=%d want=%d", report.Current.Filed, tt.filed)
			}
		})
	}
}

func assertProductivitySums(t *testing.T, out organizationProductivity) {
	t.Helper()
	for _, previous := range []bool{false, true} {
		var sum productivityMetrics
		total := out.Current
		if previous {
			total = out.Previous
		}
		for _, row := range out.Projects {
			m := row.Current
			if previous {
				m = row.Previous
			}
			sum.Filed += m.Filed
			sum.Completed += m.Completed
			sum.Cancelled += m.Cancelled
			sum.ChangesLanded += m.ChangesLanded
			sum.NetBacklog += m.NetBacklog
			sum.Worked += m.Worked
			sum.Reworked += m.Reworked
		}
		if sum.Filed != total.Filed || sum.Completed != total.Completed || sum.Cancelled != total.Cancelled || sum.NetBacklog != total.NetBacklog || sum.ChangesLanded != total.ChangesLanded || sum.Worked != total.Worked || sum.Reworked != total.Reworked {
			t.Fatalf("project counts=%+v totals=%+v", sum, total)
		}
		series := out.Series
		if previous {
			series = out.PreviousSeries
		}
		var filed, completed int
		for _, bucket := range series {
			var split productivityCount
			for _, counts := range bucket.Projects {
				split.Filed += counts.Filed
				split.Completed += counts.Completed
			}
			if split.Filed != bucket.Filed || split.Completed != bucket.Completed {
				t.Fatalf("bucket split=%+v bucket=%+v", split, bucket)
			}
			filed += bucket.Filed
			completed += bucket.Completed
		}
		if filed != total.Filed || completed != total.Completed {
			t.Fatalf("series filed=%d completed=%d totals=%+v", filed, completed, total)
		}
	}
}

func BenchmarkOrganizationProductivity(b *testing.B) {
	f := benchmarkAnalyticsFixture(b, 0)
	f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
	now := f.service.config.now().UTC().Truncate(time.Hour)
	tx, err := f.service.database.db.BeginTx(b.Context(), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	projects := []string{string(f.project.ID)}
	for i := range 7 {
		project := fmt.Sprintf("summary-project-%d", i)
		if _, err := tx.ExecContext(b.Context(), `INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT ?,organization_id,?,profile,states_json,created_at FROM projects WHERE id=?`, project, project, f.project.ID); err != nil {
			b.Fatal(err)
		}
		if _, err := tx.ExecContext(b.Context(), `INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) SELECT ?,source_name,detent_state,terminal,dispatchable,created_at,updated_at FROM workflow_states WHERE project_id=?`, project, f.project.ID); err != nil {
			b.Fatal(err)
		}
		projects = append(projects, project)
	}
	for i := range 2000 {
		project := tracker.ProjectID(projects[i%len(projects)])
		issue, err := createNativeFixtureIssueTx(b.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: project}, tracker.CreateIssue{Title: fmt.Sprintf("Summary benchmark %d", i), Body: nativeContractTestBody, State: "Todo"}, now.AddDate(0, 0, -i%90))
		if err != nil {
			b.Fatalf("seed issue %d: %#v", i, err)
		}
		if _, err := tx.ExecContext(b.Context(), `INSERT INTO report_issue_summaries VALUES(?,?,?,?,?,'Done','bug','1','person','people','runner','runner','Example runner',3600)`, f.project.OrganizationID, project, issue.WorkItemID, now.AddDate(0, 0, -i%90).UnixNano(), now.Add(-time.Duration(i%720)*time.Hour).UnixNano()); err != nil {
			b.Fatal(err)
		}
		for hour := range 48 {
			if _, err := tx.ExecContext(b.Context(), `INSERT INTO report_issue_hours VALUES(?,?,?,?,1,1,0,0)`, f.project.OrganizationID, project, issue.WorkItemID, now.Add(-time.Duration(i%720+hour)*time.Hour).Unix()); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	w := operatortool.AnalyticsWindow{From: now.AddDate(0, 0, -30), To: now, Bucket: 24 * time.Hour}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := f.service.readOrganizationProductivity(b.Context(), projects, w, reportsFilter{}); err != nil {
			b.Fatal(fmt.Errorf("read summary: %w", err))
		}
	}
}

func TestOrganizationProductivityTenant(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,local,created_at) VALUES('org_productivity_foreign','Foreign',0,?)", formatHubTime(f.service.config.now())); err != nil {
		t.Fatal(err)
	}
	foreign := newNativeFixture(t, f.service, "org_productivity_foreign", "foreign")
	f.create(t, "readable")
	foreign.create(t, "foreign sentinel")
	for _, fixture := range []nativeFixture{f, foreign} {
		rebuildTestAnalyticsRollups(t, f.service, nativeScope{organization: fixture.project.OrganizationID, project: fixture.project.ID})
	}
	f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.project.OrganizationID)}
	now := f.service.config.now()
	report, err := f.service.readOrganizationProductivity(t.Context(), []string{string(f.project.ID), string(foreign.project.ID)}, operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now.Add(time.Hour)}, reportsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Current.Filed != 1 {
		t.Fatalf("foreign organization counted: %+v", report.Current)
	}
}
