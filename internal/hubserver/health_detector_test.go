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
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Core.RequireDependencies = true
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now().UTC().Truncate(time.Microsecond)
	waiting, recent, limited, explained := f.create(t, "waiting"), f.create(t, "recent"), f.create(t, "limited"), f.create(t, "explained")
	gated, chained, held, active := f.create(t, "gated"), f.create(t, "chained"), f.create(t, "held"), f.create(t, "active")
	sibling := f.create(t, "sibling")
	appendTestHealthEvent(t, f, waiting, "workflow.transitioned", tracker.CollaborationData{Reason: "worker_progress", ReasonDetail: "permission wait"}, now.Add(-time.Hour))
	appendTestHealthEvent(t, f, recent, "workflow.transitioned", tracker.CollaborationData{Reason: "worker_progress", ReasonDetail: "permission wait"}, now.Add(-time.Minute))
	appendTestHealthEvent(t, f, limited, "workflow.transitioned", tracker.CollaborationData{Reason: "worker_progress", ReasonDetail: "lifetime limit"}, now.Add(-time.Hour))
	appendTestHealthEventAs(t, f, tracker.Actor{Kind: "human", PrincipalID: "operator"}, explained, "workflow.transitioned", tracker.CollaborationData{Reason: "user_requested", ReasonDetail: "lifetime limit"}, now.Add(-time.Hour))
	snapshot := healthSnapshot{}
	if err := f.service.readHealthWaitRecords(t.Context(), f.project.OrganizationID, now, &snapshot); err != nil {
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
	admin := healthFixtureDatabase(t, f)
	var edgeAt time.Time
	if err := admin.QueryRowContext(t.Context(), `SELECT max(created_at) FROM issue_dependencies WHERE organization_id=$1 AND dependent_project_id=$2`, f.project.OrganizationID, f.project.ID).Scan(&edgeAt); err != nil {
		t.Fatal(err)
	}
	heldAt := edgeAt.Add(time.Hour)
	siblingFixture := newNativeFixture(t, f.service, f.project.OrganizationID, "Sibling health")
	addHealthBacklog(t, siblingFixture)
	response := performHubAPIRequest(t, f.service, http.MethodPost, siblingFixture.base+"/work-items", siblingFixture.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "external-health"}, Title: "External dependency", Body: issueContractTestSections, State: "Backlog"})
	requireNativeStatus(t, response, http.StatusOK)
	var external tracker.NativeIssue
	decodeHubResponse(t, response, &external)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, WorkItemID: sibling.WorkItemID, RelatedProjectID: siblingFixture.project.ID, RelatedWorkItemID: external.WorkItemID, Type: "dependency"}, "add", heldAt.Add(time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	appendTestHealthEventAs(t, f, tracker.Actor{Kind: "human", PrincipalID: "operator"}, held, "workflow.transitioned", tracker.CollaborationData{FromState: "Todo", ToState: "Backlog", Reason: "user_requested"}, heldAt)
	holds, err := f.service.readHealthDependencyRecords(t.Context(), f.project.OrganizationID, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if holds.DependencyGated != 3 || holds.HeldCount != 2 || !reflect.DeepEqual(holds.HeldBlockers, []string{string(held.WorkItemID)}) || !holds.HoldSince.Equal(heldAt) {
		t.Fatalf("gated=%d held=%d blockers=%v since=%v want %v", holds.DependencyGated, holds.HeldCount, holds.HeldBlockers, holds.HoldSince, heldAt)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Core.RequireDependencies = false
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	holds, err = f.service.readHealthDependencyRecords(t.Context(), f.project.OrganizationID, f.project.ID)
	if err != nil || holds.DependencyGated != 0 || holds.HeldCount != 0 || len(holds.HeldBlockers) != 0 || !holds.HoldSince.IsZero() {
		t.Fatalf("ignored dependencies gated=%d held=%d blockers=%v since=%v err=%v", holds.DependencyGated, holds.HeldCount, holds.HeldBlockers, holds.HoldSince, err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET archived=true WHERE organization_id=$1 AND native_id=ANY($2::text[])`, f.project.OrganizationID, []string{string(active.WorkItemID), string(external.WorkItemID)}); err != nil {
		t.Fatal(err)
	}
	query := reportstore.OverviewQuery{Projects: []tracker.ProjectID{f.project.ID}, From: now.Add(-24 * time.Hour), To: f.service.config.now().Add(24 * time.Hour), Limit: 5000}
	for _, live := range []bool{false, true} {
		var ids []tracker.NativeWorkItemID
		if live {
			snapshot, err := f.service.readHealthSnapshotRecords(t.Context(), f.project.OrganizationID, query.To)
			if err != nil {
				t.Fatal(err)
			}
			for _, project := range snapshot.Rates {
				if project.ID == string(f.project.ID) {
					for _, timeline := range project.Timelines {
						ids = append(ids, tracker.NativeWorkItemID(timeline.issue.id))
					}
				}
			}
		} else {
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(r tenantstore.Reader) error {
				owner, ok := r.(reportstore.HistoryReader)
				if !ok {
					return workItemStorageUnavailable()
				}
				history, err := owner.ReadHistory(t.Context(), query)
				if err != nil {
					return err
				}
				for _, project := range history {
					for _, issue := range project.Issues {
						ids = append(ids, issue.WorkItemID)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		present := slices.Contains(ids, active.WorkItemID)
		if present == live || !slices.Contains(ids, gated.WorkItemID) {
			t.Fatalf("archived present=%v live=%v issues=%d", present, live, len(ids))
		}
	}
}
