package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func applyTestHealth(t *testing.T, f nativeFixture, now time.Time, findings []healthFinding) {
	t.Helper()
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.service.commitHealthEvaluation(t.Context(), tx, f.project.OrganizationID, now, findings); err != nil {
		t.Fatal(err)
	}
}

func TestHealthFindingLifecycle(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "health-lifecycle")
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	finding := newHealthFinding("test", "instance", "project", string(f.project.ID), "One sentence.", "The operator acts.", []string{string(f.project.ID)}, healthEvidence{Counts: map[string]int{"candidates": 1}})
	var original string
	for _, test := range []struct {
		name   string
		offset time.Duration
		active bool
		rows   int
		reused bool
		seen   time.Duration
	}{
		{"opens", 0, true, 1, true, 0},
		{"updates last seen", time.Minute, true, 1, true, time.Minute},
		{"resolves", 2 * time.Minute, false, 1, true, time.Minute},
		{"reopens same record at sixty minutes", 62 * time.Minute, true, 1, true, 62 * time.Minute},
		{"resolves again", 63 * time.Minute, false, 1, true, 62 * time.Minute},
		{"later recurrence opens new record", 124 * time.Minute, true, 2, false, 124 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			findings := []healthFinding{}
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
			if test.reused && opened != formatHubTime(now) {
				t.Fatalf("opened_at changed: %s", opened)
			}
		})
	}
}

func TestHealthDetectorEmptyTenantAndObservationOnly(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "empty-health")
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
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatalf("detector changed work item %s", item.WorkItemID)
	}
}

func TestHealthFindingsReads(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "health-reads")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-health")
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
		{"invalid state", f.base + "/health/findings?state=all", http.StatusUnprocessableEntity, 0},
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
	if !strings.Contains(string(result.Content), "last_tick_at") {
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
}

func TestHealthSnapshotRunnerAndPolicyFallback(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "runner-health")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	r.enroll(t)
	f.create(t, "candidate")
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
		{"policy mismatch", now, false, "every_candidate_refused"},
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
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	actor := tracker.Actor{Kind: "runner", PrincipalID: "runner-health"}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, sourceActor: &actor}
	if err := appendNativeHistory(t.Context(), tx, scope, string(item.WorkItemID), kind, data, at); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestHealthSnapshotRecordedCyclesAndLandings(t *testing.T) {
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
		{name: "different reasons", reasons: []string{"policy_mismatch", "capacity_full"}},
		{name: "partial cycle", reasons: []string{"policy_mismatch"}},
		{name: "claimed cycle", reasons: []string{"policy_mismatch", "policy_mismatch"}, claims: true},
		{name: "outside history window", reasons: []string{"policy_mismatch", "policy_mismatch"}, old: true},
		{name: "landing observation", landing: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "recorded-health")
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
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	item := f.create(t, "waiting")
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
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
		want      string
	}{
		{name: "permission action still authoritative", want: "unanswered_human_wait"},
		{name: "new human revision supersedes attempt", supersede: true},
		{name: "lifetime transition remains current", limit: true, want: "lifetime_limit_hit"},
		{name: "recovered lifetime transition", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			if test.unlinked && got[0].Subject.Kind != "project" {
				t.Fatalf("subject=%+v", got[0].Subject)
			}
		})
	}
}

func TestHealthReadBoundsPreserveEvaluation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		projects   bool
		nonsignal  bool
		candidates int
	}{
		{name: "project bound", projects: true},
		{name: "event bound"},
		{name: "ordinary runtime traffic is excluded", nonsignal: true},
		{name: "candidate pages", candidates: 101},
		{name: "candidate bound", candidates: healthReadLimit + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "bounded-health")
			item := f.create(t, "bounded")
			now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
			finding := newHealthFinding("test", "instance", "project", string(f.project.ID), "Summary.", "The operator acts.", []string{string(f.project.ID)}, healthEvidence{})
			applyTestHealth(t, f, now, []healthFinding{finding})
			if test.projects {
				_, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS(SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO projects(id,organization_id,name,profile,created_at) SELECT 'health-project-'||x,?,'health-project-'||x,'native',? FROM n`, healthProjectLimit, f.project.OrganizationID, formatHubTime(now))
				if err != nil {
					t.Fatal(err)
				}
			} else if test.candidates > 0 {
				_, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS(SELECT 3 UNION ALL SELECT x+1 FROM n WHERE x<?)
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
				_, err := f.service.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS(SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<?) INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at) SELECT 'health-event-'||x,?,?,?,x+100,?,1,'{"kind":"runner","principal_id":"health-test"}','{}',? FROM n`, healthReadLimit+1, f.project.OrganizationID, f.project.ID, item.WorkItemID, kind, formatHubTime(now))
				if err != nil {
					t.Fatal(err)
				}
			}
			err := f.service.evaluateOrganizationHealth(t.Context(), f.project.OrganizationID, now.Add(time.Minute))
			success := test.nonsignal || test.candidates > 0 && test.candidates <= healthReadLimit
			if success && err != nil || !success && !errors.Is(err, errHealthReadLimit) {
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
			snapshot, err := readHealthSnapshot(t.Context(), f.service.database.db, f.project.OrganizationID, now)
			if err != nil {
				t.Fatal(err)
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
	f.create(t, "candidate")
	writer, err := f.service.database.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := f.service.readHealthSnapshot(ctx, f.project.OrganizationID, time.Now().UTC()); err != nil {
		t.Fatalf("health snapshot waited on the held writer: %v", err)
	}
}
