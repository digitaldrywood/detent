package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func applyTestHealth(t *testing.T, f nativeFixture, now time.Time, findings []healthFinding) {
	t.Helper()
	if err := f.service.commitHealthEvaluation(t.Context(), f.project.OrganizationID, now, findings, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHealthFindingStartTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	start := now.Add(-48 * time.Hour)
	for _, test := range []struct {
		name         string
		wait         healthWait
		withoutStart bool
		want         time.Time
	}{
		{name: "transition wait", wait: healthWait{Event: "transition", Since: start}, want: start},
		{name: "attempt disposition wait", wait: healthWait{Attempt: "attempt", Since: start}, want: start},
		{name: "question wait", wait: healthWait{Since: start}, want: start},
		{name: "rule without start", withoutStart: true, want: now},
		{name: "contract wait without start", wait: healthWait{Reason: "Complete the issue contract."}, want: now},
		{name: "contract wait with future start", wait: healthWait{Reason: "Complete the issue contract.", Since: now.Add(time.Minute)}, want: now},
		{name: "contract wait bypasses age gate", wait: healthWait{Reason: "Complete the issue contract.", Since: now.Add(-time.Minute)}, want: now.Add(-time.Minute)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "health-start")
			var finding healthFinding
			if test.withoutStart {
				finding = newHealthFinding("fixture", "instance", "work_item", "item", "", "", []string{string(f.project.ID)}, healthEvidence{})
			} else {
				wait := test.wait
				wait.Item, wait.Project, wait.Active = "item", string(f.project.ID), true
				var active bool
				finding, active = humanWaitFinding(now, wait)
				if !active {
					t.Fatal("finding is inactive")
				}
			}
			applyTestHealth(t, f, now, []healthFinding{finding})
			var opened, seen string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT opened_at,last_seen_at FROM health_findings").Scan(&opened, &seen); err != nil {
				t.Fatal(err)
			}
			if opened != formatHubTime(test.want) || seen != formatHubTime(now) {
				t.Fatalf("opened=%s seen=%s want opened=%s seen=%s", opened, seen, formatHubTime(test.want), formatHubTime(now))
			}
		})
	}
}

func TestHealthFindingLifecycle(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "health-lifecycle")
	addHealthBacklog(t, f)
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	start := now.Add(-48 * time.Hour)
	finding, _ := humanWaitFinding(now, healthWait{Item: "item", Project: string(f.project.ID), Since: start, Active: true})
	var original string
	for _, test := range []struct {
		name   string
		offset time.Duration
		active bool
		rows   int
		reused bool
		seen   time.Duration
		start  time.Duration
	}{
		{"opens", 0, true, 1, true, 0, -48 * time.Hour},
		{"updates last seen", time.Minute, true, 1, true, time.Minute, -48 * time.Hour},
		{"resolves", 2 * time.Minute, false, 1, true, time.Minute, -48 * time.Hour},
		{"reopens same record at sixty minutes", 62 * time.Minute, true, 1, true, 62 * time.Minute, -48 * time.Hour},
		{"resolves again", 63 * time.Minute, false, 1, true, 62 * time.Minute, -48 * time.Hour},
		{"later recurrence opens new record", 124 * time.Minute, true, 2, false, 124 * time.Minute, -48 * time.Hour},
		{"resolves before new episode", 125 * time.Minute, false, 2, false, 124 * time.Minute, -48 * time.Hour},
		{"new wait episode uses its own start", 187 * time.Minute, true, 3, false, 187 * time.Minute, 150 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			finding.ConditionSince = now.Add(test.start)
			if test.name == "updates last seen" {
				finding.ConditionSince = now.Add(-time.Hour)
			}
			findings := evaluateHealth(now.Add(test.offset), healthSnapshot{Projects: []healthProject{{ID: string(f.project.ID), Candidates: 1, RefusalReasons: []string{"runner_capacity_full"}}}})
			if test.active {
				findings = append(findings, finding)
			}
			applyTestHealth(t, f, now.Add(test.offset), findings)
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM health_findings").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != test.rows {
				t.Fatalf("rows=%d want %d", count, test.rows)
			}
			var id, opened, seen string
			var resolved sql.NullString
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id,opened_at,last_seen_at,resolved_at FROM health_findings ORDER BY rowid DESC LIMIT 1").Scan(&id, &opened, &seen, &resolved); err != nil {
				t.Fatal(err)
			}
			if original == "" {
				original = id
			}
			if (id == original) != test.reused {
				t.Fatalf("reuse=%v", id == original)
			}
			if seen != formatHubTime(now.Add(test.seen)) || resolved.Valid == test.active {
				t.Fatalf("seen=%s resolved=%v", seen, resolved)
			}
			if opened != formatHubTime(now.Add(test.start)) {
				t.Fatalf("opened_at changed: %s", opened)
			}
		})
	}
}

func TestHealthDetectorEmptyTenantAndObservationOnly(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "empty-health")
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	addHealthBacklog(t, f)
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	if err := f.service.evaluateOrganizationHealth(t.Context(), f.project.OrganizationID, now); err != nil {
		t.Fatal(err)
	}
	var emptyCount int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM health_findings").Scan(&emptyCount); err != nil {
		t.Fatal(err)
	}
	if emptyCount != 0 {
		t.Fatalf("empty tenant findings=%d", emptyCount)
	}
	item := f.create(t, "untouched")
	before, err := f.service.readIssues(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	drainTestNativeBoard(t, f.service)
	if err := f.service.evaluateOrganizationHealth(t.Context(), f.project.OrganizationID, now); err != nil {
		t.Fatal(err)
	}
	var findings, leases int
	var tick string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM health_findings").Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM leases").Scan(&leases); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT last_tick_at FROM health_detector_ticks WHERE organization_id=?", f.project.OrganizationID).Scan(&tick); err != nil {
		t.Fatal(err)
	}
	if findings != 0 || leases != 0 || tick != formatHubTime(now) {
		t.Fatalf("findings=%d leases=%d tick=%s", findings, leases, tick)
	}
	after, err := f.service.readIssues(t.Context(), nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}, url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	if after.ChangeSequence <= before.ChangeSequence {
		t.Fatal("board refresh did not advance project change sequence")
	}
	before.ChangeSequence = after.ChangeSequence
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatalf("detector changed work item %s", item.WorkItemID)
	}
}

func TestHealthFindingsReads(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "health-reads")
	addHealthBacklog(t, f)
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-health")
	addHealthBacklog(t, other)
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	findings := []healthFinding{}
	for _, test := range []struct {
		id      string
		project tracker.ProjectID
	}{{"first", f.project.ID}, {"second", f.project.ID}, {"hidden", other.project.ID}} {
		findings = append(findings, newHealthFinding("test", "instance", "project", test.id, "Summary.", "The operator acts.", []string{string(test.project)}, healthEvidence{}))
	}
	findings[0].Projects = append(findings[0].Projects, string(other.project.ID))
	findings[0].Evidence = healthEvidence{Counts: map[string]int{"queue_depth": 8}, Queues: map[string]healthQueueEvidence{string(f.project.ID): {QueueDepth: 3}, string(other.project.ID): {QueueDepth: 5}}}
	scheduled, active := deadManFinding(now, healthProject{ID: string(f.project.ID), Scheduled: healthScheduledValidation{LastReport: now.Add(-6 * time.Hour), LastRunURL: "https://github.com/example-org/example-project/actions/runs/1/attempts/1"}})
	if !active {
		t.Fatal("scheduled validation finding was not active")
	}
	findings[1] = scheduled
	applyTestHealth(t, f, now, findings)
	for _, test := range []struct {
		name, path string
		status     int
		count      int
	}{
		{"open", f.base + "/health/findings", http.StatusOK, 2},
		{"resolved empty", f.base + "/health/findings?state=resolved", http.StatusOK, 0},
		{"since future", f.base + "/health/findings?since=" + url.QueryEscape(formatHubTime(now.Add(time.Second))), http.StatusOK, 0},
		{"since exact", f.base + "/health/findings?since=" + url.QueryEscape(formatHubTime(now)), http.StatusOK, 2},
		{"all open", f.base + "/health/findings?state=all", http.StatusOK, 2},
		{"invalid state", f.base + "/health/findings?state=unknown", http.StatusUnprocessableEntity, 0},
		{"invalid since", f.base + "/health/findings?since=yesterday", http.StatusUnprocessableEntity, 0},
		{"invalid limit", f.base + "/health/findings?limit=101", http.StatusUnprocessableEntity, 0},
		{"invalid cursor", f.base + "/health/findings?cursor=bad", http.StatusUnprocessableEntity, 0},
		{"other project", other.base + "/health/findings", http.StatusNotFound, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, test.path, f.token, nil)
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				return
			}
			var page healthFindingsPage
			decodeHubResponse(t, response, &page)
			for _, finding := range page.Items {
				if len(finding.Evidence.Queues) > 0 && (len(finding.Evidence.Queues) != 1 || finding.Evidence.Queues[string(f.project.ID)].QueueDepth != 3 || finding.Evidence.Counts["queue_depth"] != 3) {
					t.Fatalf("queue evidence escaped project scope: %+v", finding)
				}
			}
			if len(page.Items) != test.count || page.LastTickAt == nil || !page.LastTickAt.Equal(now) {
				t.Fatalf("page=%+v", page)
			}
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/health/findings?limit=1", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var first healthFindingsPage
	decodeHubResponse(t, response, &first)
	if len(first.Items) != 1 || first.NextCursor == "" {
		t.Fatalf("first=%+v", first)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/health/findings?limit=1&cursor="+first.NextCursor, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var second healthFindingsPage
	decodeHubResponse(t, response, &second)
	if len(second.Items) != 1 || second.NextCursor != "" || second.Items[0].ID == first.Items[0].ID {
		t.Fatalf("second=%+v", second)
	}
	contexts := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/health-read-test", func(c echo.Context) error { contexts <- c.Request().Context(); return c.NoContent(http.StatusOK) }, f.service.operatorAuthority)
	response = performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/health-read-test", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	ctx := <-contexts
	ctx = operatortool.BindConnection(ctx, operatortool.ConnectionIdentity(ctx).PrincipalID, "health-test")
	executor := hostedOperatorExecutor{f.service}
	result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.HealthFindings, Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Content), "last_tick_at") || !strings.Contains(string(result.Content), "scheduled_silent") || !strings.Contains(string(result.Content), "actions/runs/1/attempts/1") {
		t.Fatalf("MCP result=%+v", result)
	}
	applyTestHealth(t, f, now.Add(time.Minute), nil)
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/health/findings?state=resolved&since="+url.QueryEscape(formatHubTime(now.Add(time.Second))), f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var resolved healthFindingsPage
	decodeHubResponse(t, response, &resolved)
	if len(resolved.Items) != 2 {
		t.Fatalf("resolved=%+v", resolved)
	}
	opened := newHealthFinding("test", "instance", "project", "new-open", "New finding.", "Check project.", []string{string(f.project.ID)}, healthEvidence{})
	applyTestHealth(t, f, now.Add(2*time.Minute), []healthFinding{opened})
	for _, test := range []struct {
		name  string
		since time.Time
		count int
	}{
		{"includes resolutions", now.Add(time.Minute), 3},
		{"excludes earlier resolutions", now.Add(time.Minute + time.Second), 1},
		{"keeps open findings before since", now.Add(3 * time.Minute), 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/health/findings?state=all&since="+url.QueryEscape(formatHubTime(test.since)), f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page healthFindingsPage
			decodeHubResponse(t, response, &page)
			if len(page.Items) != test.count {
				t.Fatalf("findings=%+v", page.Items)
			}
			foundOpen := false
			for _, finding := range page.Items {
				if finding.Subject.ID == "new-open" && finding.ResolvedAt == nil {
					foundOpen = true
				}
			}
			if !foundOpen {
				t.Fatal("combined read lost the open finding")
			}
		})
	}
}

func TestHealthSnapshotRunnerAndPolicyFallback(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "runner-health")
	addHealthBacklog(t, f)
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	r.enroll(t)
	f.create(t, "candidate")
	clearNativeFixturePolicy(t, f)
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET native_created_at=? WHERE project_id=?", formatHubTime(now.Add(-time.Hour)), f.project.ID); err != nil {
		t.Fatal(err)
	}
	routing := map[string]any{"project_ranks": map[string]int{string(f.project.ID): 1}}
	raw, err := json.Marshal(routing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET routing_settings_json=?,last_heartbeat_at=?,created_at=? WHERE id=?", string(raw), formatHubTime(now.Add(-6*time.Minute)), formatHubTime(now.Add(-time.Hour)), r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		heartbeat time.Time
		policy    bool
		want      string
	}{
		{"stale runner", now.Add(-6 * time.Minute), false, "runner_heartbeat_gap"},
		{"configuration not yet reported", now, false, "dead_man"},
		{"policy restored", now, true, "dead_man"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", formatHubTime(test.heartbeat), r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			if test.policy {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			}
			snapshot, err := readHealthSnapshot(t.Context(), f.service.database.db, f.project.OrganizationID, now)
			if err != nil {
				t.Fatal(err)
			}
			got := evaluateHealth(now, snapshot)
			found := false
			for _, finding := range got {
				if finding.Signal == test.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing %s in %+v snapshot=%+v", test.want, got, snapshot)
			}
		})
	}
}

func TestHealthReadFailurePreservesFindingsAndTick(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "failed-health")
	addHealthBacklog(t, f)
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	finding := newHealthFinding("test", "instance", "project", string(f.project.ID), "Summary.", "The operator acts.", []string{string(f.project.ID)}, healthEvidence{})
	applyTestHealth(t, f, now, []healthFinding{finding})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.service.evaluateOrganizationHealth(ctx, f.project.OrganizationID, now.Add(time.Minute)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	var resolved sql.NullString
	var tick string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT resolved_at FROM health_findings").Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT last_tick_at FROM health_detector_ticks").Scan(&tick); err != nil {
		t.Fatal(err)
	}
	if resolved.Valid || tick != formatHubTime(now) {
		t.Fatalf("resolved=%v tick=%s", resolved, tick)
	}
}

func appendTestHealthEvent(t *testing.T, f nativeFixture, item tracker.NativeIssue, kind string, data tracker.CollaborationData, at time.Time) {
	t.Helper()
	appendTestHealthEventAs(t, f, tracker.Actor{Kind: "runner", PrincipalID: "runner-health"}, item, kind, data, at)
}

func appendTestHealthEventAs(t *testing.T, f nativeFixture, actor tracker.Actor, item tracker.NativeIssue, kind string, data tracker.CollaborationData, at time.Time) {
	t.Helper()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, sourceActor: &actor}
	if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(item.WorkItemID), kind, data, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestHealthSnapshotRecordedCyclesAndLandings(t *testing.T) {
	t.Run("portable time windows", func(t *testing.T) {
		f := newDefaultNativeFixture(t, Config{})
		if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organizations(id,name,created_at) VALUES('org_other_time','Other organization','2026-10-01T00:00:00Z')`); err != nil {
			t.Fatal(err)
		}
		other := newNativeFixture(t, f.service, "org_other_time", "other-time")
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
		tests := []struct{ at string }{
			{"2026-10-21T11:59:59.9996Z"},
			{"2026-10-21T11:59:59.999999999Z"},
			{"2026-10-21T07:00:00-05:00"},
			{formatHubTime(now.Add(-healthInstanceWindow))},
			{formatHubTime(now.Add(-healthInstanceWindow - time.Nanosecond))},
			{"2026-10-21T12:00:00.000000001Z"},
		}
		reasons := []string{"policy_mismatch", "runner_heartbeat_stale"}
		candidates := make(map[tracker.ProjectID]string)
		for i, fixture := range []nativeFixture{f, other} {
			item := fixture.create(t, "time windows")
			candidates[fixture.project.ID] = string(item.WorkItemID)
			appendTestHealthEvent(t, fixture, item, "run.observed", tracker.CollaborationData{Run: &tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, ObservedAt: now.Add(-time.Minute)}}}}, now.Add(-time.Minute))
			for index, test := range tests {
				at, err := parseTimeValue(test.at)
				if err != nil {
					t.Fatal(err)
				}
				decision := tracker.NativeSchedulerDecision{RunnerID: "runner-time", Source: "native_runner_routing", Outcome: "skipped", Reason: reasons[i], At: at}
				if at.After(now) {
					decision.Outcome, decision.Reason = "claimed", ""
				}
				raw, err := json.Marshal(tracker.CollaborationData{Decision: &decision})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at) VALUES(?,?,?,?,?,'scheduler.decision',1,'{}',?,?)`, newNativeID("evt"), fixture.project.OrganizationID, fixture.project.ID, item.WorkItemID, index+100, string(raw), test.at); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM health_event_times; DELETE FROM time_index_progress WHERE owner='health'"); err != nil {
			t.Fatal(err)
		}
		var before [][]healthTimeEvent
		var beforeFindings [][]healthFinding
		for _, phase := range []string{"legacy", "projected", "populated"} {
			if phase == "projected" {
				if err := f.service.convertHealthProjections(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "populated" {
				if err := f.service.database.populateTimeIndexes(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			for i, fixture := range []nativeFixture{f, other} {
				events, err := readHealthTimeEvents(t.Context(), f.service.database.reader, fixture.project.OrganizationID, now)
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 5 {
					t.Fatalf("%s events=%+v", phase, events)
				}
				for j, event := range events {
					if event.project != string(fixture.project.ID) {
						t.Fatal("cross-organization event")
					}
					if j > 0 && event.at.Before(events[j-1].at) {
						t.Fatal("events not ordered by nanosecond")
					}
				}
				if !events[0].at.Equal(now.Add(-healthInstanceWindow)) || !events[len(events)-1].at.Equal(now) {
					t.Fatalf("cutoff equality lost: %+v", events)
				}
				snapshot := healthSnapshot{Projects: []healthProject{{ID: string(fixture.project.ID), Candidates: 1, CandidateIDs: map[string]bool{candidates[fixture.project.ID]: true}, FreeSlots: 1, CandidateSince: now.Add(-time.Hour)}}}
				if err := readHealthEvents(t.Context(), f.service.database.reader, fixture.project.OrganizationID, now, &snapshot); err != nil {
					t.Fatal(err)
				}
				if !snapshot.Projects[0].LastLanding.Equal(now.Add(-time.Minute)) {
					t.Fatal("landing changed")
				}
				findings := evaluateHealth(now, snapshot)
				if len(findings) != 1 || findings[0].Signal != "every_candidate_refused" || findings[0].Subject.ID != string(fixture.project.ID) || findings[0].Evidence.Reason != reasons[i] || findings[0].Evidence.Counts["candidates"] != 1 || findings[0].Evidence.Counts["claims"] != 0 || len(findings[0].Evidence.EventIDs) != 1 || findings[0].Evidence.EventIDs[0] != events[len(events)-1].id {
					t.Fatalf("%s organization %s findings=%+v", phase, fixture.project.OrganizationID, findings)
				}
				if phase == "legacy" {
					before = append(before, events)
					beforeFindings = append(beforeFindings, findings)
				} else {
					if !reflect.DeepEqual(before[i], events) {
						t.Fatalf("%s changed health events: before=%+v after=%+v", phase, before[i], events)
					}
					if !reflect.DeepEqual(beforeFindings[i], findings) {
						t.Fatalf("%s changed health findings: before=%+v after=%+v", phase, beforeFindings[i], findings)
					}
				}
			}
			if phase == "populated" {
				item := f.create(t, "new event after population")
				appendTestHealthEvent(t, f, item, "scheduler.decision", tracker.CollaborationData{}, now.Add(-time.Second))
				events, err := readHealthTimeEvents(t.Context(), f.service.database.reader, f.project.OrganizationID, now)
				if err != nil || len(events) != 6 {
					t.Fatalf("new event missing: count=%d err=%v", len(events), err)
				}
			}
		}
		var old int
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM sqlite_schema WHERE name IN ('health_decision_window','health_landing_window','hosted_audit_retention_idx')`).Scan(&old); err != nil || old != 0 {
			t.Fatalf("old indexes=%d err=%v", old, err)
		}
	})

	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		reasons     []string
		claims      bool
		old         bool
		landing     bool
		wantRefusal bool
	}{
		{name: "all refused", reasons: []string{"policy_mismatch", "policy_mismatch"}, wantRefusal: true},
		{name: "all slots working", reasons: []string{"runner_capacity_full", "runner_capacity_full"}},
		{name: "recorded full capacity", reasons: []string{"capacity_full", "capacity_full"}},
		{name: "offline", reasons: []string{"runner_heartbeat_stale", "runner_heartbeat_stale"}, wantRefusal: true},
		{name: "different reasons", reasons: []string{"policy_mismatch", "capacity_full"}},
		{name: "partial cycle", reasons: []string{"policy_mismatch"}},
		{name: "claimed cycle", reasons: []string{"policy_mismatch", "policy_mismatch"}, claims: true},
		{name: "outside history window", reasons: []string{"policy_mismatch", "policy_mismatch"}, old: true},
		{name: "landing observation", landing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "recorded-health")
			addHealthBacklog(t, f)
			items := []tracker.NativeIssue{f.create(t, "first"), f.create(t, "second")}
			at := now.Add(-time.Minute)
			if test.old {
				at = now.Add(-25 * time.Hour)
			}
			for i, reason := range test.reasons {
				appendTestHealthEvent(t, f, items[i], "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{RunnerID: "runner", Source: "native_runner_routing", Outcome: "skipped", Reason: reason, At: at}}, at)
			}
			if test.claims {
				appendTestHealthEvent(t, f, items[0], "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{RunnerID: "runner", Source: "native_claim", Outcome: "claimed", At: at}}, at)
			}
			if test.landing {
				appendTestHealthEvent(t, f, items[0], "run.observed", tracker.CollaborationData{Run: &tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, ObservedAt: at}}}}, at)
			}
			snapshot := healthSnapshot{Projects: []healthProject{{ID: string(f.project.ID), Candidates: 2, CandidateIDs: map[string]bool{string(items[0].WorkItemID): true, string(items[1].WorkItemID): true}, FreeSlots: 1, CandidateSince: now.Add(-time.Hour)}}}
			if err := readHealthEvents(t.Context(), f.service.database.db, f.project.OrganizationID, now, &snapshot); err != nil {
				t.Fatal(err)
			}
			_, refused := refusalFinding(snapshot.Projects[0])
			if refused != test.wantRefusal {
				t.Fatalf("refused=%v snapshot=%+v", refused, snapshot)
			}
			if test.landing {
				if !snapshot.Projects[0].LastLanding.Equal(at) {
					t.Fatalf("landing=%v", snapshot.Projects[0].LastLanding)
				}
				if _, active := deadManFinding(now, snapshot.Projects[0]); active {
					t.Fatal("recent landing reported dead man")
				}
			}
		})
	}
}

func TestHealthSnapshotCurrentWaitAndLimitAuthority(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "wait-health")
	addHealthBacklog(t, f)
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	item := f.create(t, "waiting")
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states(project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,'Blocked','Blocked',0,0,?,?)", f.project.ID, formatHubTime(at), formatHubTime(at)); err != nil {
		t.Fatal(err)
	}
	var itemID int64
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id=?", item.WorkItemID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	result, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,created_at,updated_at) VALUES('health-lease',?,?,'health-session',?,?,?,?,?)`, itemID, r.binding.MachineID, formatHubTime(at.Add(time.Minute)), formatHubTime(at), formatHubTime(at), formatHubTime(at), formatHubTime(at))
	if err != nil {
		t.Fatal(err)
	}
	fence, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	data := tracker.NativeRunData{Disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true, ReasonCode: "permission_wait"}}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) VALUES('attempt-health',?,?,?,'health-lease',?,'run-health',1,'succeeded',?,?,?)`, f.project.OrganizationID, f.project.ID, item.WorkItemID, fence, string(raw), formatHubTime(at), formatHubTime(at)); err != nil {
		t.Fatal(err)
	}
	appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Revision: item.Revision, Reason: "worker_progress"}, at)
	for _, test := range []struct {
		name      string
		supersede bool
		limit     bool
		action    bool
		typed     bool
		want      string
	}{
		{name: "permission action still authoritative", want: "unanswered_human_wait"},
		{name: "typed permission action still authoritative", typed: true, want: "unanswered_human_wait"},
		{name: "human action transition precedes final attempt update", action: true, want: "unanswered_human_wait"},
		{name: "new human revision supersedes attempt", supersede: true},
		{name: "lifetime transition remains current", limit: true, want: ""},
		{name: "recovered lifetime transition", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.typed {
				if err := f.service.convertAttemptHotProjection(t.Context(), "attempt-health"); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json='{}' WHERE id='attempt-health'`); err != nil {
					t.Fatal(err)
				}
				for _, query := range []string{
					`UPDATE collaboration_events SET actor_kind='human' WHERE work_item_id=? AND type='workflow.transitioned'`,
					`UPDATE collaboration_events SET transition_reason='user_requested' WHERE work_item_id=? AND type='workflow.transitioned'`,
					`UPDATE collaboration_events SET transition_revision=0 WHERE work_item_id=? AND type='workflow.transitioned'`,
					`UPDATE native_attempts SET disposition_status='complete' WHERE work_item_id=?`,
					`UPDATE native_attempts SET disposition_human_action=0,health_disposition_reason='user_requested' WHERE work_item_id=?`,
				} {
					tx, err := f.service.database.db.BeginTx(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), query, item.WorkItemID); err != nil {
						tx.Rollback()
						t.Fatal(err)
					}
					snapshot := healthSnapshot{}
					readErr := readHealthCurrentWaits(t.Context(), tx, f.project.OrganizationID, now, &snapshot)
					if err := tx.Rollback(); err != nil {
						t.Fatal(err)
					}
					if readErr != nil || len(snapshot.Waits) != 0 {
						t.Fatalf("typed authority filter %s: waits=%+v err=%v", query, snapshot.Waits, readErr)
					}
				}
				defer func() {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json=? WHERE id='attempt-health'`, string(raw)); err != nil {
						t.Fatal(err)
					}
					appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Revision: item.Revision, Reason: "worker_progress"}, at)
				}()
			}
			if test.action {
				defer func() {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=?,updated_at=? WHERE id='attempt-health'", string(raw), formatHubTime(at)); err != nil {
						t.Fatal(err)
					}
					appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Revision: item.Revision, Reason: "worker_progress"}, at)
				}()
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json=?,updated_at=? WHERE id='attempt-health'", `{"disposition":{"status":"blocked","human_action":true}}`, formatHubTime(at.Add(time.Second))); err != nil {
					t.Fatal(err)
				}
				appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Revision: item.Revision, ToState: "Blocked", Reason: "worker_progress", ReasonDetail: "human_action"}, at)
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Blocked') WHERE native_id=?", f.project.ID, item.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if test.supersede {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET revision=revision+1 WHERE native_id=?", item.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			if test.limit {
				appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Reason: "lifetime_limit"}, now.Add(-25*time.Hour))
			}
			if test.name == "recovered lifetime transition" {
				appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Reason: "lifetime_limit_recovered"}, now)
			}
			snapshot := healthSnapshot{}
			if err := readHealthCurrentWaits(t.Context(), f.service.database.db, f.project.OrganizationID, now, &snapshot); err != nil {
				t.Fatal(err)
			}
			got := evaluateHealth(now, snapshot)
			if test.want == "unanswered_human_wait" && len(got) == 1 && !got[0].ConditionSince.Equal(at) {
				t.Fatalf("attempt start=%s want %s", got[0].ConditionSince, at)
			}
			if test.want == "" && len(got) != 0 || test.want != "" && (len(got) != 1 || got[0].Signal != test.want) {
				t.Fatalf("findings=%+v", got)
			}
		})
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO lease_runners(lease_id,runner_id) VALUES('health-lease',?)", r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET routing_settings_json='{}',last_heartbeat_at=? WHERE id=?", formatHubTime(at), r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	runners, err := readHealthRunners(t.Context(), f.service.database.db, f.project.OrganizationID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 1 || runners[0].Leases != 1 {
		t.Fatalf("runners=%+v", runners)
	}
	if _, active := heartbeatFinding(now, runners[0].healthRunner); !active {
		t.Fatal("expired unreleased assignment lost heartbeat gap")
	}
	if runners[0].FreeSlots != 2 {
		t.Fatalf("expired lease occupied capacity: %+v", runners[0])
	}
}

func TestHealthSnapshotConversationQuestions(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "question-health")
	addHealthBacklog(t, f)
	item := f.create(t, "question")
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO conversations(id,organization_id,project_id,owner_principal_id,visibility,status,work_item_id,execution_json,created_at,updated_at) VALUES('conversation-health',?,?,?,'private','active',?,'{}',?,?)`, f.project.OrganizationID, f.project.ID, credential.ID, item.WorkItemID, formatHubTime(at), formatHubTime(at)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO conversation_questions(id,conversation_id,status,created_at,updated_at) VALUES('question-health','conversation-health','pending',?,?)`, formatHubTime(at), formatHubTime(at)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, status string
		expired      bool
		unlinked     bool
		want         int
	}{
		{name: "unanswered", status: "pending", want: 1},
		{name: "answered", status: "answered"},
		{name: "expired", status: "pending", expired: true},
		{name: "unlinked project question", status: "pending", unlinked: true, want: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var expires any
			if test.expired {
				expires = formatHubTime(now.Add(-time.Minute))
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversation_questions SET status=?,expires_at=?", test.status, expires); err != nil {
				t.Fatal(err)
			}
			if test.unlinked {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE conversations SET work_item_id=NULL"); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := healthSnapshot{}
			if err := readHealthCurrentWaits(t.Context(), f.service.database.db, f.project.OrganizationID, now, &snapshot); err != nil {
				t.Fatal(err)
			}
			got := evaluateHealth(now, snapshot)
			if len(got) != test.want {
				t.Fatalf("findings=%+v", got)
			}
			if len(got) > 0 && !got[0].ConditionSince.Equal(at) {
				t.Fatalf("question start=%s want %s", got[0].ConditionSince, at)
			}
			if test.unlinked && got[0].Subject.Kind != "project" {
				t.Fatalf("subject=%+v", got[0].Subject)
			}
		})
	}
}

func TestHealthReadBoundsPreserveEvaluation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		projects      bool
		nonsignal     bool
		incomplete    bool
		laterSignal   string
		candidates    int
		cancellations bool
	}{
		{name: "project bound", projects: true},
		{name: "event bound"},
		{name: "legacy cancellation bound", cancellations: true},
		{name: "ordinary runtime traffic is excluded", nonsignal: true},
		{name: "unknown runtime traffic before refusal", nonsignal: true, incomplete: true, laterSignal: "scheduler.decision"},
		{name: "unknown runtime traffic before landing", nonsignal: true, incomplete: true, laterSignal: "run.finished"},
		{name: "candidate pages", candidates: 101},
		{name: "candidate bound", candidates: healthReadLimit + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "bounded-health")
			f.service.stopHealthDetector()
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			addHealthBacklog(t, f)
			item := f.create(t, "bounded")
			now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
			finding := newHealthFinding("test", "flow", "project", string(f.project.ID), "Summary.", "The operator acts.", []string{string(f.project.ID)}, healthEvidence{})
			applyTestHealth(t, f, now, []healthFinding{finding})
			if test.projects {
				_, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS(SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO projects(id,organization_id,name,profile,created_at) SELECT 'health-project-'||x,?,'health-project-'||x,'native',? FROM n`, healthProjectLimit, f.project.OrganizationID, formatHubTime(now))
				if err != nil {
					t.Fatal(err)
				}
			} else if test.candidates > 0 {
				_, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS(SELECT 2 UNION ALL SELECT x+1 FROM n WHERE x<?)
 INSERT INTO issues(organization_id,project_id,workflow_state_id,number,title,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at)
 SELECT i.organization_id,i.project_id,i.workflow_state_id,n.x,'candidate','','open','1',i.source_updated_at,i.synchronized_at,i.created_at,i.updated_at FROM n CROSS JOIN issues i WHERE i.native_id=?`, test.candidates, item.WorkItemID)
				if err != nil {
					t.Fatal(err)
				}
				if test.candidates <= healthReadLimit {
					snapshot, err := readHealthSnapshot(t.Context(), f.service.database.db, f.project.OrganizationID, now)
					if err != nil {
						t.Fatal(err)
					}
					if len(snapshot.Projects) != 1 {
						t.Fatalf("projects=%d want 1", len(snapshot.Projects))
					}
					if snapshot.Projects[0].Candidates != test.candidates {
						t.Fatalf("projects=%d candidate count=%d want %d", len(snapshot.Projects), snapshot.Projects[0].Candidates, test.candidates)
					}
				}
			} else {
				kind := "scheduler.decision"
				if test.nonsignal {
					kind = "run.observed"
				}
				if test.cancellations {
					kind = "workflow.transitioned"
				}
				_, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS(SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at) SELECT 'health-event-'||x,?,?,?,x+100,?,1,'{"kind":"runner","principal_id":"health-test"}','{}',? FROM n`, healthReadLimit+1, f.project.OrganizationID, f.project.ID, item.WorkItemID, kind, formatHubTime(now))
				if err != nil {
					t.Fatal(err)
				}
				if test.nonsignal && !test.incomplete {
					if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE collaboration_events SET health_projected=1,event_landed=0,analytics_projection_version=1 WHERE id LIKE 'health-event-%'`); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO health_event_times(event_id,organization_id,recorded_at_ns,type,landed) SELECT id,organization_id,?,type,0 FROM collaboration_events WHERE id LIKE 'health-event-%'`, now.UnixNano()); err != nil {
					t.Fatal(err)
				}
			}
			if test.incomplete {
				data := tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Outcome: "skipped", Reason: "runner_capacity_full", Source: "native_claim", RunnerID: "runner_example"}}
				if test.laterSignal != "scheduler.decision" {
					data = tracker.CollaborationData{Run: &tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{Landed: true, ObservedAt: now.Add(30 * time.Second)}}}}
				}
				appendTestHealthEvent(t, f, item, test.laterSignal, data, now.Add(30*time.Second))
				if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM time_index_progress WHERE owner='health'`); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if test.cancellations {
				err = f.service.commitHealthEvaluation(t.Context(), f.project.OrganizationID, now.Add(time.Minute), nil, nil)
			} else {
				err = f.service.evaluateOrganizationHealth(t.Context(), f.project.OrganizationID, now.Add(time.Minute))
			}
			success := test.nonsignal && !test.incomplete || test.candidates > 0 && test.candidates <= healthReadLimit
			if success && err != nil || !success && !errors.Is(err, errHealthReadLimit) {
				t.Fatalf("error=%v", err)
			}
			var resolved sql.NullString
			var tick string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT resolved_at FROM health_findings WHERE fingerprint=?", finding.Fingerprint).Scan(&resolved); err != nil {
				t.Fatal(err)
			}
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT last_tick_at FROM health_detector_ticks").Scan(&tick); err != nil {
				t.Fatal(err)
			}
			expectedTick := now
			if success {
				expectedTick = now.Add(time.Minute)
			}
			if resolved.Valid != success || tick != formatHubTime(expectedTick) {
				t.Fatalf("partial evaluation: resolved=%v tick=%s", resolved, tick)
			}
		})
	}
}

func TestHealthSnapshotRefreshObservation(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "refresh-health")
	addHealthBacklog(t, f)
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	routing, err := json.Marshal(runnerauth.Routing{ProjectIDs: []tracker.ProjectID{f.project.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET routing_settings_json=?,created_at=?,last_heartbeat_at=? WHERE id=?", string(routing), formatHubTime(now.Add(-time.Hour)), formatHubTime(now), r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		refresh  time.Time
		duration time.Duration
		want     bool
	}{
		{"stale refresh", now.Add(-6 * time.Minute), time.Second, true},
		{"slow refresh", now, 6 * time.Minute, true},
		{"healthy refresh", now, time.Second, false},
		{"old runner omits timing", time.Time{}, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := tracker.NativeAdmissionObservation{RunnerRevision: 1, Context: tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: hubTestPolicy().ID, LastRefreshAt: test.refresh, RefreshDuration: test.duration}}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Runner: r.identity}}
			if err := storeRunnerAdmissionObservation(t.Context(), tx, scope, &observation, now); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET admission_facts_ready=NULL WHERE id=?`, r.binding.MachineID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `DELETE FROM runner_admission_observations WHERE runner_id=?`, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			var previous healthSnapshot
			for _, phase := range []string{"legacy", "typed"} {
				if phase == "typed" {
					if err := f.service.database.populateRunnerFacts(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				snapshot, err := readHealthSnapshot(t.Context(), f.service.database.db, f.project.OrganizationID, now)
				if err != nil {
					t.Fatal(err)
				}
				if phase == "legacy" {
					previous = snapshot
				} else if !reflect.DeepEqual(previous, snapshot) {
					t.Fatalf("refresh projection changed snapshot: before=%+v after=%+v", previous, snapshot)
				}

				findings := evaluateHealth(now, snapshot)
				active := false
				for _, finding := range findings {
					if finding.Signal == "scheduler_loop_behind" {
						active = true
					}
				}
				if active != test.want {
					t.Fatalf("active=%v snapshot=%+v", active, snapshot)
				}
			}
		})
	}
}

func TestHealthDetectorChangeContent(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		archived bool
	}{
		{name: "inline output"},
		{name: "archived output", archived: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "health.db")}
			if test.archived {
				objects := &changeObjectFixture{}
				cfg.content = objects.call
			}
			service := openTestService(t, cfg)
			service.stopHealthDetector()
			f := newChangeFixture(t, service)
			addHealthBacklog(t, f.nativeFixture)
			input := changeTestInput()
			input.Validation = &gate.CommandResult{Command: "make check-landing", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 123, Output: "validation succeeded\n"}
			response := performHubAPIRequest(t, service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "health-version"}, ChangeVersionInput: input})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			var outputs int
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM change_command_outputs WHERE version_id=?", version.ID).Scan(&outputs); err != nil {
				t.Fatal(err)
			}
			if (outputs == 1) != test.archived {
				t.Fatalf("archived command outputs=%d", outputs)
			}
			r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat)
			r.enroll(t)
			now := time.Now().UTC()
			at := formatHubTime(now.Add(-time.Hour))
			lease := newNativeID("lease")
			result, err := service.database.db.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,created_at,updated_at) SELECT ?,id,?,'health-session',?,?,?,?,? FROM issues WHERE native_id=?`, lease, r.binding.MachineID, at, at, at, at, at, f.issue.WorkItemID)
			if err != nil {
				t.Fatal(err)
			}
			fence, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(tracker.NativeRunData{Disposition: &tracker.NativeDisposition{Status: "complete"}, Finalization: &tracker.NativeFinalization{Changed: true, ChangeID: f.change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}})
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := json.Marshal(tracker.NativeCheckpoint{WorktreeState: "clean", HeadSHA: version.HeadSHA})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,checkpoint_json,started_at,updated_at,work_item_revision,dispatch_generation) SELECT ?,organization_id,project_id,native_id,?,?,'health-run',1,'succeeded',?,?,?,?,revision,dispatch_generation FROM issues WHERE native_id=?`, newNativeID("attempt"), lease, fence, string(data), string(checkpoint), at, at, f.issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.database.db.ExecContext(t.Context(), "INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)", lease, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			if _, err := service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET last_heartbeat_at=? WHERE id=?", formatHubTime(now.Add(-6*time.Minute)), r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			var original [5]string
			for _, evaluation := range []string{"initial", "repeated"} {
				t.Run(evaluation, func(t *testing.T) {
					if err := service.evaluateOrganizationHealth(service.runDetailContext(t.Context()), f.project.OrganizationID, now); err != nil {
						t.Fatal(err)
					}
					var finding [5]string
					if err := service.database.reader.QueryRowContext(t.Context(), "SELECT id,signal,severity,summary,evidence_json FROM health_findings WHERE organization_id=? AND signal='runner_heartbeat_gap' AND resolved_at IS NULL", f.project.OrganizationID).Scan(&finding[0], &finding[1], &finding[2], &finding[3], &finding[4]); err != nil {
						t.Fatalf("missing stale runner finding: %v", err)
					}
					if evaluation == "initial" {
						original = finding
					} else if finding != original {
						t.Fatalf("repeated evaluation changed finding: got=%v want=%v", finding, original)
					}
				})
			}
		})
	}
}

func TestHealthDetectorTicksAndStops(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		service, err := Open(t.Context(), Config{DatabasePath: filepath.Join(t.TempDir(), "health.db")})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
		}()
		var organization tracker.OrganizationID
		if err := service.database.db.QueryRowContext(t.Context(), "SELECT id FROM organizations WHERE local=1").Scan(&organization); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		for _, minutes := range []int{1, 2} {
			time.Sleep(time.Minute)
			synctest.Wait()
			var tick string
			if err := service.database.db.QueryRowContext(t.Context(), "SELECT last_tick_at FROM health_detector_ticks WHERE organization_id=?", organization).Scan(&tick); err != nil {
				t.Fatal(err)
			}
			if tick != formatHubTime(started.Add(time.Duration(minutes)*time.Minute)) {
				t.Fatalf("tick=%s", tick)
			}
		}
		service.stopHealthDetector()
		synctest.Wait()
		select {
		case <-service.healthDetector.done:
		default:
			t.Fatal("detector did not join shutdown")
		}
	})
}

func TestHealthSnapshotReadsWithoutTheWriter(t *testing.T) {
	f := newNativeFixture(t, nil, "", "health-writer")
	addHealthBacklog(t, f)
	f.create(t, "candidate")
	clearNativeFixturePolicy(t, f)
	writer, err := f.service.database.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := f.service.readHealthSnapshot(testenv.WaitContext(t, t.Context()), f.project.OrganizationID, time.Now().UTC()); err != nil {
		t.Fatalf("health snapshot waited on the held writer: %v", err)
	}
}

func TestHealthSnapshotCloudTransitionDetailAndDependencyHolds(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "cloud-detail-health")
	addHealthBacklog(t, f)
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	waiting, recent, limited, explained := f.create(t, "waiting"), f.create(t, "recent"), f.create(t, "limited"), f.create(t, "explained")
	gated, chained, held, active := f.create(t, "gated"), f.create(t, "chained"), f.create(t, "held"), f.create(t, "active")
	sibling := f.create(t, "sibling")
	appendTestHealthEvent(t, f, waiting, "workflow.transitioned", tracker.CollaborationData{Reason: "worker_progress", ReasonDetail: "permission wait"}, now.Add(-time.Hour))
	appendTestHealthEvent(t, f, recent, "workflow.transitioned", tracker.CollaborationData{Reason: "worker_progress", ReasonDetail: "permission wait"}, now.Add(-time.Minute))
	appendTestHealthEvent(t, f, limited, "workflow.transitioned", tracker.CollaborationData{Reason: "worker_progress", ReasonDetail: "lifetime limit"}, now.Add(-time.Hour))
	appendTestHealthEventAs(t, f, tracker.Actor{Kind: "human", PrincipalID: "operator"}, explained, "workflow.transitioned", tracker.CollaborationData{Reason: "user_requested", ReasonDetail: "lifetime limit"}, now.Add(-time.Hour))
	snapshot := healthSnapshot{}
	if err := readHealthCurrentWaits(t.Context(), f.service.database.db, f.project.OrganizationID, now, &snapshot); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, finding := range evaluateHealth(now, snapshot) {
		got[finding.Subject.ID] = finding.Signal
		if finding.Signal == "unanswered_human_wait" && !finding.ConditionSince.Equal(now.Add(-time.Hour)) {
			t.Fatalf("transition start=%s", finding.ConditionSince)
		}
	}
	want := map[string]string{string(waiting.WorkItemID): "unanswered_human_wait"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings=%v want %v", got, want)
	}

	for i, edge := range []struct{ dependent, blocker tracker.NativeIssue }{{gated, held}, {chained, active}} {
		request := tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprint("edge-", i)}, ExpectedRevision: edge.dependent.Revision, RelatedWorkItemID: edge.blocker.WorkItemID, Operation: "add"}
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(edge.dependent.WorkItemID)+"/dependencies", f.token, request), http.StatusOK)
	}
	request := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "hold"}, ExpectedRevision: held.Revision, State: "Backlog", Reason: "user_requested"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(held.WorkItemID)+"/workflow", f.token, request), http.StatusOK)
	var latestEdge string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT max(created_at) FROM issue_dependencies").Scan(&latestEdge); err != nil {
		t.Fatal(err)
	}
	edgeAt, err := parseTimeValue(latestEdge)
	if err != nil {
		t.Fatal(err)
	}
	heldAt := edgeAt.Add(time.Hour)
	siblingProject := tracker.ProjectID("prj_sibling_health")
	for _, statement := range []string{
		"INSERT INTO projects (id, organization_id, name, profile, states_json, require_dependencies, created_at, github_repository_enabled) SELECT ?, organization_id, 'sibling', profile, states_json, require_dependencies, created_at, github_repository_enabled FROM projects WHERE id = ?",
		"INSERT INTO workflow_states (project_id, source_name, detent_state, terminal, dispatchable, created_at, updated_at) SELECT ?, source_name, detent_state, terminal, dispatchable, created_at, updated_at FROM workflow_states WHERE project_id = ?",
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement, siblingProject, f.project.ID); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	external, err := createNativeFixtureIssueTx(t.Context(), tx, nativeScope{organization: f.project.OrganizationID, project: siblingProject}, tracker.CreateIssue{Title: "external", Body: issueContractTestSections, State: "Backlog"}, heldAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO issue_dependencies (blocker_issue_id, dependent_issue_id, provenance, created_at, updated_at) SELECT b.id, d.id, 'native', ?, ? FROM issues b, issues d WHERE b.native_id=? AND d.native_id=?", formatHubTime(heldAt.Add(time.Hour)), formatHubTime(heldAt.Add(time.Hour)), external.WorkItemID, sibling.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	appendTestHealthEventAs(t, f, tracker.Actor{Kind: "human", PrincipalID: "operator"}, held, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Backlog", Reason: "user_requested"}, heldAt)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	gatedCount, heldCount, blockers, since, err := readHealthDependencyHolds(t.Context(), f.service.database.db, scope)
	if err != nil {
		t.Fatal(err)
	}
	if gatedCount != 3 || heldCount != 2 || !reflect.DeepEqual(blockers, []string{string(held.WorkItemID)}) || !since.Equal(heldAt) {
		t.Fatalf("gated=%d held=%d blockers=%v since=%v want %v", gatedCount, heldCount, blockers, since, heldAt)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET require_dependencies=0 WHERE id=?", f.project.ID); err != nil {
		t.Fatal(err)
	}
	if gatedCount, heldCount, blockers, since, err = readHealthDependencyHolds(t.Context(), f.service.database.db, scope); err != nil || gatedCount != 0 || heldCount != 0 || len(blockers) != 0 || !since.IsZero() {
		t.Fatalf("ignored dependencies gated=%d held=%d blockers=%v since=%v err=%v", gatedCount, heldCount, blockers, since, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET archived=1 WHERE native_id IN (?,?)", active.WorkItemID, external.WorkItemID); err != nil {
		t.Fatal(err)
	}
	window := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: f.service.config.now().Add(24 * time.Hour)}
	for _, live := range []bool{false, true} {
		ctx := t.Context()
		if live {
			ctx = withAnalyticsLiveIssues(ctx)
		}
		_, issues, _, err := readAnalyticsResidenceHistory(ctx, f.service.database.db, scope, window)
		if err != nil {
			t.Fatal(err)
		}
		present := slices.ContainsFunc(issues, func(issue analyticsResidenceIssue) bool { return issue.id == string(active.WorkItemID) })
		if present == live || !slices.ContainsFunc(issues, func(issue analyticsResidenceIssue) bool { return issue.id == string(gated.WorkItemID) }) {
			t.Fatalf("archived present=%v live=%v issues=%d", present, live, len(issues))
		}
	}
}
