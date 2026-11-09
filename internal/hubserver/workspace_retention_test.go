package hubserver

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresWorkspaceRetainsOnlyItsSubjectAttemptRunner(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	first := newWorkspaceHostedNativeService(t, Config{now: func() time.Time { return now }, Workspace: &WorkspaceConfig{Enabled: true, maintenanceTicks: make(chan time.Time)}})
	second := openWorkspaceTenantHub(t, first.config, workspaceTestDB(t, first), "org_first_second")
	hubs := []*Service{first, second}
	type retainedAttempt struct {
		record workspaceRecord
		runner string
	}
	saved := make([][]retainedAttempt, len(hubs))
	for tenant, hub := range hubs {
		fixture := newNativeFixture(t, hub, tracker.OrganizationID(hub.localOrganization), "Workspace retention project")
		policy := hubTestPolicy()
		approveHubTestPolicy(t, hub, fixture.base+"/policy", policy)
		for index := range 2 {
			subject := fixture.create(t, fmt.Sprintf("Workspace attempt subject %d", index))
			runner := prepareRunner(t, fixture, runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events)
			runner.redemption.Capacity = 4
			runner.enroll(t)
			worker := runner.redemption.Credential
			heartbeat := fixture.base + "/machines/" + string(runner.binding.MachineID) + "/heartbeat"
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, heartbeat, worker, map[string]any{"display_name": "Example runner", "capacity": 4, "version": "test", "capacity_configuration": workspaceTestCapacity(4, now)}), http.StatusOK)
			claim := tracker.NativeClaim{PolicyID: policy.ID, WorkItemID: subject.WorkItemID, MachineID: runner.binding.MachineID, SessionID: newNativeID("session"), TTLSeconds: 600, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
			response := performHubAPIRequest(t, hub, http.MethodPost, fixture.base+"/claims", worker, claim)
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			attempt, run := newNativeID("attempt"), newNativeID("run")
			identity := &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}
			path := fixture.base + "/work-items/" + string(subject.WorkItemID) + "/events"
			started := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("start")}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 1, Identity: identity, LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: run, AttemptID: attempt, PolicyID: policy.ID}}
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path, "invalid_workspace_credential", started), http.StatusUnauthorized)
			requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path, worker, started), http.StatusOK)
			if index == 1 {
				finished := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: newNativeID("finish")}, Type: "run.finished", SchemaVersion: 1, Data: tracker.NativeRunData{Sequence: 2, Identity: identity, LeaseID: lease.ID, FencingToken: lease.FencingToken, RunID: run, AttemptID: attempt, PolicyID: policy.ID, Outcome: "succeeded"}}
				requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPost, path, worker, finished), http.StatusOK)
			}
			saved[tenant] = append(saved[tenant], retainedAttempt{record: workspaceRecord{OrganizationID: fixture.project.OrganizationID, ProjectID: fixture.project.ID, SubjectWorkItemID: string(subject.WorkItemID), AttemptID: attempt}, runner: runner.binding.RunnerID})
		}
	}
	check := func(hub *Service, organization tracker.OrganizationID, record workspaceRecord, at time.Time, want string, wantErr error) {
		t.Helper()
		err := hub.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			got, err := retainedWorkspaceRunnerRecord(t.Context(), r.(workspaceReader), record, time.Hour, at)
			if !errors.Is(err, wantErr) {
				t.Errorf("retained runner error=%v, want %v", err, wantErr)
			}
			if got != want {
				t.Errorf("retained runner=%q, want %q", got, want)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for reopen := range 2 {
		for tenant, records := range saved {
			if reopen == 1 {
				hubs[tenant] = reopenWorkspaceHub(t, hubs[tenant])
			}
			hub := hubs[tenant]
			for index, entry := range records {
				organization := entry.record.OrganizationID
				check(hub, organization, entry.record, now.Add(time.Hour-time.Nanosecond), entry.runner, nil)
				want := entry.runner
				if index == 1 {
					want = ""
				}
				check(hub, organization, entry.record, now.Add(time.Hour), want, nil)
				wrongSubject := entry.record
				wrongSubject.SubjectWorkItemID = records[1-index].record.SubjectWorkItemID
				check(hub, organization, wrongSubject, now, "", corestore.ErrNotFound)
				foreign := saved[1-tenant][index].record
				check(hub, organization, foreign, now, "", corestore.ErrNotFound)
			}
		}
	}
}
