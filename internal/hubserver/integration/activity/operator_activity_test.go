package activity_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestHostedActivityReads(t *testing.T) {
	f := newActivityPostgresFixture(t, "org_browser_preview")
	now := f.now
	historyProject := f.createProject(t, "Historical project")
	mainIssue := f.issue(t, f.project, "Main activity")
	privateIssue := f.issue(t, f.privateProject, "Private activity")
	expiredIssue := f.issue(t, f.project, "Expired activity")
	historyIssue := f.issue(t, historyProject, "Historical activity")
	runtime := tracker.NativeRuntimeObservation{Phase: "implement", HeartbeatAt: now.Add(-time.Second), Identity: agentidentity.Configured("codex", "codex", "", "code", "a", "openai", "high", "", now), Phases: []tracker.NativePhase{{Name: "setup", StartedAt: now.Add(-5 * time.Minute), FinishedAt: now.Add(-4 * time.Minute)}, {Name: "implement", StartedAt: now.Add(-3 * time.Minute), FinishedAt: now.Add(-time.Minute)}}}
	main := f.storeAttempt(t, mainIssue, "attempt_browser_usage_0_0", "running", now.Add(-30*24*time.Hour), now.Add(-time.Minute), &runtime, true, activityAttemptOptions{finalization: &tracker.NativeFinalization{ChangeID: "change_activity", VersionID: "version_activity", Publication: &tracker.NativePRPublication{External: tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://example.test/pull/1"}}}})
	private := f.storeAttempt(t, privateIssue, "attempt_browser_usage_1_0", "running", now.Add(-30*24*time.Hour), now.Add(-time.Minute), &runtime, true)
	expiry := now.Add(-30 * time.Second)
	f.storeAttempt(t, expiredIssue, "attempt_browser_usage_2_0", "running", now.Add(-25*time.Minute), now.Add(-time.Minute), &runtime, true, activityAttemptOptions{expires: &expiry})
	for index, outcome := range []string{"failed", "cancelled", "succeeded"} {
		finished := now.Add(-time.Duration(3-index) * time.Minute)
		f.storeAttempt(t, mainIssue, fmt.Sprintf("attempt_browser_usage_%d_1", index), outcome, finished.Add(-24*time.Minute), finished, &runtime, true)
	}
	activeWorkspace := f.links(t, main)
	beforeHistory := f.page(t, "owner", "/api/v2/organizations/org_browser_preview/activity")
	browserHostedStatus(t, beforeHistory, http.StatusOK)
	for index := 0; index < 10002; index++ {
		issue := []corestore.WorkItem{mainIssue, privateIssue, historyIssue}[index%3]
		at := now.Add(-30 * 24 * time.Hour)
		phases := &tracker.NativeRuntimeObservation{Phase: "implement", Identity: runtime.Identity, Phases: []tracker.NativePhase{{Name: "implement", StartedAt: at.Add(-time.Minute), FinishedAt: at}}}
		f.storeAttempt(t, issue, fmt.Sprintf("activity_history_%05d", index), "succeeded", at.Add(-time.Minute), at, phases, false, activityAttemptOptions{fencing: int64(index + 1)})
	}
	var afterHistory *httptest.ResponseRecorder
	f.assertPlans(t, func() {
		for _, runner := range []string{"", "runner_browser_preview"} {
			query := "?runner_id=" + runner
			response := f.page(t, "owner", "/api/v2/organizations/org_browser_preview/activity"+query)
			browserHostedStatus(t, response, http.StatusOK)
		}
		afterHistory = f.page(t, "owner", "/api/v2/organizations/org_browser_preview/activity")
	})
	browserHostedStatus(t, afterHistory, http.StatusOK)
	var beforeReport, afterReport operatortool.ActivityReport
	usageDecode(t, beforeHistory, &beforeReport)
	usageDecode(t, afterHistory, &afterReport)
	if beforeReport.Partial || afterReport.Partial || len(beforeReport.FinishedIssues) != 0 || len(afterReport.FinishedIssues) != 0 || len(beforeReport.IssueTypicalDurations) != 0 || len(afterReport.IssueTypicalDurations) != 0 {
		t.Fatalf("historical issue clipping changed complete window fields: before=%+v after=%+v", beforeReport, afterReport)
	}
	if !reflect.DeepEqual(beforeReport.Running, afterReport.Running) || !reflect.DeepEqual(beforeReport.Finished, afterReport.Finished) || !reflect.DeepEqual(beforeReport.TypicalDurations, afterReport.TypicalDurations) || !reflect.DeepEqual(beforeReport.FinishedIssues, afterReport.FinishedIssues) || !reflect.DeepEqual(beforeReport.IssueTypicalDurations, afterReport.IssueTypicalDurations) || !reflect.DeepEqual(beforeReport.ChangeSequences, afterReport.ChangeSequences) || beforeReport.OrganizationID != afterReport.OrganizationID || !beforeReport.ObservedAt.Equal(afterReport.ObservedAt) || !reflect.DeepEqual(beforeReport.Window, afterReport.Window) || beforeReport.PopulationLimit != afterReport.PopulationLimit {
		t.Fatalf("historical attempts changed window activity: before=%s after=%s", beforeHistory.Body.String(), afterHistory.Body.String())
	}
	if beforeReport.IssuesPartial || !afterReport.IssuesPartial || len(afterReport.RunningIssues) != 2 {
		t.Fatalf("old attempt history was presented as complete: %+v", afterReport)
	}
	for _, issue := range afterReport.RunningIssues {
		if (issue.WorkItemID != string(mainIssue.NativeID) && issue.WorkItemID != string(privateIssue.NativeID)) || !issue.Partial || issue.CurrentLane != "Todo" || issue.TotalSeconds != 3600 || !issue.ClockStartedAt.Equal(now.Add(-time.Hour)) {
			t.Fatalf("running issue history clipping=%+v", issue)
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
				if a.AttemptID == "attempt_browser_usage_0_0" && (a.ChangeID != "change_activity" || a.PullRequestURL != "https://example.test/pull/1" || !reflect.DeepEqual(a.WorkspaceIDs, []string{activeWorkspace})) {
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

	for _, query := range []string{"", "?project_id=" + f.project} {
		measurements := []time.Duration{}
		for range 40 {
			started := time.Now()
			response := f.page(t, "owner", "/api/v2/organizations/org_browser_preview/activity"+query)
			if response.Code != http.StatusOK {
				t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
			}
			measurements = append(measurements, time.Since(started))
		}
		slices.Sort(measurements)
		t.Logf("10002 historical attempts across 3 projects with running and recent attempts, 40 HTTP samples, scope %q: p50=%s p95=%s", query, measurements[19], measurements[37])
	}

	for _, attempt := range []activityFixtureAttempt{main, private} {
		response := f.page(t, "owner", "/api/v2/organizations/org_browser_preview/projects/"+string(attempt.issue.ProjectID)+"/work-items/"+string(attempt.issue.NativeID)+"/attempts/"+attempt.attempt.ID)
		browserHostedStatus(t, response, http.StatusOK)
		var observed tracker.NativeAttempt
		usageDecode(t, response, &observed)
		if observed.Status != "running" || observed.Runtime == nil || observed.Runtime.Identity.Role != "code" || !observed.Runtime.Phases[len(observed.Runtime.Phases)-1].StartedAt.Equal(now.Add(-3*time.Minute)) {
			t.Fatalf("per-item=%+v", observed)
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
	for _, tt := range []struct {
		name, status, role string
		phases             any
		dropped            int
		seconds            float64
		partial            bool
	}{
		{name: "fractional duration", status: "succeeded", role: "code", phases: []tracker.NativePhase{{StartedAt: now.Add(-time.Minute), FinishedAt: now.Add(250001 * time.Microsecond)}}, seconds: 60.250001},
		{name: "updated role and outcome", status: "failed", role: "plan", phases: []tracker.NativePhase{{StartedAt: now.Add(-time.Minute), FinishedAt: now}}, seconds: 60},
		{name: "dropped intervals", status: "failed", role: "plan", phases: []tracker.NativePhase{{StartedAt: now.Add(-time.Minute), FinishedAt: now}}, dropped: 1, seconds: 60, partial: true},
		{name: "malformed phases", status: "succeeded", role: "code", phases: "malformed", partial: true},
		{name: "malformed interval", status: "succeeded", role: "code", phases: []map[string]string{{"started_at": "invalid", "finished_at": formatHubTime(now)}}, partial: true},
		{name: "cancelled excluded", status: "cancelled", role: "code", phases: []tracker.NativePhase{{StartedAt: now.Add(-time.Minute), FinishedAt: now}}},
		{name: "running excluded", status: "running", role: "code", phases: []tracker.NativePhase{{StartedAt: now.Add(-time.Minute), FinishedAt: now}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolated := newActivityPostgresFixture(t, "org_browser_preview", now)
			for index, project := range []string{isolated.project, isolated.privateProject} {
				raw, err := json.Marshal(map[string]any{"runtime": map[string]any{"identity": agentidentity.Configured("codex", "codex", "", tt.role, "a", "openai", "high", "", now), "phases": tt.phases, "phases_dropped": tt.dropped}})
				if err != nil {
					t.Fatal(err)
				}
				var historical tracker.NativeRunData
				if err := decodeActivityRun(t.Context(), nil, string(raw), &historical); err != nil {
					t.Fatal(err)
				}
				issue := isolated.issue(t, project, "Duration shape")
				initial := &tracker.NativeRuntimeObservation{Identity: agentidentity.Configured("codex", "codex", "", "code", "a", "openai", "high", "", now), Phases: []tracker.NativePhase{{StartedAt: now.Add(-time.Minute), FinishedAt: now}}}
				attempt := isolated.storeAttempt(t, issue, fmt.Sprintf("duration_shape_%d", index), "running", now.Add(-time.Minute), now.Add(-time.Second), initial, true)
				before := isolated.page(t, "owner", "/api/v2/organizations/org_browser_preview/activity?project_id="+project)
				browserHostedStatus(t, before, http.StatusOK)
				var initialReport operatortool.ActivityReport
				usageDecode(t, before, &initialReport)
				if len(initialReport.Running) != 1 || initialReport.Running[0].Stage != "code" || len(initialReport.TypicalDurations) != 0 {
					t.Fatalf("initial duration owner=%+v", initialReport)
				}
				isolated.advanceAttempt(t, attempt, tt.status, historical.Runtime, now.Add(-time.Second))
				response := isolated.page(t, "owner", "/api/v2/organizations/org_browser_preview/activity?project_id="+project+"&runner_id=runner_browser_preview&from="+formatHubTime(now.Add(-2*time.Second))+"&to="+formatHubTime(now))
				browserHostedStatus(t, response, http.StatusOK)
				var report operatortool.ActivityReport
				usageDecode(t, response, &report)
				if report.Partial != tt.partial {
					t.Fatalf("partial=%t want=%t", report.Partial, tt.partial)
				}
				if tt.seconds == 0 {
					if len(report.TypicalDurations) != 0 {
						t.Fatalf("excluded duration=%+v", report.TypicalDurations)
					}
					continue
				}
				if len(report.TypicalDurations) != 1 || report.TypicalDurations[0].ProjectID != project || report.TypicalDurations[0].Stage != tt.role || report.TypicalDurations[0].Count != 1 || report.TypicalDurations[0].Partial != tt.partial || math.Abs(report.TypicalDurations[0].P50Seconds-tt.seconds) > 1e-9 || math.Abs(report.TypicalDurations[0].P90Seconds-tt.seconds) > 1e-9 {
					t.Fatalf("project duration=%+v want=%+v", report.TypicalDurations, tt)
				}
			}
		})
	}
}

func jsonEqualActivity(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

func TestActivityPopulationCap(t *testing.T) {
	t.Run("10001 terminal attempts clip without a running attempt", func(t *testing.T) {
		terminal, _ := activityPopulationFixture(t, false)
		response := terminal.page(t, "viewer", "/api/v2/organizations/org_security/activity?limit=1")
		browserHostedStatus(t, response, http.StatusOK)
		var report operatortool.ActivityReport
		usageDecode(t, response, &report)
		if !report.Partial || len(report.TypicalDurations) != 1 || !report.TypicalDurations[0].Partial || report.TypicalDurations[0].Count != 10000 || report.TypicalDurations[0].P50Seconds != 60 || len(report.Running) != 0 {
			t.Fatalf("terminal-only clipped activity=%+v", report)
		}
	})
	f, live := activityPopulationFixture(t, true)
	started := time.Now()
	var response *httptest.ResponseRecorder
	f.assertPlans(t, func() { response = f.page(t, "viewer", "/api/v2/organizations/org_security/activity?limit=1") })
	browserHostedStatus(t, response, http.StatusOK)
	var report operatortool.ActivityReport
	usageDecode(t, response, &report)
	if !report.Partial || len(report.TypicalDurations) != 1 || !report.TypicalDurations[0].Partial || report.TypicalDurations[0].Count != 10000 || report.TypicalDurations[0].P50Seconds != 60 || len(report.Running) != 1 || report.Running[0].AttemptID != live.attempt.ID || len(report.Finished) != 1 {
		t.Fatalf("capped activity=%+v", report)
	}
	t.Logf("11002-attempt complete activity HTTP read: %s", time.Since(started))
	request := httptest.NewRequest(http.MethodGet, "/api/v2/organizations/org_security/activity?limit=1", nil)
	request.AddCookie(f.cookies["viewer"])
	c := f.service.echo.NewContext(request, httptest.NewRecorder())
	c.SetPath("/mcp")
	authorityContexts := make(chan context.Context, 1)
	if err := f.service.operatorAuthority(func(c echo.Context) error { authorityContexts <- c.Request().Context(); return nil })(c); err != nil {
		t.Fatal(err)
	}
	if len(authorityContexts) == 0 {
		t.Fatal("missing activity authority")
	}
	authority := <-authorityContexts
	writer, err := f.admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := writer.Rollback(); err != nil && err != sql.ErrTxDone {
			t.Error(err)
		}
	}()
	if _, err := writer.ExecContext(t.Context(), "SELECT id FROM issues WHERE organization_id=$1 AND id=$2 FOR UPDATE", f.organization, live.issue.ID); err != nil {
		t.Fatal(err)
	}
	result, err := f.service.executeActivityRead(testenv.WaitContext(t, authority), operatortool.Call{Arguments: []byte(`{"limit":1}`)})
	if err != nil {
		t.Fatalf("activity read used the held writer: %v", err)
	}
	if !jsonEqualActivity(result.Content, response.Body.Bytes()) {
		t.Fatalf("reader report changed: %s", result.Content)
	}
}

func activityPopulationFixture(t *testing.T, running bool) (*activityPostgresFixture, activityFixtureAttempt) {
	t.Helper()
	f := newActivityPostgresFixture(t, "org_security")
	now := f.now
	issue := f.issue(t, f.project, "population")
	var live activityFixtureAttempt
	for index := 0; index < 11002; index++ {
		id := fmt.Sprintf("population_%04d", index)
		at, start := now.Add(-time.Minute), now.Add(-2*time.Minute)
		if index > 10000 {
			at, start = now.Add(-30*24*time.Hour), now.Add(-31*24*time.Hour)
		}
		status := "succeeded"
		if running && index == 10000 {
			status = "running"
		}
		runtime := &tracker.NativeRuntimeObservation{Identity: agentidentity.Configured("codex", "codex", "", "code", "a", "openai", "high", "", now), Phases: []tracker.NativePhase{{StartedAt: now.Add(-2 * time.Minute), FinishedAt: now.Add(-time.Minute)}}}
		attempt := f.storeAttempt(t, issue, id, status, start, at, runtime, index <= 10000)
		if status == "running" {
			live = attempt
		}
	}
	f.copyOldHistory(t, "population_10001", 150000)
	return f, live
}

func (f *activityPostgresFixture) copyOldHistory(t *testing.T, attempt string, copies int) {
	t.Helper()
	tx, err := f.admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
			t.Error(err)
		}
	}()
	var fence int64
	if err := tx.QueryRowContext(t.Context(), "SELECT coalesce(max(fencing_token),0) FROM leases").Scan(&fence); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"CREATE TEMP TABLE old_leases ON COMMIT DROP AS SELECT g, l.* FROM native_attempts a JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id, generate_series(1,$3::int) g WHERE a.organization_id=$1 AND a.id=$2",
		"UPDATE old_leases SET lease_id=lease_id||'_old_'||g, session_id=session_id||'_old_'||g, fencing_token=$1::bigint+g",
		"CREATE TEMP TABLE old_runs ON COMMIT DROP AS SELECT g, r.* FROM native_attempts a JOIN native_attempt_runs r ON r.organization_id=a.organization_id AND r.run_id=a.run_id, generate_series(1,$3::int) g WHERE a.organization_id=$1 AND a.id=$2",
		"UPDATE old_runs SET run_id=run_id||'_old_'||g",
		"CREATE TEMP TABLE old_attempts ON COMMIT DROP AS SELECT g, a.* FROM native_attempts a, generate_series(1,$3::int) g WHERE a.organization_id=$1 AND a.id=$2",
		"UPDATE old_attempts SET id=id||'_old_'||g, lease_id=lease_id||'_old_'||g, run_id=run_id||'_old_'||g, fencing_token=$1::bigint+g, updated_at=updated_at-g*interval '1 minute', started_at=started_at-g*interval '1 minute'",
		"UPDATE old_leases SET acquired_at=acquired_at-g*interval '1 minute', renewed_at=renewed_at-g*interval '1 minute'",
		"ALTER TABLE old_leases DROP COLUMN g",
		"ALTER TABLE old_runs DROP COLUMN g",
		"ALTER TABLE old_attempts DROP COLUMN g",
		"INSERT INTO leases OVERRIDING SYSTEM VALUE SELECT * FROM old_leases",
		"INSERT INTO native_attempt_runs SELECT * FROM old_runs",
		"INSERT INTO native_attempts SELECT * FROM old_attempts",
	} {
		var args []any
		switch {
		case strings.Contains(statement, "$3"):
			args = []any{string(f.organization), attempt, copies}
		case strings.Contains(statement, "$1"):
			args = []any{fence}
		}
		if _, err := tx.ExecContext(t.Context(), statement, args...); err != nil {
			t.Fatalf("copy old history: %v", err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	f.fence = fence + int64(copies) + 20000
}

type activityPostgresFixture struct {
	*browserHostedFixture
	admin        *sql.DB
	repository   *postgres.Repository
	organization tracker.OrganizationID
	now          time.Time
	fence        int64
	number       int64
}

type activityFixtureAttempt struct {
	attempt leasestore.Attempt
	lease   leasestore.Lease
	issue   corestore.WorkItem
	data    tracker.NativeRunData
}

type activityAttemptOptions struct {
	expires      *time.Time
	finalization *tracker.NativeFinalization
	fencing      int64
}

func newActivityPostgresFixture(t *testing.T, organization tracker.OrganizationID, clocks ...time.Time) *activityPostgresFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if len(clocks) > 0 {
		now = clocks[0].UTC().Truncate(time.Microsecond)
	}
	browser := newPostgresBrowserHostedFixtureServing(t, true, string(organization), false, func(cfg *Config) { cfg.Clock = func() time.Time { return now } })
	f := &activityPostgresFixture{browserHostedFixture: browser, admin: hostedPostgresAdmin(t, browser.service), repository: browser.service.database.postgres, organization: organization, now: now}
	expires := now.Add(24 * time.Hour)
	const runner, machine, credential, enrollment = "runner_browser_preview", "machine_browser_preview", "activity_credential", "activity_enrollment"
	operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Heartbeat}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		hash := apikey.HashToken("activity-fixture-token")
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: credential, Name: "Activity runner", Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: enrollment, RunnerID: runner, MachineID: machine, TokenHash: apikey.HashToken("activity-fixture-enrollment"), Scope: "organization", Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "user_browser_owner"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollment, runner, machine, now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machine, Hostname: "example", Capacity: 64, Version: "example"}); err != nil {
			return err
		}
		return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runner, MachineID: machine, TokenID: credential, EnrollmentID: enrollment, Scope: "organization", State: "active", Operations: operations, CapacityLimit: 64, ReportedCapacity: 64, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.QueryRowContext(t.Context(), "SELECT coalesce(max(fencing_token),0) FROM leases").Scan(&f.fence); err != nil {
		t.Fatal(err)
	}
	f.fence += 20000
	f.number = 1000
	return f
}

func (f *activityPostgresFixture) issue(t *testing.T, project, title string) corestore.WorkItem {
	t.Helper()
	f.number++
	state, actorKind, actorID := "Todo", "human", "user_browser_owner"
	created := f.now.Add(-time.Hour)
	var item corestore.WorkItem
	content := eventstore.NewContent(f.service.runDetailStore())
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		item, err = w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: f.organization, ProjectID: tracker.ProjectID(project), NativeID: tracker.NativeWorkItemID(newNativeID("activity_item")), Number: f.number, Title: title, WorkflowStateName: &state, ActorKind: &actorKind, ActorPrincipalID: &actorID, CreatedAt: created, UpdatedAt: created, NativeCreatedAt: created, NativeUpdatedAt: created})
		if err != nil {
			return err
		}
		issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: f.organization, ProjectID: item.ProjectID, WorkItemID: item.NativeID, Number: int(item.Number), Revision: tracker.Revision(item.Revision), Profile: "native"}, Title: title, Body: nativeContractTestBody, State: "Todo", CreatedAt: created, UpdatedAt: created, Actor: tracker.Actor{Kind: actorKind, PrincipalID: actorID}}
		raw, err := json.Marshal(issue)
		if err != nil {
			return err
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: f.organization, ProjectID: item.ProjectID, WorkItemID: item.NativeID, RecordID: string(item.NativeID), Revision: item.Revision}, raw)
		if err != nil {
			return err
		}
		if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
			return err
		}
		raw, err = json.Marshal(tracker.CollaborationEvent{ID: newNativeID("activity_created"), OrganizationID: f.organization, ProjectID: item.ProjectID, AggregateID: item.NativeID, Type: "issue.created", SchemaVersion: 1, Actor: issue.Actor, RecordedAt: created})
		if err != nil {
			return err
		}
		event, err := content.PrepareEvent(t.Context(), f.organization, raw)
		if err != nil {
			return err
		}
		_, err = w.AppendEvent(t.Context(), event)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func (f *activityPostgresFixture) receipt(t *testing.T, operation, key string, value any) eventstore.Receipt {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := eventstore.NewContent(f.service.runDetailStore()).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.organization, ActorID: "production-import", Operation: operation, CommandKey: key, RequestHash: artifact.Digest(raw), CreatedAt: f.now}, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

func (f *activityPostgresFixture) storeAttempt(t *testing.T, issue corestore.WorkItem, id, status string, start, at time.Time, runtime *tracker.NativeRuntimeObservation, withEvent bool, options ...activityAttemptOptions) activityFixtureAttempt {
	t.Helper()
	f.fence++
	fence := f.fence
	var option activityAttemptOptions
	if len(options) > 0 {
		option = options[0]
	}
	if option.fencing > 0 {
		fence = option.fencing
	}
	end := at
	lease := leasestore.Lease{OrganizationID: f.organization, LeaseID: "lease_" + id, FencingToken: fence, IssueID: issue.ID, MachineID: "machine_browser_preview", SessionID: "session_" + id, WorkItemRevision: issue.Revision, AcquiredAt: start, RenewedAt: start, ExpiresAt: at.Add(time.Hour), ReleasedAt: &end}
	if status == "running" {
		lease.ReleasedAt, lease.RenewedAt, lease.ExpiresAt = nil, f.now.Add(-time.Minute), f.now.Add(time.Hour)
	}
	if option.expires != nil {
		lease.ExpiresAt = *option.expires
	}
	if err := f.repository.ImportCoreLease(t.Context(), postgres.CoreLeaseImport{Lease: lease, Receipt: f.receipt(t, "core-lease", lease.LeaseID, lease)}); err != nil {
		t.Fatal(err)
	}
	assignment := runnerstore.LeaseAssignment{OrganizationID: f.organization, LeaseID: lease.LeaseID, RunnerID: "runner_browser_preview", AssignedAt: start}
	if err := f.repository.ImportCoreRunnerLeaseAssignment(t.Context(), postgres.CoreRunnerLeaseAssignmentImport{Assignment: assignment, Receipt: f.receipt(t, "core-runner-lease-assignment", lease.LeaseID, assignment)}); err != nil {
		t.Fatal(err)
	}
	data := tracker.NativeRunData{AttemptID: id, RunID: id, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), MachineID: "machine_browser_preview", RunnerID: "runner_browser_preview", Sequence: 1, Outcome: status, Runtime: runtime, Finalization: option.finalization}
	if status == "running" {
		data.Outcome = ""
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(f.organization), raw)
	if err != nil {
		t.Fatal(err)
	}
	attempt := leasestore.Attempt{OrganizationID: f.organization, ID: id, IssueID: issue.ID, LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, RunID: id, Sequence: 1, Status: status, WorkItemRevision: issue.Revision, PayloadObjectKey: ref.Key, StartedAt: start, UpdatedAt: at}
	if err := f.repository.ImportCoreAttempt(t.Context(), postgres.CoreAttemptImport{Attempt: attempt, Receipt: f.receipt(t, "core-attempt", id, attempt)}); err != nil {
		t.Fatal(err)
	}
	if withEvent {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Background, func(w tenantstore.Writer) error {
			_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: f.organization, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: at})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		kind := "run.finished"
		if status == "running" {
			kind = "run.observed"
		}
		raw, err := json.Marshal(tracker.CollaborationEvent{ID: "event_" + id, OrganizationID: f.organization, ProjectID: issue.ProjectID, AggregateID: issue.NativeID, Type: kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: "machine_browser_preview"}, RecordedAt: at, Data: tracker.CollaborationData{Run: &data}})
		if err != nil {
			t.Fatal(err)
		}
		event, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Background, func(w tenantstore.Writer) error {
			_, err := w.AppendEvent(t.Context(), event)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return activityFixtureAttempt{attempt: attempt, lease: lease, issue: issue, data: data}
}

func (f *activityPostgresFixture) advanceAttempt(t *testing.T, attempt activityFixtureAttempt, status string, runtime *tracker.NativeRuntimeObservation, at time.Time) {
	t.Helper()
	data := attempt.data
	data.Sequence, data.Outcome, data.Runtime = 2, status, runtime
	kind := "run.finished"
	if status == "running" {
		data.Outcome, kind = "", "run.observed"
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(f.organization), raw)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(tracker.CollaborationEvent{ID: "updated_" + attempt.attempt.ID, OrganizationID: f.organization, ProjectID: attempt.issue.ProjectID, AggregateID: attempt.issue.NativeID, Type: kind, SchemaVersion: 1, Actor: tracker.Actor{Kind: "machine", PrincipalID: "machine_browser_preview"}, RecordedAt: at, Data: tracker.CollaborationData{Run: &data}})
	if err != nil {
		t.Fatal(err)
	}
	event, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.organization, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: attempt.attempt.ID, Fence: leasestore.Fence{LeaseID: attempt.lease.LeaseID, FencingToken: attempt.lease.FencingToken}, RunID: data.RunID, Sequence: 2, Status: status, PayloadObjectKey: ref.Key}); err != nil {
			return err
		}
		_, err := w.AppendEvent(t.Context(), event)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *activityPostgresFixture) links(t *testing.T, main activityFixtureAttempt) string {
	t.Helper()
	content := eventstore.NewContent(f.service.runDetailStore())
	active, closed := "ws_"+strings.Repeat("a", 32), "ws_"+strings.Repeat("b", 32)
	if err := f.repository.WriteWorkspace(t.Context(), f.organization, corestore.Serving, func(w postgres.WorkspaceWriter) error {
		for _, id := range []string{closed, active} {
			row, err := w.CreateWorkspaceSession(t.Context(), postgres.WorkspaceCreate{OrganizationID: f.organization, ProjectID: main.issue.ProjectID, ID: id, SubjectWorkItemID: string(main.issue.NativeID), AttemptID: main.attempt.ID, Ref: "develop", IdleTimeoutSeconds: 300, ExpiresAt: f.now.Add(time.Hour), RequestedExpiresAt: f.now.Add(time.Hour), CreatedBy: "user_browser_owner", CreatedAt: f.now})
			if err != nil {
				return err
			}
			if id == active {
				if err := w.PutWorkspaceItem(t.Context(), claimstore.WorkspaceItem{OrganizationID: f.organization, ProjectID: main.issue.ProjectID, WorkItemID: main.issue.NativeID, WorkspaceID: id, CreatedAt: f.now}); err != nil {
					return err
				}
				_, err = w.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: f.organization, ProjectID: main.issue.ProjectID, ID: id, ExpectedRevision: row.Revision, State: "ready", RunnerID: "runner_browser_preview", MachineID: "machine_browser_preview", LeaseID: main.lease.LeaseID, FencingToken: main.lease.FencingToken, UpdatedAt: f.now})
				if err != nil {
					return err
				}
			} else {
				for _, state := range []string{"closing", "closed"} {
					row, err = w.UpdateWorkspaceSession(t.Context(), postgres.WorkspaceUpdate{OrganizationID: f.organization, ProjectID: main.issue.ProjectID, ID: id, ExpectedRevision: row.Revision, State: state, Reason: workspacesession.ReasonClosedByActor, UpdatedAt: f.now})
					if err != nil {
						return err
					}
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.organization, corestore.Serving, func(w tenantstore.Writer) error {
		policy, err := w.ProjectPolicy(t.Context(), main.issue.ProjectID)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(tracker.ChangeRequest{ID: "change_activity", OrganizationID: f.organization, ProjectID: main.issue.ProjectID, WorkItemID: main.issue.NativeID, Title: "Activity change", Revision: 1, CreatedAt: f.now, UpdatedAt: f.now})
		if err != nil {
			return err
		}
		prepared, err := content.PrepareChange(t.Context(), f.organization, raw)
		if err != nil {
			return err
		}
		change, err := w.CreateChange(t.Context(), prepared)
		if err != nil {
			return err
		}
		raw, err = json.Marshal(tracker.ChangeVersion{ID: "version_activity", ChangeID: change.ID, Number: 1, CreatedAt: f.now, Actor: tracker.Actor{Kind: "human", PrincipalID: "user_browser_owner"}, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), PolicyID: policy.PolicyID, Repository: "example-org/example-project", AttemptID: main.attempt.ID, External: &tracker.ChangeExternalReference{Provider: "github", ID: "1", URL: "https://example.test/pull/1"}}})
		if err != nil {
			return err
		}
		version, err := content.PrepareVersion(t.Context(), f.organization, raw)
		if err != nil {
			return err
		}
		if _, err := w.CreateVersion(t.Context(), version); err != nil {
			return err
		}
		change.CurrentVersionID = &version.ID
		if _, err := w.UpdateChange(t.Context(), change); err != nil {
			return err
		}
		return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: f.organization, ProjectID: main.issue.ProjectID, WorkItemID: main.issue.NativeID, ChangeID: change.ID, CreatedAt: f.now})
	}); err != nil {
		t.Fatal(err)
	}
	return active
}

type activitySQLStatement struct {
	sql  string
	args []any
}

type activitySQLTrace struct {
	mu         sync.Mutex
	statements []activitySQLStatement
}

func (trace *activitySQLTrace) record(statement string, args []any) {
	if strings.Contains(statement, "native_attempts") || strings.Contains(statement, "collaboration_events") || strings.Contains(statement, "FROM change_requests") {
		trace.mu.Lock()
		trace.statements = append(trace.statements, activitySQLStatement{sql: statement, args: append([]any{}, args...)})
		trace.mu.Unlock()
	}
}

func (trace *activitySQLTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace.record(data.SQL, data.Args)
	return ctx
}

func (*activitySQLTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (*activitySQLTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (trace *activitySQLTrace) TraceBatchQuery(_ context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	trace.record(data.SQL, data.Args)
}

func (*activitySQLTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func (f *activityPostgresFixture) assertPlans(t *testing.T, read func()) {
	t.Helper()
	if _, err := f.admin.ExecContext(t.Context(), "ANALYZE"); err != nil {
		t.Fatal(err)
	}
	connection := f.service.config.Postgres.Connection
	config := nativeConfig(t, f.admin, connection.User, connection.Password)
	config.MaxConns = int32(f.service.config.Postgres.Pool.MaxConnections)
	trace := &activitySQLTrace{}
	config.ConnConfig.Tracer = multitracer.New(config.ConnConfig.Tracer, trace)
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	repository, err := postgres.NewRepository(t.Context(), pool, f.service.config.Postgres.Pool)
	if err != nil {
		t.Fatal(err)
	}
	previous := f.service.config.TenantRepository
	f.service.config.TenantRepository = repository
	defer func() { f.service.config.TenantRepository = previous }()
	read()
	trace.mu.Lock()
	statements := append([]activitySQLStatement{}, trace.statements...)
	trace.mu.Unlock()
	if len(statements) == 0 {
		t.Fatal("activity did not execute any source queries")
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(cleanup); err != nil && err != pgx.ErrTxClosed {
			t.Error(err)
		}
	}()
	if _, err := tx.Exec(t.Context(), "SELECT set_config('detent.organization_id', $1, true)", string(f.organization)); err != nil {
		t.Fatal(err)
	}
	indexes := map[string]bool{}
	for _, statement := range statements {
		var raw []byte
		if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement.sql, statement.args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plans []struct{ Plan map[string]any }
		if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
			t.Fatalf("decode activity plan: %v", err)
		}
		visited := map[string]float64{}
		var walk func(map[string]any)
		walk = func(node map[string]any) {
			if name, ok := node["Index Name"].(string); ok {
				indexes[name] = true
			}
			if relation, ok := node["Relation Name"].(string); ok {
				rows, _ := node["Actual Rows"].(float64)
				filtered, _ := node["Rows Removed by Filter"].(float64)
				loops, _ := node["Actual Loops"].(float64)
				visited[relation] += (rows + filtered) * loops
			}
			children, _ := node["Plans"].([]any)
			for _, child := range children {
				if child, ok := child.(map[string]any); ok {
					walk(child)
				}
			}
		}
		walk(plans[0].Plan)
		for _, relation := range []string{"native_attempts", "collaboration_events", "change_requests"} {
			if visited[relation] > 10001 {
				t.Fatalf("activity walked history outside its source budget: relation=%s visited=%.0f plan=%s", relation, visited[relation], raw)
			}
		}
		t.Logf("activity actual plan rows=%v", visited)
	}
	if !indexes["native_attempts_activity_recent_idx"] || !indexes["native_attempts_activity_running_idx"] {
		t.Fatalf("activity source windows lacked their native indexes: %v", indexes)
	}
}
