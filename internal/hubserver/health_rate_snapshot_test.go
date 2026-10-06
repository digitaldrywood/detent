package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedHealthRateAttempt(t *testing.T, tx *sql.Tx, scope nativeScope, item tracker.NativeIssue, machine string, status string, data tracker.NativeRunData, at time.Time, cost float64) string {
	t.Helper()
	leaseID, attemptID := newNativeID("lease"), newNativeID("attempt")
	var itemID int64
	if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id=?", item.WorkItemID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	result, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, leaseID, itemID, machine, leaseID, formatHubTime(at.Add(time.Minute)), formatHubTime(at), formatHubTime(at), formatHubTime(at), formatHubTime(at), formatHubTime(at))
	if err != nil {
		t.Fatal(err)
	}
	fence, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,started_at,updated_at) VALUES(?,?,?,?,?,?,?,1,?,?,?,?)`, attemptID, scope.organization, scope.project, item.WorkItemID, leaseID, fence, attemptID, status, string(raw), formatHubTime(at.Add(-time.Minute)), formatHubTime(at)); err != nil {
		t.Fatal(err)
	}
	if cost >= 0 {
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO attempt_usage(attempt_id,organization_id,project_id,period,provider,model,cost_estimate,updated_at) VALUES(?,?,?,?,?,?,?,?)`, attemptID, scope.organization, scope.project, formatHubTime(at.Truncate(time.Hour)), "codex", "test", cost, formatHubTime(at)); err != nil {
			t.Fatal(err)
		}
	}
	return attemptID
}

func TestHealthRatePersistedObservations(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 6, 18, 0, 0, 0, time.UTC)
	f := newNativeFixture(t, nil, "", "health-rate-observations")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "health-rate-other")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	r.enroll(t)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	states := nativeFixtureStates()
	states = append(states, tracker.NativeState{Name: "Rework", Dispatchable: true}, tracker.NativeState{Name: "Merging", Dispatchable: true}, tracker.NativeState{Name: "Backlog"})
	if err := applyNativeProjectStates(t.Context(), tx, scope, states, now.Add(-8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	create := func(title string, at time.Time) tracker.NativeIssue {
		issue, err := createNativeIssueDraft(t.Context(), tx, scope, tracker.CreateIssue{Title: title, State: "Todo"}, at, false)
		if err != nil {
			t.Fatal(err)
		}
		return issue
	}
	for i := range 24 {
		at := now.Add(-time.Duration(7+6*i) * time.Hour)
		issue := create(fmt.Sprint("baseline-", i), at.Add(-40*time.Minute))
		for j, lane := range []string{"In Progress", "Rework", "Merging", "Done"} {
			from := issue.State
			issue.State = lane
			issue, err = persistNativeIssue(t.Context(), tx, scope, issue, "workflow.transitioned", tracker.CollaborationData{FromState: from, ToState: lane}, at.Add(-time.Duration(3-j)*10*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
		}
		seedHealthRateAttempt(t, tx, scope, issue, string(r.binding.MachineID), "succeeded", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{HeartbeatAt: at, Landing: &tracker.NativeLandingReceipt{Landed: true, ObservedAt: at}}}, at, 4)
	}
	storm := create("storm", now.Add(-4*time.Hour))
	for i := range 5 {
		seedHealthRateAttempt(t, tx, scope, storm, string(r.binding.MachineID), "failed", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: fmt.Sprintf("protocol count %d failed", i)}}, now.Add(-time.Duration(i+1)*time.Minute), 1.25)
	}
	conflict := create("single landing conflict", now.Add(-time.Hour))
	seedHealthRateAttempt(t, tx, scope, conflict, string(r.binding.MachineID), "interrupted", tracker.NativeRunData{}, now.Add(-33*time.Minute), 0)
	seedHealthRateAttempt(t, tx, scope, conflict, string(r.binding.MachineID), "succeeded", tracker.NativeRunData{}, now.Add(-2*time.Minute), 6.00108)
	seedHealthRateAttempt(t, tx, scope, conflict, string(r.binding.MachineID), "succeeded", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{FromState: "Merging", TargetState: "Rework", RefusalKind: "conflict", ObservedAt: now.Add(-time.Minute)}}}, now.Add(-time.Minute), 0)
	seedHealthRateAttempt(t, tx, scope, conflict, string(r.binding.MachineID), "running", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{HeartbeatAt: now.Add(-time.Second)}}, now.Add(-time.Second), 0)
	recovery := create("recovery", now.Add(-4*time.Hour))
	for i := range 3 {
		seedHealthRateAttempt(t, tx, scope, recovery, string(r.binding.MachineID), "interrupted", tracker.NativeRunData{TerminalFailure: &tracker.NativeTerminalFailure{Error: "native checkpoint requires recovery: checkpoint_unavailable"}}, now.Add(-time.Duration(i+1)*time.Minute), 0.5)
	}
	stalled := create("stalled", now.Add(-4*time.Hour))
	stalled.State = "In Progress"
	stalled, err = persistNativeIssue(t.Context(), tx, scope, stalled, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "In Progress"}, now.Add(-3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	stalledAttempt := seedHealthRateAttempt(t, tx, scope, stalled, string(r.binding.MachineID), "running", tracker.NativeRunData{Runtime: &tracker.NativeRuntimeObservation{HeartbeatAt: now.Add(-3 * time.Hour)}}, now.Add(-3*time.Hour), 1)
	costly := create("costly", now.Add(-4*time.Hour))
	seedHealthRateAttempt(t, tx, scope, costly, string(r.binding.MachineID), "running", tracker.NativeRunData{}, now.Add(-time.Minute), 45)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.evaluateOrganizationHealth(t.Context(), scope.organization, now); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/health/findings", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page healthFindingsPage
	decodeHubResponse(t, response, &page)
	for _, signal := range []string{"retry_storm", "recovery_loop", "stalled_active_item", "cost_anomaly"} {
		if !slices.ContainsFunc(page.Items, func(f healthFinding) bool { return f.Signal == signal }) {
			t.Fatalf("missing %s: %+v", signal, page)
		}
	}
	for _, finding := range page.Items {
		if finding.Signal == "retry_storm" && (finding.Evidence.Values["wasted_cost_usd"] != 6.25 || len(finding.Evidence.AttemptIDs) != 5 || !slices.Equal(finding.Evidence.Signatures, []string{"protocol count <n> failed"})) {
			t.Fatalf("lost normalized usage evidence: %+v", finding)
		}
	}
	if !slices.ContainsFunc(page.BaselineUnavailable, func(g healthBaselineUnavailable) bool { return g.Signal == "throughput_collapse" }) || slices.ContainsFunc(page.BaselineUnavailable, func(g healthBaselineUnavailable) bool { return g.Signal == "retry_storm" || g.Signal == "cost_anomaly" }) {
		t.Fatalf("baseline gaps=%+v", page.BaselineUnavailable)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/health/findings", other.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var otherPage healthFindingsPage
	decodeHubResponse(t, response, &otherPage)
	if len(otherPage.Items) != 0 || len(otherPage.BaselineUnavailable) != 9 {
		t.Fatalf("cross-project findings or missing diagnostics: %+v", otherPage)
	}
	for _, gap := range otherPage.BaselineUnavailable {
		if gap.Project != string(other.project.ID) {
			t.Fatal("baseline diagnostics escaped scope")
		}
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET data_json=json_set(data_json,'$.runtime.heartbeat_at',?),updated_at=? WHERE id=?`, formatHubTime(now), formatHubTime(now), stalledAttempt); err != nil {
		t.Fatal(err)
	}
	if err := f.service.evaluateOrganizationHealth(t.Context(), scope.organization, now.Add(6*time.Hour)); err != nil {
		t.Fatal(err)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/health/findings?state=resolved", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &page)
	for _, signal := range []string{"retry_storm", "recovery_loop"} {
		if !slices.ContainsFunc(page.Items, func(f healthFinding) bool { return f.Signal == signal && f.ResolvedAt != nil }) {
			t.Fatalf("rate did not resolve: %s %+v", signal, page)
		}
	}
}
