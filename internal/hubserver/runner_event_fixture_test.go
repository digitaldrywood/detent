package hubserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func seedRunnerEventTenant(t *testing.T, service *Service, template runnerPGFixture, org tracker.OrganizationID) runnerPGFixture {
	t.Helper()
	seedStartupOrganization(t, service.config.TenantRepository, org)
	f := runnerPGFixture{admin: template.admin, service: service}
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(time.Hour)
	descriptor := hubTestPolicy()
	policy := descriptor.WithID()
	var err error
	f.policy, err = eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), org, policy)
	if err != nil {
		t.Fatal(err)
	}
	f.policy.ProjectID = "prj_runner"
	f.policy.ApprovedAt, f.policy.ApprovedBy = now, "test_hub_admin"
	err = service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: "hub_" + strings.Repeat("a", 32), CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "prj_runner", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		if err := w.ApproveProjectPolicy(t.Context(), f.policy, ""); err != nil {
			return err
		}
		for index := 1; index <= 2; index++ {
			id := strconv.Itoa(index)
			token, err := apikey.GenerateToken()
			if err != nil {
				return err
			}
			binding := template.bindings[index-1]
			f.bindings = append(f.bindings, binding)
			runnerID, machineID, credentialID := binding.RunnerID, string(binding.MachineID), "credential_"+id
			f.tokens = append(f.tokens, token)
			hash := apikey.HashToken(token)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credentialID, Name: "Example runner " + id, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credentialID, ProjectID: "prj_runner"}); err != nil {
				return err
			}
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: "enrollment_" + id, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenHash: apikey.HashToken("example_enrollment_" + id), Scope: "projects", ProjectIDs: []tracker.ProjectID{"prj_runner"}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_"+id, runnerID, tracker.MachineID(machineID), now); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: machineID, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenID: credentialID, EnrollmentID: "enrollment_" + id, Scope: "projects", State: "active", Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{"prj_runner"}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
				return err
			}
			state := "Todo"
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_runner", NativeID: tracker.NativeWorkItemID("wi_" + id), Number: int64(index), Title: "Example", WorkflowStateName: &state})
			if err != nil {
				return err
			}
			itemRaw, err := json.Marshal(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_runner", WorkItemID: item.NativeID, Number: int(item.Number), Revision: tracker.Revision(item.Revision)}, Title: "Example", State: "Todo"})
			if err != nil {
				return err
			}
			itemVersion, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: "prj_runner", WorkItemID: item.NativeID, RecordID: string(item.NativeID), Revision: item.Revision}, itemRaw)
			if err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), itemVersion); err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machineID, SessionID: "execution_" + id, LeaseID: "lease_" + id, TTL: time.Hour})
			if err != nil {
				return err
			}
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
				return err
			}
			if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: org, ProjectID: "prj_runner", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: policy.ID, BoundAt: now}); err != nil {
				return err
			}
			f.leases = append(f.leases, tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: item.NativeID, MachineID: tracker.MachineID(machineID), SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: policy.ID, ExpiresAt: lease.ExpiresAt})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, lease := range f.leases {
		data := tracker.NativeRunData{AttemptID: newNativeID("attempt"), RunID: newNativeID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: policy.ID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "example-model"}}
		data.AttemptID, data.RunID = template.data[index].AttemptID, template.data[index].RunID
		f.data = append(f.data, data)
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start_" + data.AttemptID}, Type: "run.started", SchemaVersion: 1, Data: data}
		path := "/api/v2/organizations/" + string(org) + "/projects/prj_runner/work-items/" + string(lease.WorkItemID) + "/events"
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, f.tokens[index], event), http.StatusOK)
	}
	return f
}
