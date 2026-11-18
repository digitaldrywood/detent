package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"

	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
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
	var foreignReceipt artifact.Reference
	for _, fixture := range []struct {
		suffix string
		minute int
	}{{"a", 0}, {"b", 4}} {
		t.Run(fixture.suffix, func(t *testing.T) {
			suffix := fixture.suffix
			organization := tracker.OrganizationID("org_" + strings.Repeat(suffix, 32))
			project := tracker.ProjectID("prj_" + strings.Repeat("c", 32))
			cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
			var databaseTime time.Time
			if err := admin.QueryRowContext(t.Context(), `SELECT clock_timestamp()`).Scan(&databaseTime); err != nil {
				t.Fatal(err)
			}
			start := databaseTime.UTC().Truncate(time.Hour).Add(time.Duration(fixture.minute)*time.Minute + 15*time.Second)
			if start.Before(databaseTime) {
				start = start.Add(time.Hour)
			}
			clock := hubtest.NewClock(start)
			cfg.Clock = clock.Now
			cfg.Postgres.LeaseClock = clock.Now
			now := clock.Now()
			cfg.Hosted.OrganizationID = string(organization)
			cfg.SecretKeys = secretTestKeys(t, "1", "1")
			seedHostedStartup(t, cfg)
			service, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			if service.database.postgres == nil || service.config.TenantRepository == nil {
				t.Fatal("required PostgreSQL fixture owners are not configured")
			}
			repository := service.config.TenantRepository
			const tokenID = "credential_runner"
			runnerID := "runner_" + strings.Repeat("e", 32)
			machineID := tracker.MachineID("machine_" + strings.Repeat("f", 32))
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
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: tokenID, Name: "Example runner", Hash: workerHash, Fingerprint: hubapi.TokenFingerprint(workerHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: tokenID, ProjectID: project}); err != nil {
					return err
				}
				for _, role := range []string{"operator", "publisher"} {
					scope := role
					if role == "publisher" {
						scope = "worker"
					}
					id := "credential_" + role + "_boundary"
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: role, Hash: apikey.HashToken("boundary_" + role + "_" + suffix), Scope: scope, NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: id, ProjectID: project}); err != nil {
						return err
					}
				}
				if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_runner", RunnerID: runnerID, MachineID: machineID, TokenHash: apikey.HashToken("example_enrollment_secret"), Scope: "projects", ProjectIDs: []tracker.ProjectID{project}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate}, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
					return err
				}
				if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_runner", runnerID, machineID, now); err != nil {
					return err
				}
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(machineID), Hostname: "example-host", Capacity: 2, Version: "example"}); err != nil {
					return err
				}
				if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: machineID, TokenID: tokenID, EnrollmentID: "enrollment_runner", Scope: "projects", State: "active", Revision: 1, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate}, CapacityLimit: 2, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
					return err
				}

				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: tracker.NativeWorkItemID(reservation.WorkItemID), Number: 1, Revision: 1, Title: "Example issue", WorkflowStateName: new("Todo"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now})
				if err != nil {
					return err
				}
				lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(machineID), SessionID: "session_example", LeaseID: "lease_example", TTL: time.Minute})
				if err != nil {
					return err
				}
				if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization, ProjectID: project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, BoundAt: now}); err != nil {
					return err
				}
				now := lease.RenewedAt
				clock.Set(now)
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
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, "boundary_operator_"+suffix, reservation), http.StatusNotFound)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, "boundary_publisher_"+suffix, reservation), http.StatusNotFound)
			for _, tc := range []struct {
				name   string
				mutate func(*artifact.Reservation)
				status int
			}{
				{"valid", func(*artifact.Reservation) {}, http.StatusNoContent},
				{"replay", func(*artifact.Reservation) {}, http.StatusNoContent},
				{"changed key reservation", func(r *artifact.Reservation) { r.Bytes++ }, http.StatusConflict},
				{"wrong service", func(r *artifact.Reservation) { r.ServiceID = artifact.NewID("service") }, http.StatusNotFound},
				{"wrong project", func(r *artifact.Reservation) { r.ProjectID = artifact.NewID("prj") }, http.StatusNotFound},
				{"unapproved hosted opt in", func(r *artifact.Reservation) { r.HostedOptIn = true }, http.StatusNotFound},
				{"wrong custody", func(r *artifact.Reservation) { r.Mode = "hosted" }, http.StatusNotFound},
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
			for _, tc := range []struct {
				name    string
				expired bool
				status  int
			}{
				{"expires while locking", true, http.StatusConflict},
				{"renewed while locking", false, http.StatusNoContent},
			} {
				t.Run(tc.name, func(t *testing.T) {
					beforeLock := clock.Now()
					requestClock := hubtest.NewClock(beforeLock)
					fixture := &Service{config: service.config, database: service.database, echo: echo.New()}
					fixture.config.Clock = requestClock.Now
					fixture.config.TenantRepository = artifactLeaseClockRepository{Repository: repository, afterLock: func(ctx context.Context, w tenantstore.Writer, lease leasestore.Lease) (leasestore.Lease, error) {
						if tc.expired {
							requestClock.Set(lease.ExpiresAt)
							return lease, nil
						}
						renewAt := beforeLock.Add(30 * time.Second)
						clock.Set(renewAt)
						requestClock.Set(renewAt)
						renewed, err := w.Renew(ctx, leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, time.Minute)
						if err == nil && (!renewed.RenewedAt.Equal(renewAt) || !renewed.ExpiresAt.After(lease.ExpiresAt)) {
							t.Errorf("renewed lease interval = [%v, %v), want renewal at %v extending expiry past %v", renewed.RenewedAt, renewed.ExpiresAt, renewAt, lease.ExpiresAt)
						}
						return renewed, err
					}}
					fixture.registerArtifactRoutes(fixture.echo)
					response := httptest.NewRecorder()
					fixture.echo.ServeHTTP(response, newHubAPIRequest(t, http.MethodPost, path, workerToken, reservation))
					requireNativeStatus(t, response, tc.status)
					if !requestClock.Now().After(beforeLock) {
						t.Fatal("request did not reach the lease lock")
					}
				})
			}

			t.Run("authorization bound", func(t *testing.T) {
				now := clock.Now()
				rows := func(expired bool) int {
					t.Helper()
					var count int
					if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM artifact_upload_authorizations WHERE organization_id=$1 AND (expires_at<=$2)=$3`, organization, now, expired).Scan(&count); err != nil {
						t.Fatal(err)
					}
					return count
				}
				flood := reservation
				flood.Kind = "screenshot"
				for i := range artifactstore.UploadAuthorizationLimit - 1 {
					flood.Key = fmt.Sprintf("flood-%d", i)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, flood), http.StatusNoContent)
				}
				flood.Key = "flood-over"
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, flood), http.StatusTooManyRequests)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, reservation), http.StatusNoContent)
				if got := rows(false); got != artifactstore.UploadAuthorizationLimit {
					t.Fatalf("live authorizations=%d, want %d", got, artifactstore.UploadAuthorizationLimit)
				}
				if _, err := admin.ExecContext(t.Context(), `UPDATE artifact_upload_authorizations SET created_at=CURRENT_TIMESTAMP-interval '2 hours',expires_at=CURRENT_TIMESTAMP-interval '1 hour' WHERE organization_id=$1 AND kind='screenshot'`, organization); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_upload_authorizations(organization_id,project_id,service_id,idempotency_key,work_item_id,run_id,attempt_id,kind,bytes,created_at,expires_at) SELECT $1,$2,$3,'backlog-'||n,$4,$5,$6,'video',1,CURRENT_TIMESTAMP-interval '2 hours',CURRENT_TIMESTAMP-interval '1 hour' FROM generate_series(1,300) n`, organization, project, reservation.ServiceID, reservation.WorkItemID, reservation.RunID, reservation.AttemptID); err != nil {
					t.Fatal(err)
				}
				backlog := rows(true)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, flood), http.StatusNoContent)
				if got, want := rows(true), backlog-artifactstore.UploadAuthorizationPruneBatch; got != want {
					t.Fatalf("expired authorizations after bounded request cleanup=%d, want %d", got, want)
				}
				otherExpired := func() int {
					t.Helper()
					var count int
					if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM artifact_upload_authorizations WHERE organization_id<>$1 AND expires_at<=$2`, organization, now).Scan(&count); err != nil {
						t.Fatal(err)
					}
					return count
				}
				if err := service.expireNativeRecords(t.Context(), now); err != nil {
					t.Fatal(err)
				}
				if got := rows(true); got != 0 {
					t.Fatalf("expired authorizations after background prune=%d", got)
				}
				if got := otherExpired(); got != 0 {
					t.Fatalf("background prune left %d expired rows in other tenants", got)
				}
				if got := rows(false); got != 2 {
					t.Fatalf("live authorizations after expiry=%d, want 2", got)
				}
				if _, err := admin.ExecContext(t.Context(), `DELETE FROM artifact_upload_authorizations WHERE organization_id=$1 AND kind='screenshot'`, organization); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_upload_authorizations(organization_id,project_id,service_id,idempotency_key,work_item_id,run_id,attempt_id,kind,bytes,created_at,expires_at) SELECT $1,$2,$3,'kept-'||n,$4,$5,$6,'video',1,$7::timestamptz-interval '2 hours',$7::timestamptz-interval '1 hour' FROM generate_series(1,7) n`, organization, project, reservation.ServiceID, reservation.WorkItemID, reservation.RunID, reservation.AttemptID, now); err != nil {
					t.Fatal(err)
				}
			})

			t.Run("cross-attempt key", func(t *testing.T) {
				now := clock.Now()
				attacker := reservation
				attacker.WorkItemID, attacker.AttemptID, attacker.RunID = artifact.NewID("wi"), artifact.NewID("attempt"), artifact.NewID("run")
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: tracker.NativeWorkItemID(attacker.WorkItemID), Number: 2, Revision: 1, Title: "Attacker issue", WorkflowStateName: new("Todo"), CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now})
					if err != nil {
						return err
					}
					lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(machineID), SessionID: "session_attacker", LeaseID: "lease_attacker", TTL: time.Minute})
					if err != nil {
						return err
					}
					if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization, ProjectID: project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, BoundAt: now}); err != nil {
						return err
					}
					if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
						return err
					}
					attacker.LeaseID, attacker.FencingToken = lease.LeaseID, lease.FencingToken
					now = lease.RenewedAt
					clock.Set(now)
					_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: attacker.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: attacker.RunID, Sequence: 1, Status: "running", StartedAt: now, PayloadObjectKey: "example/attacker"})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				attackerVersion, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(attacker.WorkItemID), RecordID: attacker.WorkItemID, Revision: 1}, []byte(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.CreateCollaborationVersion(t.Context(), attackerVersion)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				const crossPublisher = "credential_cross_publisher"
				crossToken := "cross_publisher_" + string(organization)
				bindPublisher := func(id string) {
					t.Helper()
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: reservation.ServiceID, Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: id})
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: crossPublisher, Name: "Example cross publisher", Hash: apikey.HashToken(crossToken), Scope: "worker", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
					return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: crossPublisher, ProjectID: project})
				}); err != nil {
					t.Fatal(err)
				}
				victim := reservation
				victim.Key, victim.Kind = reservation.AttemptID+":diff", "diff"
				attacker.Key, attacker.Kind = victim.Key, victim.Kind
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, victim), http.StatusNoContent)
				receipt := artifact.Reference{SchemaVersion: 1, Scope: victim.Scope, ServiceID: victim.ServiceID, ArtifactID: artifact.NewID("artifact"), ManifestID: artifact.NewID("manifest"), Revision: 1, SHA256: artifact.Digest([]byte("partial")), Kind: "diff", State: "partial", Availability: "available", Bytes: 5, Objects: 1, ObservedAt: now, ExpiresAt: now.Add(time.Hour)}
				receiptPath := "/api/v2/organizations/" + string(organization) + "/projects/" + string(project) + "/artifact-services/" + victim.ServiceID + "/receipts"
				bindPublisher(crossPublisher)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, receiptPath, crossToken, receipt), http.StatusNoContent)
				bindPublisher(tokenID)
				attackerPath := "/api/v2/organizations/" + string(organization) + "/projects/" + string(project) + "/work-items/" + attacker.WorkItemID + "/artifact-authority"
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, attackerPath, workerToken, attacker), http.StatusNoContent)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, victim), http.StatusNoContent)
				foreignAttempt := attacker
				foreignAttempt.AttemptID, foreignAttempt.RunID = victim.AttemptID, victim.RunID
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, attackerPath, workerToken, foreignAttempt), http.StatusNotFound)
				if _, err := admin.ExecContext(t.Context(), `DELETE FROM artifact_upload_authorizations WHERE organization_id=$1 AND idempotency_key=$2`, organization, victim.Key); err != nil {
					t.Fatal(err)
				}
			})

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
			t.Run("receipts", func(t *testing.T) {
				clock.Set(clock.Now().Truncate(time.Minute).Add(time.Minute))
				foreignReceipt = testPostgresArtifactReceipts(t, admin, service, reservation, foreignReceipt, workerToken, clock)
			})
		})
	}
}

func testPostgresArtifactReceipts(t *testing.T, admin *sql.DB, service *Service, reservation artifact.Reservation, foreign artifact.Reference, workerToken string, clock *hubtest.Clock) artifact.Reference {
	t.Helper()
	now := clock.Now()
	organization, project := tracker.OrganizationID(reservation.OrganizationID), tracker.ProjectID(reservation.ProjectID)
	const publisherID = "credential_publisher"
	publisherToken := "publisher_" + string(organization)
	repository := service.config.TenantRepository
	service.database.hostedRuntime.Store(&hostedplans.RuntimeConfig{Plans: hostedplans.HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
	plan := billingstore.PlanReference{ID: "artifact_usage", Version: 1}
	err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: organization, PlanReference: plan, Name: "Artifact usage", Allowances: map[string]int64{"api_mutations": 2}}); err != nil {
			return err
		}
		current, err := w.BillingAssignment(t.Context())
		if err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization, Base: plan}, current.Revision); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: publisherID, Name: "Example publisher", Hash: apikey.HashToken(publisherToken), Scope: "worker", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: publisherID, ProjectID: project}); err != nil {
			return err
		}
		return w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: reservation.ServiceID, Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: publisherID})
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/organizations/" + string(organization) + "/projects/" + string(project) + "/artifact-services/" + reservation.ServiceID + "/receipts"
	reference := artifact.Reference{SchemaVersion: 1, Scope: reservation.Scope, ServiceID: reservation.ServiceID, ArtifactID: "artifact_" + strings.Repeat("e", 32), ManifestID: artifact.NewID("manifest"), Revision: 1, SHA256: artifact.Digest([]byte("example")), Kind: "log", State: "complete", Availability: "available", Bytes: 10, Objects: 1, ObservedAt: now, ExpiresAt: now.Add(time.Hour)}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, workerToken, reference), http.StatusForbidden)
	for _, tc := range []struct {
		name   string
		mutate func(*artifact.Reference)
		status int
	}{
		{"terminal attempt", func(*artifact.Reference) {}, http.StatusNoContent},
		{"replay", func(*artifact.Reference) {}, http.StatusNoContent},
		{"immutable conflict", func(r *artifact.Reference) { r.Bytes++ }, http.StatusConflict},
		{"foreign scope", func(r *artifact.Reference) { r.OrganizationID = artifact.NewID("org") }, http.StatusUnprocessableEntity},
		{"wrong run", func(r *artifact.Reference) { r.RunID = artifact.NewID("run") }, http.StatusNotFound},
		{"wrong item", func(r *artifact.Reference) { r.WorkItemID = artifact.NewID("wi") }, http.StatusNotFound},
		{"wrong attempt", func(r *artifact.Reference) { r.AttemptID = artifact.NewID("attempt") }, http.StatusNotFound},
		{"unauthorized artifact", func(r *artifact.Reference) {
			r.ArtifactID, r.ManifestID = artifact.NewID("artifact"), artifact.NewID("manifest")
		}, http.StatusNotFound},
		{"unauthorized kind", func(r *artifact.Reference) {
			r.ArtifactID, r.ManifestID, r.Kind = artifact.NewID("artifact"), artifact.NewID("manifest"), "diff"
		}, http.StatusNotFound},
		{"unauthorized artifact sorted first", func(r *artifact.Reference) {
			r.ArtifactID, r.ManifestID = "artifact_"+strings.Repeat("0", 32), artifact.NewID("manifest")
		}, http.StatusNotFound},
		{"missing version", func(r *artifact.Reference) { r.VersionID = artifact.NewID("version") }, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := reference
			tc.mutate(&request)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisherToken, request), tc.status)
		})
	}
	if foreign.OrganizationID != "" {
		foreign.OrganizationID, foreign.ProjectID, foreign.WorkItemID = reference.OrganizationID, reference.ProjectID, reference.WorkItemID
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisherToken, foreign), http.StatusNotFound)
	}
	content := eventstore.NewContent(service.runDetailStore())
	for _, tc := range []struct {
		name         string
		attempt      string
		otherProject bool
		status       int
	}{
		{"matching version", reference.AttemptID, false, http.StatusNoContent},
		{"other attempt version", artifact.NewID("attempt"), false, http.StatusNotFound},
		{"other project version", reference.AttemptID, true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			versionProject := project
			if tc.otherProject {
				versionProject = tracker.ProjectID(artifact.NewID("prj"))
			}
			changeItem := tracker.NativeWorkItemID(reference.WorkItemID)
			if tc.otherProject {
				changeItem = tracker.NativeWorkItemID(artifact.NewID("wi"))
			}
			changeID, versionID := artifact.NewID("change"), artifact.NewID("version")
			raw, err := json.Marshal(tracker.ChangeRequest{OrganizationID: organization, ProjectID: versionProject, WorkItemID: changeItem, ID: changeID, Title: "Example", Revision: 1, CreatedAt: now, UpdatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			change, err := content.PrepareChange(t.Context(), organization, raw)
			if err != nil {
				t.Fatal(err)
			}
			input := changeTestInput()
			input.AttemptID = tc.attempt
			raw, err = json.Marshal(tracker.ChangeVersion{ID: versionID, ChangeID: changeID, Number: 1, ChangeVersionInput: input, Policy: hubTestPolicy(), CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			version, err := content.PrepareVersion(t.Context(), organization, raw)
			if err != nil {
				t.Fatal(err)
			}
			err = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if tc.otherProject {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: versionProject, Name: "Other project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
						return err
					}
				}
				if tc.otherProject {
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: versionProject, NativeID: changeItem, Number: 1, Revision: 1, Title: "Other item"}); err != nil {
						return err
					}
				}

				if _, err := w.CreateChange(t.Context(), change); err != nil {
					return err
				}
				_, err := w.CreateVersion(t.Context(), version)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			request := reference
			request.VersionID, request.Revision, request.ManifestID = versionID, 2, artifact.NewID("manifest")
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisherToken, request), tc.status)
		})
	}
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.ArtifactReference(t.Context(), project, tracker.NativeWorkItemID(reference.WorkItemID), reference.ArtifactID, 1)
		if err != nil {
			return err
		}
		if stored.Bytes != reference.Bytes || stored.AttemptID != reference.AttemptID || stored.ManifestID != reference.ManifestID {
			t.Fatalf("receipt changed: %+v", stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Run("reference without legacy database", func(t *testing.T) {
		reader := &Service{config: service.config}
		response := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), response)
		c.SetParamNames("item", "artifact", "revision")
		c.SetParamValues(reference.WorkItemID, reference.ArtifactID, "1")
		c.Set("native_scope", hubscope.NativeScope{Organization: organization, Project: project})
		if err := reader.getArtifactReference(c); err != nil {
			t.Fatal(err)
		}
		if response.Code != http.StatusOK {
			t.Fatalf("reference status = %d", response.Code)
		}
		var stored artifact.Reference
		if err := json.Unmarshal(response.Body.Bytes(), &stored); err != nil {
			t.Fatal(err)
		}
		if stored.ArtifactID != reference.ArtifactID || stored.Revision != 1 {
			t.Fatalf("reference = %+v", stored)
		}
		bindings, err := reader.artifactServicesCommand(t.Context(), hubscope.NativeScope{Organization: organization, Project: project})
		if err != nil || len(bindings) != 1 || bindings[0].PublisherTokenID != "" {
			t.Fatalf("artifact service bindings = %+v, error = %v", bindings, err)
		}
		references, err := reader.readArtifactReferences(t.Context(), hubscope.NativeScope{Organization: organization, Project: project}, reference.WorkItemID)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, current := range references {
			if current.ArtifactID == reference.ArtifactID {
				found = true
				if current.Bytes != reference.Bytes || current.SHA256 != reference.SHA256 {
					t.Fatalf("artifact reference changed: %+v", current)
				}
			}
		}
		if !found {
			t.Fatal("artifact reference missing")
		}

	})
	window := time.Unix(now.Unix()/60*60, 0).UTC()
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		used, err := r.BillingUsage(t.Context(), window, "api_mutations")
		if err != nil {
			return err
		}
		if used != 2 {
			t.Errorf("receipt mutations = %d, want 2", used)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	denied := reference
	denied.Revision, denied.ManifestID = 3, artifact.NewID("manifest")
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisherToken, denied), http.StatusTooManyRequests)
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.ArtifactReference(t.Context(), project, tracker.NativeWorkItemID(reference.WorkItemID), reference.ArtifactID, 3)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Errorf("denied receipt persisted: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Run("later window replay", func(t *testing.T) {
		replayAt := now.Add(2 * time.Minute)
		clock.Set(replayAt)
		defer clock.Set(now)
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisherToken, reference), http.StatusNoContent)
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			usages, err := r.BillingUsages(t.Context(), replayAt, 64)
			if len(usages) != 0 {
				t.Errorf("replay charged a new window: %+v", usages)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})

	err = repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.BindArtifactService(t.Context(), project, artifact.Binding{ServiceID: reservation.ServiceID, Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: "credential_runner"})
	})
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, publisherToken, reference), http.StatusNotFound)
	return reference
}

type artifactLeaseClockRepository struct {
	tenantstore.Repository
	afterLock artifactLeaseLockHook
}

type artifactLeaseLockHook func(context.Context, tenantstore.Writer, leasestore.Lease) (leasestore.Lease, error)

func (r artifactLeaseClockRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error {
		return write(artifactLeaseClockWriter{Writer: w, afterLock: r.afterLock})
	})
}

type artifactLeaseClockWriter struct {
	tenantstore.Writer
	afterLock artifactLeaseLockHook
}

func (w artifactLeaseClockWriter) LockLease(ctx context.Context, id string) (leasestore.Lease, error) {
	lease, err := w.Writer.(leasestore.FencedLeaseLocker).LockLease(ctx, id)
	if err != nil {
		return lease, err
	}
	return w.afterLock(ctx, w.Writer, lease)
}

type postgresArtifactFixture struct {
	hostedSecurityFixture
	admin *sql.DB
}

func newPostgresArtifactFixture(t *testing.T, configure ...func(*Config)) postgresArtifactFixture {
	t.Helper()
	cfg := Config{RunDetails: &memoryRunDetails{}, Hosted: &hostedplans.HostedConfig{
		OrganizationID:       "org_" + strings.Repeat("a", 32),
		WorkOSOrganizationID: "org_provider",
		BootstrapSubject:     "user_owner",
		PublicURL:            "https://hub.example.test",
		StaffEmails:          []string{"staff@example.test", "support@example.test"},
		SupportActors:        []string{"support@example.test"},
		Provider:             newHostedSecurityProvider(),
	}, SecretKeys: secretTestKeys(t, "1", "1")}
	for _, apply := range configure {
		apply(&cfg)
	}
	service := openHostedPostgresService(t, cfg)
	org := service.accessOrganization()
	project := tracker.ProjectID("prj_" + strings.Repeat("b", 32))
	if err := service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return postgresArtifactFixture{hostedSecurityFixture: hostedSecurityFixture{service: service, provider: cfg.Hosted.Provider.(*hostedSecurityProvider), project: project, base: "/api/v2/organizations/" + string(org) + "/projects/" + string(project)}, admin: hostedPostgresAdmin(t, service)}
}

func (f postgresArtifactFixture) user(t *testing.T, name, role, email, grant, support string) hostedSecurityUser {
	t.Helper()
	user := f.hostedSecurityFixture.user(t, name, role, email, "", support)
	org := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.DeleteAccessTokenGrant(t.Context(), member.PrincipalID, f.project); err != nil {
			return err
		}
		if grant == "" {
			return w.DeleteAccessProjectGrant(t.Context(), member.UserID, f.project)
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: member.UserID, ProjectID: f.project, CanWrite: grant == "write"}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
	return user
}

func (f postgresArtifactFixture) grant(t *testing.T, user hostedSecurityUser, write, runner bool) {
	t.Helper()
	org := f.service.accessOrganization()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: member.UserID, ProjectID: f.project, CanWrite: write, ManageRunner: runner}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
}

func seedPostgresHostedArtifact(t *testing.T, f postgresArtifactFixture) (artifact.Reference, string, string) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	org := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
	item := f.seedIssue(t, 1)
	publisher := "artifact-publisher-token"
	binding := artifact.Binding{ServiceID: artifact.NewID("service"), Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: "artifact-publisher"}
	ref := artifact.Reference{SchemaVersion: 1, Scope: artifact.Scope{OrganizationID: string(org), ProjectID: string(f.project), WorkItemID: string(item), RunID: artifact.NewID("run"), AttemptID: artifact.NewID("attempt")}, ServiceID: binding.ServiceID, ArtifactID: artifact.NewID("artifact"), ManifestID: artifact.NewID("manifest"), Revision: 1, SHA256: artifact.Digest([]byte("manifest")), Kind: "log", State: "partial", Availability: "available", Bytes: 100, Objects: 1, ExpiresAt: now.Add(time.Hour), ObservedAt: now}
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
	err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: binding.PublisherTokenID, Name: "Example publisher", Hash: apikey.HashToken(publisher), Scope: "worker", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: binding.PublisherTokenID, ProjectID: f.project}); err != nil {
			return err
		}
		if err := w.BindArtifactService(t.Context(), f.project, binding); err != nil {
			return err
		}
		_, err := w.AppendArtifactReference(t.Context(), ref)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return ref, publisher, f.base + "/artifact-services/" + binding.ServiceID
}
