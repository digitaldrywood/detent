package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestActivityStageTiming(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	phase := func(start, finish int) tracker.NativePhase {
		p := tracker.NativePhase{Name: "implement", StartedAt: base.Add(time.Duration(start) * time.Second)}
		if finish >= 0 {
			p.FinishedAt = base.Add(time.Duration(finish) * time.Second)
		}
		return p
	}
	for _, tt := range []struct {
		name                     string
		phases                   []tracker.NativePhase
		running                  bool
		start, elapsed, duration *float64
		partial                  bool
	}{
		{name: "latest open phase", phases: []tracker.NativePhase{phase(0, 10), phase(20, -1)}, running: true, start: reportsPercent(20), elapsed: reportsPercent(80)},
		{name: "last recorded start regardless of current name", phases: []tracker.NativePhase{phase(0, 10), phase(30, 40)}, running: true, start: reportsPercent(30), elapsed: reportsPercent(70)},
		{name: "sum closed intervals without gaps or open tail", phases: []tracker.NativePhase{phase(0, 10), phase(20, 40), phase(50, -1)}, start: reportsPercent(50), duration: reportsPercent(30)},
		{name: "negative interval contributes nothing", phases: []tracker.NativePhase{phase(10, 5), phase(20, 40)}, start: reportsPercent(20), duration: reportsPercent(20)},
		{name: "missing phases", running: true, partial: true},
		{name: "no closed data", phases: []tracker.NativePhase{phase(20, -1)}, start: reportsPercent(20), partial: true},
		{name: "future start unavailable", phases: []tracker.NativePhase{phase(101, -1)}, running: true, partial: true},
		{name: "zero last start does not fall back", phases: []tracker.NativePhase{phase(0, 10), {}}, running: true, partial: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := operatortool.ActivityAttempt{Stage: "code", Phase: "different"}
			activityStageTiming(&a, tt.phases, base.Add(100*time.Second), tt.running)
			var start *float64
			if a.StageStartedAt != nil {
				seconds := a.StageStartedAt.Sub(base).Seconds()
				start = &seconds
			}
			if !reflect.DeepEqual(start, tt.start) || !reflect.DeepEqual(a.StageElapsedSeconds, tt.elapsed) || !reflect.DeepEqual(a.StageDurationSeconds, tt.duration) || a.Partial != tt.partial {
				t.Fatalf("timing=%+v start=%v", a, start)
			}
		})
	}
}

func TestActivityTypicalDurations(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	makeAttempt := func(role, model, effort, status string, seconds int) nativeAnalyticsAttempt {
		return nativeAnalyticsAttempt{Identity: agentidentity.Configured("codex", "codex", "", role, model, "openai", effort, "", base), Status: status, Phases: []tracker.NativePhase{{StartedAt: base, FinishedAt: base.Add(time.Duration(seconds) * time.Second)}}}
	}
	population := []nativeAnalyticsAttempt{
		makeAttempt("code", "a", "high", "succeeded", 60), makeAttempt("code", "b", "medium", "failed", 180), makeAttempt("code", "c", "low", "succeeded", 300),
		makeAttempt("plan", "a", "high", "succeeded", 30), makeAttempt("code", "a", "high", "running", 900), makeAttempt("code", "a", "high", "interrupted", 900), makeAttempt("code", "a", "high", "cancelled", 900), makeAttempt("merge", "a", "high", "succeeded", 0),
	}
	for _, tt := range []struct {
		name     string
		attempts []nativeAnalyticsAttempt
		partial  bool
		want     []operatortool.ActivityTypicalDuration
	}{
		{name: "combined models and efforts", attempts: population, want: []operatortool.ActivityTypicalDuration{{ProjectID: "project", Stage: "code", AnalyticsDuration: operatortool.AnalyticsDuration{Count: 3, Seconds: 540, P50Seconds: 180, P90Seconds: 276}}, {ProjectID: "project", Stage: "plan", AnalyticsDuration: operatortool.AnalyticsDuration{Count: 1, Seconds: 30, P50Seconds: 30, P90Seconds: 30}}}},
		{name: "no data", attempts: population[4:], want: []operatortool.ActivityTypicalDuration{}},
		{name: "partial sample", attempts: population[:1], partial: true, want: []operatortool.ActivityTypicalDuration{{ProjectID: "project", Stage: "code", AnalyticsDuration: operatortool.AnalyticsDuration{Count: 1, Seconds: 60, P50Seconds: 60, P90Seconds: 60}, Partial: true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := activityTypicalDurations("project", tt.attempts, tt.partial)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("typical=%+v want=%+v", got, tt.want)
			}
			combined := append([]nativeAnalyticsAttempt{}, tt.attempts...)
			for i := range combined {
				combined[i].Identity = agentidentity.Configured("codex", "codex", "", combined[i].Identity.Role, "combined", "openai", "combined", "", base)
			}
			reports, _ := reportsAttemptTotals(combined, nil)
			for _, typical := range got {
				found := false
				for _, stage := range reports {
					if stage.Stage == typical.Stage {
						found = true
						if !reflect.DeepEqual(typical.AnalyticsDuration, stage.Duration) {
							t.Fatalf("activity=%+v Reports=%+v", typical, stage)
						}
					}
				}
				if !found {
					t.Fatalf("Reports missing %s", typical.Stage)
				}
			}
		})
	}
}

func TestHostedActivityReads(t *testing.T) {
	f := newUsageHostedFixture(t)
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	f.service.config.now = func() time.Time { return now }
	db := f.service.database.db
	runtime := tracker.NativeRuntimeObservation{Phase: "implement", Identity: agentidentity.Configured("codex", "codex", "", "code", "a", "openai", "high", "", now), Phases: []tracker.NativePhase{{Name: "setup", StartedAt: now.Add(-5 * time.Minute), FinishedAt: now.Add(-4 * time.Minute)}, {Name: "implement", StartedAt: now.Add(-3 * time.Minute), FinishedAt: now.Add(-time.Minute)}}}
	raw, err := json.Marshal(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=json_set(data_json,'$.runtime',json(?)),updated_at=?", string(raw), formatHubTime(now.Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(f.project)}
	for _, suffix := range []string{"1_0", "2_0"} {
		issueScope := scope
		if suffix == "1_0" {
			issueScope.project = tracker.ProjectID(f.privateProject)
		}
		issue, err := createNativeIssueTx(t.Context(), tx, issueScope, tracker.CreateIssue{Title: "running issue " + suffix, State: "Todo"}, now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "UPDATE leases SET issue_id=(SELECT id FROM issues WHERE native_id=?) WHERE lease_id=?", issue.WorkItemID, "lease_browser_usage_"+suffix); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET work_item_id=?,project_id=? WHERE id=?", issue.WorkItemID, issueScope.project, "attempt_browser_usage_"+suffix); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE leases SET renewed_at=?,expires_at=?,released_at=NULL WHERE lease_id IN ('lease_browser_usage_0_0','lease_browser_usage_1_0')", formatHubTime(now.Add(-time.Minute)), formatHubTime(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE native_attempts SET status='running',started_at=? WHERE id IN ('attempt_browser_usage_0_0','attempt_browser_usage_1_0')", formatHubTime(now.Add(-30*24*time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE native_attempts SET status='running' WHERE id='attempt_browser_usage_2_0'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "UPDATE leases SET expires_at=?,released_at=NULL WHERE lease_id='lease_browser_usage_2_0'", formatHubTime(now.Add(-30*time.Second))); err != nil {
		t.Fatal(err)
	}

	for _, entry := range []struct{ id, status string }{{"attempt_browser_usage_0_1", "failed"}, {"attempt_browser_usage_1_1", "cancelled"}} {
		if _, err := db.ExecContext(t.Context(), "UPDATE native_attempts SET status=? WHERE id=?", entry.status, entry.id); err != nil {
			t.Fatal(err)
		}
	}
	var linkedItem string
	if err := db.QueryRowContext(t.Context(), "SELECT work_item_id FROM native_attempts WHERE id='attempt_browser_usage_0_0'").Scan(&linkedItem); err != nil {
		t.Fatal(err)
	}
	changeRaw, err := json.Marshal(tracker.ChangeRequest{ID: "change_activity", OrganizationID: scope.organization, ProjectID: scope.project, WorkItemID: tracker.NativeWorkItemID(linkedItem), CurrentVersion: "version_activity", CreatedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO change_requests(id,organization_id,project_id,work_item_id,record_json) VALUES ('change_activity',?,?,?,?)", scope.organization, scope.project, linkedItem, string(changeRaw)); err != nil {
		t.Fatal(err)
	}
	versionRaw, err := json.Marshal(tracker.ChangeVersion{ChangeVersionInput: tracker.ChangeVersionInput{External: &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://example.test/pull/1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO change_versions(id,change_id,number,record_json) VALUES ('version_activity','change_activity',1,?)", string(versionRaw)); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct{ id, state string }{{"workspace_activity", "ready"}, {"workspace_closed", "closed"}} {
		if _, err := db.ExecContext(t.Context(), "INSERT INTO workspace_sessions(id,organization_id,project_id,subject_work_item_id,attempt_id,ref,state,idle_timeout_seconds,expires_at,requested_expires_at,created_by,created_at,updated_at) VALUES (?,?,?,?,'attempt_browser_usage_0_0','develop',?,300,?,?,?,?,?)", entry.id, scope.organization, scope.project, linkedItem, entry.state, formatHubTime(now.Add(time.Hour)), formatHubTime(now.Add(time.Hour)), "user_browser_owner", formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tt := range []struct {
		name, account, query      string
		status, running, finished int
	}{
		{"all running including outside window", "owner", "limit=1", 200, 2, 1},
		{"matching project", "owner", "project_id=" + f.project + "&limit=2", 200, 1, 2},
		{"matching runner", "owner", "runner_id=runner_browser_preview&limit=2", 200, 2, 2},
		{"project grant filters running", "viewer", "limit=2", 200, 1, 2},
		{"private project denied", "viewer", "project_id=" + f.privateProject, 403, 0, 0},
		{"nonmatching runner", "viewer", "runner_id=absent", 200, 0, 0},
		{"no project grants", "invitee", "", 200, 0, 0},
		{"foreign project", "viewer", "project_id=prj_foreign", 403, 0, 0},
		{"invalid limit", "viewer", "limit=201", 422, 0, 0},
		{"invalid window", "viewer", "from=invalid", 422, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := "/api/v2/organizations/org_browser_preview/activity?" + tt.query
			response := f.page(t, tt.account, path)
			if response.Code != tt.status {
				t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
			}
			if tt.status != 200 {
				return
			}
			var got operatortool.ActivityReport
			usageDecode(t, response, &got)
			if len(got.Running) != tt.running || len(got.Finished) != tt.finished {
				t.Fatalf("activity=%+v", got)
			}
			if tt.running == 0 {
				if len(got.TypicalDurations) != 0 {
					t.Fatalf("unfiltered typical=%+v", got)
				}
				return
			}
			for _, a := range got.Running {
				if a.AttemptID == "attempt_browser_usage_0_0" && (a.ChangeID != "change_activity" || a.PullRequestURL != "https://example.test/pull/1" || !reflect.DeepEqual(a.WorkspaceIDs, []string{"workspace_activity"})) {
					t.Fatalf("links=%+v", a)
				}
				if a.Stage != "code" || a.StageStartedAt == nil || !a.StageStartedAt.Equal(now.Add(-3*time.Minute)) || a.StageElapsedSeconds == nil || *a.StageElapsedSeconds != 180 || a.RunnerID != "runner_browser_preview" || a.Title == "" || a.Number == 0 || a.SessionID == "" {
					t.Fatalf("running=%+v", a)
				}
			}
			if got.Finished[0].AttemptID != "attempt_browser_usage_2_0" || got.Finished[0].Outcome != "interrupted" || got.Finished[0].FinishedAt == nil || !got.Finished[0].FinishedAt.Equal(now.Add(-30*time.Second)) || got.Finished[0].StageDurationSeconds == nil || *got.Finished[0].StageDurationSeconds != 180 {
				t.Fatalf("finished=%+v", got.Finished)
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	request.AddCookie(f.cookies["viewer"])
	c := f.service.echo.NewContext(request, httptest.NewRecorder())
	c.SetPath("/mcp")
	authorityContexts := make(chan context.Context, 1)
	if err := f.service.operatorAuthority(func(c echo.Context) error { authorityContexts <- c.Request().Context(); return nil })(c); err != nil {
		t.Fatal(err)
	}
	if len(authorityContexts) == 0 {
		t.Fatal("missing operator context")
	}
	ctx := <-authorityContexts
	args := []byte(`{"limit":2}`)
	result, err := (hubFleetExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.Activity, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	response := f.page(t, "viewer", "/api/v2/organizations/org_browser_preview/activity?limit=2")
	if !jsonEqualActivity(result.Content, response.Body.Bytes()) {
		t.Fatalf("MCP=%s API=%s", result.Content, response.Body.String())
	}
	ctx = operatortool.BindConnection(context.WithoutCancel(ctx), "activity-test", "activity-test")
	stdio := nativeAnalyticsStdioCall(t, ctx, hostedOperatorExecutor{f.service}, operatortool.Activity)
	full := f.page(t, "viewer", "/api/v2/organizations/org_browser_preview/activity")
	if !jsonEqualActivity(stdio, full.Body.Bytes()) {
		t.Fatalf("stdio=%s API=%s", stdio, full.Body.String())
	}
	var fullReport operatortool.ActivityReport
	usageDecode(t, full, &fullReport)
	outcomes := []string{}
	for _, attempt := range fullReport.Finished {
		outcomes = append(outcomes, attempt.Outcome)
	}
	if !reflect.DeepEqual(outcomes, []string{"interrupted", "succeeded", "cancelled", "failed"}) {
		t.Fatalf("recent outcome order=%v", outcomes)
	}

	for _, id := range []string{"attempt_browser_usage_0_0", "attempt_browser_usage_1_0"} {
		var item, project string
		if err := db.QueryRowContext(t.Context(), "SELECT work_item_id,project_id FROM native_attempts WHERE id=?", id).Scan(&item, &project); err != nil {
			t.Fatal(err)
		}
		itemScope := scope
		itemScope.project = tracker.ProjectID(project)
		attempt, err := f.service.readNativeAttempt(t.Context(), itemScope, item, id)
		if err != nil {
			t.Fatal(err)
		}
		if attempt.Status != "running" || attempt.Runtime.Identity.Role != "code" || !attempt.Runtime.Phases[len(attempt.Runtime.Phases)-1].StartedAt.Equal(now.Add(-3*time.Minute)) {
			t.Fatalf("per-item=%+v", attempt)
		}
	}
	tools, err := (hubFleetExecutor{f.service}).ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range tools {
		if tool.Name == operatortool.Activity {
			found = true
		}
	}
	if !found {
		t.Fatal("activity absent from MCP catalog")
	}
}

func jsonEqualActivity(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func TestActivityPopulationCap(t *testing.T) {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	f := newHostedSecurityFixture(t, func(cfg *Config) { cfg.now = func() time.Time { return now } })
	user := f.user(t, "population", "viewer", "population@example.test", "", "")
	f.grant(t, user, false, false)
	scope := nativeScope{organization: "org_security", project: f.project}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	issue, err := createNativeIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: "population", State: "Todo"}, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var issueRow int64
	if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id=?", issue.WorkItemID).Scan(&issueRow); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id) VALUES ('population','population',1,'test',?,?,?,'org_security')", formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
		t.Fatal(err)
	}
	for i := range maxAnalyticsPopulation + 1 {
		id := fmt.Sprintf("population_%04d", i)
		stamp := formatHubTime(now.Add(-time.Minute))
		lease, err := tx.ExecContext(t.Context(), "INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES (?,?,'population',?,?,?,?,?,?,?)", id, issueRow, id, stamp, stamp, stamp, stamp, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		fence, err := lease.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Configured("codex", "codex", "", "code", "a", "openai", "high", "", now), Phases: []tracker.NativePhase{{StartedAt: now.Add(-2 * time.Minute), FinishedAt: now.Add(-time.Minute)}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), "INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) VALUES (?,?,?,?,?,?,?,1,'succeeded',?,?,?)", id, scope.organization, scope.project, issue.WorkItemID, id, fence, id, string(raw), formatHubTime(now.Add(-2*time.Minute)), stamp); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	population, err := loadNativeAnalyticsAttempts(t.Context(), tx, scope, operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now}, "")
	if err != nil {
		t.Fatal(err)
	}
	typical := activityTypicalDurations(string(f.project), population.Items, population.Partial)
	if !population.Partial || !population.Clipped || len(population.Items) != maxAnalyticsPopulation || len(typical) != 1 || !typical[0].Partial || typical[0].Count != maxAnalyticsPopulation || typical[0].P50Seconds != 60 {
		t.Fatalf("population=%d partial=%t clipped=%t typical=%+v", len(population.Items), population.Partial, population.Clipped, typical)
	}
	t.Logf("1001-attempt capped duration read: %s", time.Since(started))
	last := fmt.Sprintf("population_%04d", maxAnalyticsPopulation)
	if _, err := tx.ExecContext(t.Context(), "UPDATE leases SET released_at=NULL,renewed_at=?,expires_at=? WHERE lease_id=?", formatHubTime(now.Add(-time.Minute)), formatHubTime(now.Add(time.Hour)), last); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET status='running' WHERE id=?", last); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/org_security/activity?limit=1", nil)
	request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
	response := httptest.NewRecorder()
	started = time.Now()
	f.service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
	}
	var report operatortool.ActivityReport
	usageDecode(t, response, &report)
	if !report.Partial || len(report.TypicalDurations) != 1 || !report.TypicalDurations[0].Partial || report.TypicalDurations[0].Count != maxAnalyticsPopulation || len(report.Running) != 1 || report.Running[0].AttemptID != last || len(report.Finished) != 1 {
		t.Fatalf("capped activity=%+v", report)
	}
	t.Logf("1001-attempt complete activity HTTP read: %s", time.Since(started))

}
