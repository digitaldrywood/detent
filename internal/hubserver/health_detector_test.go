package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

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
