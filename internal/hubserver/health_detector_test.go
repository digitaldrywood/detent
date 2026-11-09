package hubserver

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func healthFixtureDatabase(t *testing.T, f nativeFixture) *sql.DB {
	t.Helper()
	value, ok := projectRecordDatabases.Load(f.service.database.postgres)
	if !ok {
		t.Fatal("PostgreSQL fixture database is missing")
	}
	return value.(*sql.DB)
}

func TestHealthSnapshotRunnerAndPolicyFallback(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "runner-health")
	addHealthBacklog(t, f)
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
	r.enroll(t)
	f.create(t, "candidate")
	admin := healthFixtureDatabase(t, f)
	if _, err := admin.ExecContext(t.Context(), `DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2`, f.project.OrganizationID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET native_created_at=$1 WHERE organization_id=$2 AND project_id=$3`, now.Add(-time.Hour), f.project.OrganizationID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=$1,created_at=$2 WHERE organization_id=$3 AND runner_id=$4`, now.Add(-6*time.Minute), now.Add(-time.Hour), f.project.OrganizationID, r.binding.RunnerID); err != nil {
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
			if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=$1 WHERE organization_id=$2 AND runner_id=$3`, test.heartbeat, f.project.OrganizationID, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			if test.policy {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			}
			snapshot, err := f.service.readHealthSnapshotRecords(t.Context(), f.project.OrganizationID, now)
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
	raw, err := json.Marshal(tracker.CollaborationEvent{ID: newNativeID("event"), OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateID: item.WorkItemID, Type: kind, SchemaVersion: 1, Actor: actor, RecordedAt: at, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	event, err := eventstore.NewContent(f.service.runDetailStore()).PrepareEvent(t.Context(), f.project.OrganizationID, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestHealthSnapshotCurrentWaitAndLimitAuthority(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "wait-health")
	addHealthBacklog(t, f)
	var states []tracker.NativeState
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		project, err := r.ProjectRecord(t.Context(), f.project.ID)
		states = project.States
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(states, func(state tracker.NativeState) bool { return state.Name == "Blocked" }) {
		states = append(states, tracker.NativeState{Name: "Blocked", Transitions: []string{"Todo"}})
	}
	for i := range states {
		if states[i].Name == "Todo" && !slices.Contains(states[i].Transitions, "Blocked") {
			states[i].Transitions = append(states[i].Transitions, "Blocked")
		}
	}
	setHealthProjectStates(t, f, states)
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	item := f.create(t, "waiting")
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	at := now.Add(-time.Hour)
	data := tracker.NativeRunData{Disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true, ReasonCode: "permission_wait"}}
	currentAttempt := seedHealthAttemptRecord(t, f, item, string(r.binding.MachineID), "succeeded", data, at, -1)
	appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Revision: item.Revision, Reason: "worker_progress"}, at)
	for _, test := range []struct {
		name                            string
		supersede, limit, action, typed bool
		want                            string
	}{
		{name: "permission action still authoritative", want: "unanswered_human_wait"},
		{name: "typed permission action still authoritative", typed: true, want: "unanswered_human_wait"},
		{name: "human action transition precedes final attempt update", action: true, want: "unanswered_human_wait"},
		{name: "new human revision supersedes attempt", supersede: true},
		{name: "lifetime transition remains current", limit: true},
		{name: "recovered lifetime transition"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.typed {
				for _, filter := range []string{"human actor", "user reason", "old revision", "complete disposition", "no human action"} {
					modified := data
					disposition := *data.Disposition
					modified.Disposition = &disposition
					transition := tracker.CollaborationData{Revision: item.Revision, Reason: "worker_progress"}
					actor := tracker.Actor{Kind: "runner", PrincipalID: "runner-health"}
					switch filter {
					case "human actor":
						actor.Kind = "human"
					case "user reason":
						transition.Reason = "user_requested"
					case "old revision":
						transition.Revision = 0
					case "complete disposition":
						disposition.Status = "complete"
					case "no human action":
						disposition.HumanAction, disposition.ReasonCode = false, "user_requested"
					}
					currentAttempt = seedHealthAttemptRecord(t, f, item, string(r.binding.MachineID), "succeeded", modified, at, -1)
					var attempt leasestore.Attempt
					if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(reader tenantstore.Reader) error {
						var err error
						attempt, err = reader.Attempt(t.Context(), currentAttempt)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					stale := data
					stale.AttemptID, stale.RunID, stale.Outcome, stale.Sequence = currentAttempt, attempt.RunID, "succeeded", 1
					appendTestHealthEvent(t, f, item, "run.finished", tracker.CollaborationData{Run: &stale}, at)
					appendTestHealthEventAs(t, f, actor, item, "workflow.transitioned", transition, at)
					snapshot := healthSnapshot{}
					readErr := f.service.readHealthWaitRecords(t.Context(), f.project.OrganizationID, now, &snapshot)
					if readErr != nil || len(snapshot.Waits) != 0 {
						t.Fatalf("typed authority filter %s: waits=%+v err=%v", filter, snapshot.Waits, readErr)
					}
				}
				currentAttempt = seedHealthAttemptRecord(t, f, item, string(r.binding.MachineID), "succeeded", data, at, -1)
				appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Revision: item.Revision, Reason: "worker_progress"}, at)
			}
			if test.action {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "health-permission-state"}, ExpectedRevision: item.Revision, State: "Blocked", Reason: "user_requested"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &item)
				action := tracker.NativeRunData{Disposition: &tracker.NativeDisposition{Status: "blocked", HumanAction: true}}
				currentAttempt = seedHealthAttemptRecord(t, f, item, string(r.binding.MachineID), "succeeded", action, at.Add(time.Second), -1)
				appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Revision: item.Revision, ToState: "Blocked", Reason: "worker_progress", ReasonDetail: "human_action"}, at)
				defer func() {
					currentAttempt = seedHealthAttemptRecord(t, f, item, string(r.binding.MachineID), "succeeded", data, at, -1)
					appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Revision: item.Revision, Reason: "worker_progress"}, at)
				}()
			}
			if test.supersede {
				title := "Human revision supersedes attempt"
				response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(item.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "health-human-revision"}, ExpectedRevision: item.Revision, Title: &title})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &item)
			}
			if test.limit {
				appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Reason: "lifetime_limit"}, now.Add(-25*time.Hour))
			}
			if test.name == "recovered lifetime transition" {
				appendTestHealthEvent(t, f, item, "workflow.transitioned", tracker.CollaborationData{Reason: "lifetime_limit_recovered"}, now)
			}
			snapshot := healthSnapshot{}
			if err := f.service.readHealthWaitRecords(t.Context(), f.project.OrganizationID, now, &snapshot); err != nil {
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
	admin := healthFixtureDatabase(t, f)
	if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=NULL WHERE organization_id=$1 AND lease_id=(SELECT lease_id FROM native_attempts WHERE organization_id=$1 AND id=$2)`, f.project.OrganizationID, currentAttempt); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `DELETE FROM token_grants WHERE organization_id=$1 AND token_id=(SELECT token_id FROM runner_identities WHERE organization_id=$1 AND runner_id=$2)`, f.project.OrganizationID, r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET last_heartbeat_at=$1 WHERE organization_id=$2 AND runner_id=$3`, at, f.project.OrganizationID, r.binding.RunnerID); err != nil {
		t.Fatal(err)
	}
	runners, err := f.service.readHealthRunnerRecords(t.Context(), f.project.OrganizationID, now)
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
	var principal string
	if err := f.service.config.AccessRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		credential, err := r.CredentialByHash(t.Context(), apikey.HashToken(f.token))
		principal = credential.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	admin := healthFixtureDatabase(t, f)
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	at := now.Add(-time.Hour)
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversations(id,organization_id,project_id,owner_principal_id,origin,visibility,status,work_item_id,created_at,updated_at) VALUES('conversation-health',$1,$2,$3,'user','private','active',$4,$5,$5)`, f.project.OrganizationID, f.project.ID, principal, item.WorkItemID, at); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO conversation_questions(organization_id,id,conversation_id,status,created_at,updated_at) VALUES($1,'question-health','conversation-health','pending',$2,$2)`, f.project.OrganizationID, at); err != nil {
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
				expires = now.Add(-time.Minute)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE conversation_questions SET status=$1,expires_at=$2 WHERE organization_id=$3 AND conversation_id='conversation-health'`, test.status, expires, f.project.OrganizationID); err != nil {
				t.Fatal(err)
			}
			if test.unlinked {
				if _, err := admin.ExecContext(t.Context(), `UPDATE conversations SET work_item_id=NULL WHERE organization_id=$1 AND id='conversation-health'`, f.project.OrganizationID); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := healthSnapshot{}
			if err := f.service.readHealthWaitRecords(t.Context(), f.project.OrganizationID, now, &snapshot); err != nil {
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
	admin := healthFixtureDatabase(t, f)
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	if _, err := admin.ExecContext(t.Context(), `UPDATE runner_identities SET created_at=$1,last_heartbeat_at=$2 WHERE organization_id=$3 AND runner_id=$4`, now.Add(-time.Hour), now, f.project.OrganizationID, r.binding.RunnerID); err != nil {
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
			observation := tracker.NativeAdmissionObservation{RunnerRevision: 1, ReceivedAt: now, Context: tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: hubTestPolicy().ID, LastRefreshAt: test.refresh, RefreshDuration: test.duration}}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				return w.ReplaceRunnerAdmissionObservation(t.Context(), r.binding.RunnerID, f.project.ID, observation)
			}); err != nil {
				t.Fatal(err)
			}
			var previous healthSnapshot
			for _, phase := range []string{"first read", "cached read"} {
				snapshot, err := f.service.readHealthSnapshotRecords(t.Context(), f.project.OrganizationID, now)
				if err != nil {
					t.Fatal(err)
				}
				if phase == "first read" {
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
