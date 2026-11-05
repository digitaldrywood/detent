package hubserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
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

func TestPostgresArtifactUploadAuthority(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	var foreign artifact.Reservation
	for _, suffix := range []string{"a", "b"} {
		t.Run(suffix, func(t *testing.T) {
			organization := tracker.OrganizationID("org_" + strings.Repeat(suffix, 32))
			project := tracker.ProjectID("prj_" + strings.Repeat("c", 32))
			cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
			now := time.Now().UTC()
			cfg.Clock = func() time.Time { return now }
			cfg.Hosted.OrganizationID = string(organization)
			cfg.SecretKeys = secretTestKeys(t, "1", "1")
			service, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			if service.database.db != nil || service.database.reader != nil {
				t.Fatal("legacy database is not nil")
			}
			repository := service.config.TenantRepository
			seedStartupOrganization(t, repository, organization)
			const tokenID = "credential_runner"
			const runnerID = "runner_example"
			const machineID tracker.MachineID = "machine_example"
			workerToken := "example_runner_token_" + suffix
			workerHash := apikey.HashToken(workerToken)
			expires := now.Add(time.Hour)
			reservation := artifact.Reservation{Scope: artifact.Scope{OrganizationID: string(organization), ProjectID: string(project), WorkItemID: artifact.NewID("wi"), AttemptID: artifact.NewID("attempt"), RunID: artifact.NewID("run")}, ServiceID: "service_" + strings.Repeat("d", 32), Mode: "customer", Key: "example", Kind: "log", Bytes: 10}
			descriptor := hubTestPolicy()
			policyRevision, err := eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), organization, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			policyRevision.ProjectID, policyRevision.ApprovedBy, policyRevision.ApprovedAt = project, "example", now
			err = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				if err := w.ApproveProjectPolicy(t.Context(), policyRevision, ""); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: tokenID, Name: "Example runner", Hash: workerHash, Fingerprint: tokenFingerprint(workerHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: tokenID, ProjectID: project}); err != nil {
					return err
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_runner", RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_runner", runnerID, machineID, now); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(machineID), Hostname: "example-host", Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: machineID, TokenID: tokenID, EnrollmentID: "enrollment_runner", Scope: "projects", State: "active", Revision: 1, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate}, CapacityLimit: 2, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
					return err
				}

				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: tracker.NativeWorkItemID(reservation.WorkItemID), Number: 1, Revision: 1, Title: "Example issue", WorkflowStateName: new("Todo"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now})
				if err != nil {
					return err
				}
				lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(machineID), SessionID: "session_example", LeaseID: "lease_example", TTL: time.Hour})
				if err != nil {
					return err
				}
				if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization, ProjectID: project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, BoundAt: now}); err != nil {
					return err
				}
				now = lease.RenewedAt
				reservation.LeaseID, reservation.FencingToken = lease.LeaseID, lease.FencingToken
				if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
					return err
				}
				if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: reservation.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: reservation.RunID, Sequence: 1, Status: "running", StartedAt: now, PayloadObjectKey: "example/start"}); err != nil {
					return err
				}
				return w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: reservation.ServiceID, Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: tokenID})
			})
			if err != nil {
				t.Fatal(err)
			}
			version, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(reservation.WorkItemID), RecordID: reservation.WorkItemID, Revision: 1}, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateCollaborationVersion(t.Context(), version)
				return err
			}); err != nil {
				t.Fatal(err)
			}

			path := "/api/v2/organizations/" + string(organization) + "/projects/" + string(project) + "/work-items/" + reservation.WorkItemID + "/artifact-authority"
			for _, tc := range []struct {
				name   string
				mutate func(*artifact.Reservation)
				status int
			}{
				{"valid", func(*artifact.Reservation) {}, http.StatusNoContent},
				{"wrong run", func(r *artifact.Reservation) { r.RunID = artifact.NewID("run") }, http.StatusNotFound},
				{"wrong attempt", func(r *artifact.Reservation) { r.AttemptID = artifact.NewID("attempt") }, http.StatusNotFound},
				{"stale fence", func(r *artifact.Reservation) { r.FencingToken++ }, http.StatusConflict},
				{"binding mismatch", func(r *artifact.Reservation) { r.Mode = "hosted"; r.HostedOptIn = true }, http.StatusNotFound},
			} {
				t.Run(tc.name, func(t *testing.T) {
					request := reservation
					tc.mutate(&request)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, request), tc.status)
				})
			}
			if foreign.OrganizationID != "" {
				foreign.OrganizationID, foreign.ProjectID, foreign.WorkItemID = reservation.OrganizationID, reservation.ProjectID, reservation.WorkItemID
				foreign.LeaseID, foreign.FencingToken = reservation.LeaseID, reservation.FencingToken
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, foreign), http.StatusNotFound)
			}
			foreign = reservation
			err = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: reservation.LeaseID, FencingToken: reservation.FencingToken}, "completed")
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, reservation), http.StatusConflict)
		})
	}
}
