package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestActivityIssueResidence(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	lanes := []tracker.NativeState{{Name: "Ideas"}, {Name: "Ready", Dispatchable: true}, {Name: "Build", Dispatchable: true}, {Name: "Waiting"}, {Name: "Shipped", Terminal: true}, {Name: "Cancelled", Terminal: true}}
	type entry struct {
		lane   string
		second int
	}
	for _, tt := range []struct {
		name         string
		entries      []entry
		start, total float64
		seconds      []float64
		counts       []int
		names        []string
		terminal     string
		unknown      bool
	}{
		{name: "first active entry excludes backlog", entries: []entry{{"Ideas", 0}, {"Ready", 10}, {"Build", 30}}, start: 10, total: 90, seconds: []float64{20, 70}, counts: []int{1, 1}, names: []string{"Ready", "Build"}},
		{name: "holding before active resets cycle", entries: []entry{{"Ready", 0}, {"Build", 10}, {"Ideas", 20}, {"Ready", 40}, {"Build", 60}}, start: 40, total: 60, seconds: []float64{20, 40}, counts: []int{1, 1}, names: []string{"Ready", "Build"}},
		{name: "holding after active parks without reset", entries: []entry{{"Ready", 0}, {"Build", 10}, {"Waiting", 20}, {"Build", 50}}, total: 100, seconds: []float64{10, 60, 30}, counts: []int{1, 2, 1}, names: []string{"Ready", "Build", "Waiting"}},
		{name: "unvisited active lanes present and holding absent", entries: []entry{{"Ready", 0}}, total: 100, seconds: []float64{100, 0}, counts: []int{1, 0}, names: []string{"Ready", "Build"}},
		{name: "custom terminal stops clock", entries: []entry{{"Ready", 0}, {"Build", 10}, {"Shipped", 70}}, total: 70, seconds: []float64{10, 60}, counts: []int{1, 1}, names: []string{"Ready", "Build"}, terminal: "Shipped"},
		{name: "cancelled is terminal", entries: []entry{{"Ideas", 0}, {"Ready", 10}, {"Cancelled", 25}}, start: 10, total: 15, seconds: []float64{15, 0}, counts: []int{1, 0}, names: []string{"Ready", "Build"}, terminal: "Cancelled"},
		{name: "truncated prefix does not fabricate a start", entries: []entry{{"Build", 20}}, unknown: true, seconds: []float64{0, 0}, counts: []int{0, 0}, names: []string{"Ready", "Build"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			samples := []analyticsResidenceSample{{Created: base, At: base, Entered: base, Initial: tt.entries[0].lane, State: tt.entries[0].lane}}
			for i, e := range tt.entries[1:] {
				at := base.Add(time.Duration(e.second) * time.Second)
				samples = append(samples, analyticsResidenceSample{Created: base, At: at, Entered: at, State: e.lane, Sequence: int64(i + 1)})
			}
			if tt.unknown {
				samples[0].Sequence = 4
			}
			got := activityIssueResidence(operatortool.ActivityIssue{}, lanes, samples, base.Add(100*time.Second))
			if got.Partial != tt.unknown || (!tt.unknown && got.ClockStartedAt.Sub(base).Seconds() != tt.start) || (tt.unknown && !got.ClockStartedAt.IsZero()) || got.TotalSeconds != tt.total || got.TerminalLane != tt.terminal {
				t.Fatalf("issue=%+v", got)
			}
			var names []string
			var seconds []float64
			var counts []int
			for _, lane := range got.Lanes {
				names = append(names, lane.Name)
				seconds = append(seconds, lane.Seconds)
				counts = append(counts, lane.Entries)
			}
			if !reflect.DeepEqual(names, tt.names) || !reflect.DeepEqual(seconds, tt.seconds) || !reflect.DeepEqual(counts, tt.counts) {
				t.Fatalf("lanes=%+v", got.Lanes)
			}
		})
	}
}

func TestActivityIssueRead(t *testing.T) {
	lanes := []tracker.NativeState{{Name: "Ideas"}, {Name: "Ready", Dispatchable: true}, {Name: "Build", Dispatchable: true}, {Name: "Waiting"}, {Name: "Shipped", Terminal: true}, {Name: "Cancelled", Terminal: true}}
	type entry struct {
		lane   string
		second int
	}
	for _, tt := range []struct {
		name                       string
		entries                    []entry
		live                       bool
		attempts                   int
		finished                   bool
		worked                     float64
		runner                     string
		reporter, expired, missing bool
		reset                      bool
	}{
		{name: "two overlapping attempts yield one running issue", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 2, worked: 80},
		{name: "parked holding issue without live attempt omitted", entries: []entry{{"Ready", 0}, {"Waiting", 20}}, attempts: 1},
		{name: "active issue without live attempt omitted", entries: []entry{{"Ready", 0}}, attempts: 1},
		{name: "custom terminal in window with no attempts included", entries: []entry{{"Ready", 0}, {"Shipped", 90}}, finished: true},
		{name: "cancelled issue included with worked time excluding parked time", entries: []entry{{"Ready", 0}, {"Waiting", 20}, {"Cancelled", 90}}, attempts: 1, finished: true, worked: 30},
		{name: "terminal before window omitted", entries: []entry{{"Ready", 0}, {"Shipped", 10}}},
		{name: "runner filter matches a previous runner", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 2, worked: 80, runner: "runner_previous"},
		{name: "runner filter excludes other issues", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 1, runner: "runner_other"},
		{name: "reporter sees only their issue and baseline", entries: []entry{{"Ready", 0}, {"Shipped", 90}}, finished: true, attempts: 1, worked: 30, reporter: true},
		{name: "expired attempt is not live", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 1, expired: true},
		{name: "live issue remains visible without rollups", entries: []entry{{"Ready", 0}, {"Build", 20}}, live: true, attempts: 1, missing: true},
		{name: "reset excludes earlier attempt phases", entries: []entry{{"Ready", 0}, {"Build", 10}, {"Ideas", 20}, {"Ready", 60}, {"Build", 70}}, live: true, attempts: 2, worked: 40, reset: true},
		{name: "holding after active does not reset", entries: []entry{{"Ready", 0}, {"Waiting", 20}, {"Build", 60}}, live: true, attempts: 2, worked: 80},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newUsageHostedFixture(t)
			now := f.service.config.now().UTC().Truncate(time.Microsecond)
			base := now.Add(-100 * time.Second)
			db := f.service.database.db

			project := f.createProject(t, "Activity example "+tt.name)
			scope := nativeScope{organization: "org_browser_preview", project: tracker.ProjectID(project)}
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyNativeProjectStates(t.Context(), tx, scope, lanes, base); err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ids := []string{}
			for n := range 2 {
				issue, err := createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Body: nativeContractTestBody, Title: fmt.Sprintf("Example issue %d", n), State: tt.entries[0].lane}, base)
				if err != nil {
					t.Fatal(err)
				}
				id := string(issue.WorkItemID)
				actor, err := json.Marshal(tracker.Actor{PrincipalID: fmt.Sprintf("reporter_%d", n)})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `UPDATE issues SET actor_json=? WHERE native_id=?`, string(actor), id); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
				state := tt.entries[len(tt.entries)-1].lane
				if _, err := tx.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND source_name=?),updated_at=? WHERE native_id=?`, project, state, formatHubTime(now), id); err != nil {
					t.Fatal(err)
				}
				if tt.finished || tt.name == "terminal before window omitted" {
					at := base.Add(time.Duration(tt.entries[len(tt.entries)-1].second) * time.Second)
					if _, err := tx.ExecContext(t.Context(), `UPDATE issues SET terminal_entered_at=? WHERE native_id=?`, formatHubTime(at), id); err != nil {
						t.Fatal(err)
					}
				}

				for i, e := range tt.entries {
					if tt.missing {
						continue
					}
					at := base.Add(time.Duration(e.second) * time.Second)
					sample := analyticsResidenceSample{ID: id, Created: base, At: at, Entered: at, Initial: tt.entries[0].lane, State: e.lane, Sequence: int64(i)}
					if err := writeAnalyticsResidenceSample(t.Context(), tx, scope, "residence_edge", fmt.Sprintf("%s:%d", id, i), sample); err != nil {
						t.Fatal(err)
					}
				}
				for i := range tt.attempts {
					lease := fmt.Sprintf("activity_%s_%d", id, i)
					attemptID := lease + "_attempt"
					released := any(formatHubTime(base.Add(50 * time.Second)))
					status := "succeeded"
					if tt.live && i == tt.attempts-1 {
						status = "running"
						released = nil
					}
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) SELECT ?,i.id,l.machine_id,?, ?,?,?,?, ?,? FROM leases l JOIN issues i ON i.native_id=? WHERE l.lease_id='lease_browser_usage_0_0'`, lease, lease+"_session", formatHubTime(now.Add(time.Hour)), formatHubTime(base), formatHubTime(base), released, formatHubTime(base), formatHubTime(base), id); err != nil {
						t.Fatal(err)
					}
					runner := "runner_previous"
					if i > 0 {
						runner = "runner_current"
					}
					if tt.expired {
						if _, err := tx.ExecContext(t.Context(), `UPDATE leases SET expires_at=? WHERE lease_id=?`, formatHubTime(now.Add(-time.Second)), lease); err != nil {
							t.Fatal(err)
						}
					}

					phase := tracker.NativePhase{StartedAt: base.Add(time.Duration(20+i*10) * time.Second), FinishedAt: base.Add(50 * time.Second)}
					if status == "running" {
						phase.FinishedAt = time.Time{}
					}
					run := tracker.NativeRunData{RunnerID: runner, Runtime: &tracker.NativeRuntimeObservation{Identity: agentidentity.Identity{Role: "security_audit"}, Phases: []tracker.NativePhase{phase}}}
					raw, err := json.Marshal(run)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,checkpoint_json,started_at,updated_at) SELECT ?,?,?,?,?,(SELECT max(fencing_token)+1 FROM native_attempts),?,1,?,?,'{}',?,? FROM native_attempts WHERE id='attempt_browser_usage_0_0'`, attemptID, scope.organization, project, id, lease, attemptID+"_run", status, string(raw), formatHubTime(base), formatHubTime(base.Add(50*time.Second))); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			assertActivityQueryPlan(t, db, activityIssueSamplesQuery, []any{scope.organization, project, ids[0], activityIssueHistoryLimit + 1}, "analytics_activity_issue_edges")
			assertActivityQueryPlan(t, db, activityIssueCandidatesQuery, []any{sql.Named("org", scope.organization), sql.Named("project", project), sql.Named("from", formatHubTime(base)), sql.Named("now", formatHubTime(now)), sql.Named("limit", 100)}, "issues_activity_terminal")

			out := operatortool.ActivityReport{OrganizationID: string(scope.organization), ObservedAt: now, Window: operatortool.AnalyticsWindow{From: base.Add(50 * time.Second), To: now}}
			ctx := t.Context()
			if tt.reporter {
				ctx = context.WithValue(ctx, reportsScopeKey{}, reportsScope{reporterPrincipal: "reporter_0"})
			}
			if err := readActivityIssues(ctx, db, []string{project}, operatortool.ActivityRequest{Limit: 100, RunnerID: tt.runner}, &out); err != nil {
				t.Fatal(err)
			}
			rows := out.RunningIssues
			want := 0
			if tt.live && !tt.expired && tt.runner != "runner_other" {
				want = 2
			}
			if tt.reporter {
				want = 1
			}
			if tt.finished {
				rows = out.FinishedIssues
				want = 2
				if tt.reporter {
					want = 1
				}
				if out.IssueTypicalDurations[0].Total.Count != want || out.IssueTypicalDurations[0].Total.P50Seconds != 90 {
					t.Fatalf("baseline=%+v", out.IssueTypicalDurations)
				}
			}
			if len(rows) != want || !tt.finished && len(out.FinishedIssues) != 0 || !tt.live && len(out.RunningIssues) != 0 {
				t.Fatalf("running=%+v finished=%+v", out.RunningIssues, out.FinishedIssues)
			}
			for _, row := range rows {
				if tt.missing && (!row.ClockStartedAt.IsZero() || !row.Partial) {
					t.Fatalf("missing timing=%+v", row)
				}
				if !reflect.DeepEqual([]string{row.Lanes[0].Name, row.Lanes[1].Name}, []string{"Ready", "Build"}) || row.WorkedSeconds != tt.worked {
					t.Fatalf("row=%+v", row)
				}
				if tt.reset && (row.TotalSeconds != 40 || row.ClockStartedAt.Sub(base).Seconds() != 60) {
					t.Fatalf("reset=%+v", row)
				}
				if tt.attempts == 2 && !tt.reset && (len(row.RunnerIDs) != 2 || len(row.WorkedByRole) != 1 || row.WorkedByRole[0].Attempts != 2 || row.RunnerID != "runner_current") {
					t.Fatalf("work=%+v", row)
				}
			}
		})
	}
}
