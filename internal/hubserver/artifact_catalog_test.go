package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type catalogJourneyProject struct {
	id          tracker.ProjectID
	reservation artifact.Reservation
}
type catalogJourneyArtifact struct {
	project   tracker.ProjectID
	upload    artifact.Upload
	object    artifact.Object
	reference artifact.Reference
	part      artifact.Part
}
type catalogJourneyPublisher struct {
	id        string
	token     string
	serviceID string
	mode      string
	config    artifact.Config
	remote    *artifact.RemoteHub
	service   *artifact.Service
	server    *httptest.Server
	handler   atomic.Pointer[artifact.HTTPServer]
	records   []catalogJourneyArtifact
	abandoned artifact.Upload
}

type catalogJourneyTransport struct {
	base *http.Transport
	drop *atomic.Bool
}

func (t catalogJourneyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(r)
	if err == nil && strings.HasSuffix(r.URL.Path, "/receipts") && t.drop.CompareAndSwap(true, false) {
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		return nil, errors.Join(io.ErrUnexpectedEOF, readErr, closeErr)
	}
	return response, err
}

func TestPostgresArtifactCatalogJourney(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	storage := &pilotArtifactStorage{}
	var foreignPublisher string
	for range 2 {
		org := tracker.OrganizationID(artifact.NewID("org"))
		var clock atomic.Value
		clock.Store(time.Now().UTC().Truncate(time.Microsecond))
		now := func() time.Time { return clock.Load().(time.Time) }
		cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
		cfg.Hosted.OrganizationID = string(org)
		cfg.Hosted.WorkOSOrganizationID = "provider_" + string(org)
		cfg.SecretKeys = secretTestKeys(t, "1", "1")
		cfg.Clock = now
		open := func() *Service {
			service, err := Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			service.database.hostedRuntime.Store(&hostedRuntimeConfig{Plans: HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}})
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			return service
		}
		hub := open()
		seedStartupOrganization(t, hub.config.TenantRepository, org)
		projects := []catalogJourneyProject{{id: tracker.ProjectID(artifact.NewID("prj"))}, {id: tracker.ProjectID(artifact.NewID("prj"))}}
		publishers := []*catalogJourneyPublisher{{id: artifact.NewID("credential"), token: artifact.NewID("token"), serviceID: artifact.NewID("service"), mode: "customer"}, {id: artifact.NewID("credential"), token: artifact.NewID("token"), serviceID: artifact.NewID("service"), mode: "hosted"}}
		for _, publisher := range publishers {
			current := publisher
			current.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler := current.handler.Load()
				if handler == nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(current.server.Close)
		}
		workerID, workerToken := artifact.NewID("credential"), artifact.NewID("token")
		adminID, adminToken := artifact.NewID("credential"), artifact.NewID("token")
		machine := tracker.MachineID(artifact.NewID("machine"))
		runner := artifact.NewID("runner")
		enrollment := artifact.NewID("enrollment")
		expiry := now().Add(48 * time.Hour)
		descriptor := hubTestPolicy()
		adminMembership, err := cfg.Hosted.Provider.CreateMembership(t.Context(), adminID, cfg.Hosted.WorkOSOrganizationID, "admin")
		if err != nil {
			t.Fatal(err)
		}
		err = hub.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			for _, credential := range []accessstore.Credential{{OrganizationID: org, ID: workerID, Name: workerID, Hash: apikey.HashToken(workerToken), Scope: "worker", NativeOnly: true, CreatedAt: now(), UpdatedAt: now(), ExpiresAt: &expiry, ProjectAccess: "selected", Kind: "service"}, {OrganizationID: org, ID: adminID, Name: adminID, Hash: apikey.HashToken(adminToken), Scope: "operator", NativeOnly: true, UserID: adminID, MembershipID: adminMembership.ID, KeyScope: "admin", CreatedAt: now(), UpdatedAt: now(), ExpiresAt: &expiry, ProjectAccess: "all", Kind: "service"}} {
				if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
					return err
				}
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: adminID, Email: "admin@example.test", MembershipID: adminMembership.ID, Role: "admin", Active: true, PrincipalID: adminID, CreatedAt: now(), UpdatedAt: now()}); err != nil {
				return err
			}
			for _, publisher := range publishers {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: publisher.id, Name: publisher.id, Hash: apikey.HashToken(publisher.token), Scope: "worker", NativeOnly: true, CreatedAt: now(), UpdatedAt: now(), ExpiresAt: &expiry, ProjectAccess: "selected", Kind: "service"}); err != nil {
					return err
				}
			}
			plan := billingstore.PlanReference{ID: "artifact_journey", Version: 1}
			if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: org, PlanReference: plan, Name: "Example", Features: []string{"hosted_artifacts"}, Allowances: map[string]int64{"api_mutations": 100000, "api_requests": 100000, "hosted_artifact_services": 10, "relay_bytes": 1 << 30, "artifact_retained_bytes": 64 << 20, "artifact_reserved_bytes": 32 << 20, "artifact_bytes": 8 << 20, "artifact_upload_bytes": artifact.MaxChunkBytes, "artifact_retention_seconds": 3600}}); err != nil {
				return err
			}
			if err := w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: org, Base: plan}, 0); err != nil {
				return err
			}
			for _, project := range projects {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project.id, Name: string(project.id), Slug: strings.ReplaceAll(string(project.id), "_", "-"), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				for _, id := range []string{workerID, adminID, publishers[0].id, publishers[1].id} {
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: id, ProjectID: project.id}); err != nil {
						return err
					}
				}
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: adminID, ProjectID: project.id, CanWrite: true}); err != nil {
					return err
				}
				for _, publisher := range publishers {
					if err := w.BindArtifactService(t.Context(), project.id, artifact.Binding{ServiceID: publisher.serviceID, Origin: publisher.server.URL, Mode: publisher.mode, HostedOptIn: publisher.mode == "hosted", PublisherTokenID: publisher.id}); err != nil {
						return err
					}
				}
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: enrollment, RunnerID: runner, MachineID: machine, TokenHash: apikey.HashToken(artifact.NewID("token")), Scope: "projects", ProjectIDs: []tracker.ProjectID{projects[0].id, projects[1].id}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate}, CreatedAt: now(), ExpiresAt: expiry, CreatedBy: adminID}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), enrollment, runner, machine, now()); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: string(machine), Hostname: "example", Capacity: 2, Version: "example"}); err != nil {
				return err
			}
			return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: runner, MachineID: machine, TokenID: workerID, EnrollmentID: enrollment, Scope: "projects", State: "active", Revision: 1, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Events, runnerauth.Collaborate}, CapacityLimit: 2, ReportedCapacity: 2, ProjectIDs: []tracker.ProjectID{projects[0].id, projects[1].id}, CreatedAt: now(), ExpiresAt: expiry, LastHeartbeatAt: now()})
		})
		if err != nil {
			t.Fatal(err)
		}
		for i := range projects {
			project := &projects[i]
			approved, err := eventstore.NewContent(hub.runDetailStore()).PrepareProjectPolicy(t.Context(), org, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			approved.ProjectID, approved.ApprovedBy, approved.ApprovedAt = project.id, adminID, now()
			reservation := artifact.Reservation{Scope: artifact.Scope{OrganizationID: string(org), ProjectID: string(project.id), WorkItemID: artifact.NewID("wi"), AttemptID: artifact.NewID("attempt"), RunID: artifact.NewID("run")}, Key: "example", Kind: "log", Bytes: 1 << 20}
			err = hub.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.ApproveProjectPolicy(t.Context(), approved, ""); err != nil {
					return err
				}
				item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project.id, NativeID: tracker.NativeWorkItemID(reservation.WorkItemID), Number: 1, Revision: 1, Title: "Example", WorkflowStateName: new("Todo"), CreatedAt: now(), UpdatedAt: now(), NativeCreatedAt: now(), NativeUpdatedAt: now()})
				if err != nil {
					return err
				}
				lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(machine), SessionID: artifact.NewID("session"), LeaseID: artifact.NewID("lease"), TTL: time.Minute})
				if err != nil {
					return err
				}
				clock.Store(lease.RenewedAt)
				reservation.LeaseID, reservation.FencingToken = lease.LeaseID, lease.FencingToken
				if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: org, ProjectID: project.id, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, BoundAt: now()}); err != nil {
					return err
				}
				if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: lease.LeaseID, RunnerID: runner, AssignedAt: now()}); err != nil {
					return err
				}
				_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: reservation.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: reservation.RunID, Sequence: 1, Status: "running", StartedAt: now(), PayloadObjectKey: "example/start"})
				return err
			})
			if err != nil {
				t.Fatal(err)
			}

			version, err := eventstore.NewContent(hub.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project.id, WorkItemID: tracker.NativeWorkItemID(reservation.WorkItemID), RecordID: reservation.WorkItemID, Revision: 1}, []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := hub.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateCollaborationVersion(t.Context(), version)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			project.reservation = reservation
		}
		hubServer := httptest.NewServer(hub.Handler())
		t.Cleanup(hubServer.Close)
		var dropReceipt atomic.Bool
		policy := artifact.Policy{ID: "example", Limits: artifact.Limits{RetainedBytes: 64 << 20, ReservedBytes: 32 << 20, ArtifactBytes: 8 << 20, UploadBytes: artifact.MaxChunkBytes, RetentionSeconds: 3600}, AbandonedUploadSeconds: 30, DeletionRecordSeconds: 7200, BackupSeconds: 3600}
		for _, publisher := range publishers {
			publisher.config = artifact.Config{OrganizationID: string(org), ServiceID: publisher.serviceID, Mode: publisher.mode, HostedOptIn: publisher.mode == "hosted", Policy: policy}
			for _, project := range projects {
				unconfigured := &artifact.RemoteCatalog{Hub: &artifact.RemoteHub{Origin: hubServer.URL, OrganizationID: string(org), ProjectID: string(project.id), ServiceID: publisher.serviceID, PublisherToken: func() string { return publisher.token }, Client: catalogJourneyHTTPClient(t)}}
				if _, err := unconfigured.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: artifact.CatalogScope{OrganizationID: string(org), ProjectID: string(project.id), ServiceID: publisher.serviceID}, Operation: "traffic_settings"}); !errors.Is(err, artifact.ErrQuota) {
					t.Fatal("catalog admitted an unconfigured server policy", err)
				}
				path := "/api/v2/organizations/" + string(org) + "/projects/" + string(project.id) + "/artifact-services/" + publisher.serviceID + "/catalog-policy"
				requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPut, path, publisher.token, policy), http.StatusNotFound)
				requireNativeStatus(t, performHubAPIRequest(t, hub, http.MethodPut, path, adminToken, policy), http.StatusNoContent)
			}
			startCatalogJourneyPublisher(t, publisher, hubServer.URL, storage, &dropReceipt)
			if foreignPublisher != "" {
				foreign := &artifact.RemoteCatalog{Hub: &artifact.RemoteHub{Origin: hubServer.URL, OrganizationID: string(org), ServiceID: publisher.serviceID, PublisherToken: func() string { return foreignPublisher }, Client: catalogJourneyHTTPClient(t)}}
				if _, err := foreign.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: artifact.CatalogScope{OrganizationID: string(org), ServiceID: publisher.serviceID}, Operation: "usage"}); !errors.Is(err, artifact.ErrDenied) {
					t.Fatal("foreign tenant publisher accepted", err)
				}
			}
			for _, project := range projects {
				reservation := project.reservation
				reservation.ServiceID, reservation.Mode, reservation.HostedOptIn = publisher.serviceID, publisher.mode, publisher.mode == "hosted"
				raw := artifact.CatalogCommand{CatalogScope: artifact.CatalogScope{OrganizationID: string(org), ProjectID: string(project.id), ServiceID: publisher.serviceID}, Operation: "reserve", Upload: artifact.Upload{ArtifactID: artifact.NewID("artifact"), Reservation: reservation}}
				if _, err := publisher.config.Catalog.Execute(t.Context(), raw); !errors.Is(err, artifact.ErrDenied) {
					t.Fatal("publisher bypassed live execution authorization", err)
				}
				client := &artifact.Client{Origin: publisher.server.URL, Token: func(context.Context) (string, error) { return workerToken, nil }}
				upload, err := client.Reserve(t.Context(), reservation)
				if err != nil {
					t.Fatal(err)
				}
				replay, err := client.Reserve(t.Context(), reservation)
				if err != nil || replay.ArtifactID != upload.ArtifactID {
					t.Fatal("reservation replay", replay, err)
				}
				part := artifact.Part{Sequence: 0, MediaType: "text/plain; charset=utf-8", Data: []byte("example " + string(project.id)), SHA256: artifact.Digest([]byte("example " + string(project.id)))}
				object, err := client.Append(t.Context(), upload.ArtifactID, part)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := client.Finalize(t.Context(), upload.ArtifactID, "complete", nil)
				if err != nil {
					t.Fatal(err)
				}
				again, err := client.Finalize(t.Context(), upload.ArtifactID, "complete", nil)
				if err != nil || again != ref {
					t.Fatal("commit replay", err)
				}
				dropReceipt.Store(true)
				if err := publisher.service.PublishPending(t.Context(), publisher.remote); !errors.Is(err, artifact.ErrAuthorization) {
					t.Fatal("lost publication response", err)
				}
				if err := publisher.service.PublishPending(t.Context(), publisher.remote); err != nil {
					t.Fatal("uncertain publication reconciliation", err)
				}
				publisher.records = append(publisher.records, catalogJourneyArtifact{project: project.id, upload: upload, object: object, reference: ref, part: part})
			}
		}
		hubServer.Close()
		if err := hub.Close(); err != nil {
			t.Fatal(err)
		}
		hub = open()
		hubServer = httptest.NewServer(hub.Handler())
		t.Cleanup(hubServer.Close)
		for _, publisher := range publishers {
			if err := publisher.service.Close(); err != nil {
				t.Fatal(err)
			}
			startCatalogJourneyPublisher(t, publisher, hubServer.URL, storage, &dropReceipt)
			var retained int64
			for _, record := range publisher.records {
				_, data, err := publisher.service.ReadObject(t.Context(), record.upload.ArtifactID, record.reference.Revision, record.object.ID)
				if err != nil || !bytes.Equal(data, record.part.Data) {
					t.Fatal("catalog did not survive Hub and service restart", err)
				}
				current, err := publisher.service.Upload(t.Context(), record.upload.ArtifactID)
				if err != nil {
					t.Fatal(err)
				}
				retained += current.RetainedBytes
			}
			usage, err := publisher.service.Usage(t.Context())
			if err != nil || usage.RetainedBytes != retained || usage.ReservedBytes != 0 {
				t.Fatal("service accounting crossed publishers", usage, retained, err)
			}
			destination := filepath.Join(t.TempDir(), "catalog.jsonl")
			if err := publisher.service.Backup(t.Context(), destination); err != nil {
				t.Fatal(err)
			}
			verifyCatalogJourneyExport(t, destination, org, publisher.serviceID, len(publisher.records))
		}
		for _, publisher := range publishers {
			old := now().Add(-40 * 24 * time.Hour).Truncate(time.Minute)
			if err := hub.database.postgres.WriteArtifactCatalog(t.Context(), org, corestore.Serving, func(w postgres.ArtifactCatalogWriter) error {
				for _, project := range projects {
					if err := w.RecordCatalogTrafficNow(t.Context(), postgres.CatalogTrafficReport{OrganizationID: org, ProjectID: project.id, ServiceID: publisher.serviceID, ID: "old-report", UploadBytes: 10, Requests: 1}, old); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := hub.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.DeleteAccessTokenGrant(t.Context(), publisher.id, projects[1].id)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := publisher.config.Catalog.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: artifact.CatalogScope{OrganizationID: string(org), ServiceID: publisher.serviceID}, Operation: "prune_traffic", Limit: 32}); err != nil {
				t.Fatal(err)
			}
			if err := hub.database.postgres.ReadArtifactCatalog(t.Context(), org, corestore.Serving, func(r postgres.ArtifactCatalogReader) error {
				usage, err := r.CatalogServiceTraffic(t.Context(), postgres.CatalogServiceTrafficInput{OrganizationID: org, ServiceID: publisher.serviceID, Minute: old, Minute_2: old.Add(time.Minute)})
				if err != nil {
					return err
				}
				if usage.RelayBytes != 20 || usage.Requests != 2 {
					t.Fatal("traffic pruning erased a summary with protected project sources", usage)
				}
				for index, project := range projects {
					buckets, err := r.ScopedCatalogTrafficBuckets(t.Context(), postgres.ScopedCatalogTrafficBucketsInput{OrganizationID: org, ServiceID: publisher.serviceID, ProjectID: string(project.id), After1: time.Unix(0, 0), PageLimit: 32})
					if err != nil {
						return err
					}
					receipts, err := r.ScopedCatalogTrafficReceipts(t.Context(), postgres.ScopedCatalogTrafficReceiptsInput{OrganizationID: org, ServiceID: publisher.serviceID, ProjectID: string(project.id), PageLimit: 32})
					if err != nil {
						return err
					}
					oldBuckets, oldReceipts := 0, 0
					for _, row := range buckets {
						if row.Minute.Equal(old) {
							oldBuckets++
						}
					}
					for _, row := range receipts {
						if row.ID == "old-report" {
							oldReceipts++
						}
					}
					if oldBuckets != index || oldReceipts != index {
						t.Fatalf("traffic pruning crossed a revoked project grant: project %d buckets=%d receipts=%d", index, oldBuckets, oldReceipts)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := hub.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: publisher.id, ProjectID: projects[1].id})
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := publisher.config.Catalog.Execute(t.Context(), artifact.CatalogCommand{CatalogScope: artifact.CatalogScope{OrganizationID: string(org), ServiceID: publisher.serviceID}, Operation: "prune_traffic", Limit: 32}); err != nil {
				t.Fatal(err)
			}
			request := projects[0].reservation
			request.ServiceID, request.Mode, request.HostedOptIn = publisher.serviceID, publisher.mode, publisher.mode == "hosted"
			request.Key = "abandoned"
			client := &artifact.Client{Origin: publisher.server.URL, Token: func(context.Context) (string, error) { return workerToken, nil }}
			upload, err := client.Reserve(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Append(t.Context(), upload.ArtifactID, artifact.Part{Sequence: 0, MediaType: "text/plain; charset=utf-8", Data: []byte("retained prefix"), SHA256: artifact.Digest([]byte("retained prefix"))}); err != nil {
				t.Fatal(err)
			}
			publisher.abandoned = upload
		}
		clock.Store(now().Add(2 * time.Minute))
		for _, publisher := range publishers {
			if err := publisher.service.Maintain(t.Context()); err != nil {
				t.Fatal(err)
			}
			current, err := publisher.service.Upload(t.Context(), publisher.abandoned.ArtifactID)
			if err != nil || current.State != "interrupted" {
				t.Fatal("abandoned log was not interrupted", current.State, err)
			}
			if err := publisher.service.PublishPending(t.Context(), publisher.remote); err != nil {
				t.Fatal(err)
			}
			record := publisher.records[0]
			grantPath := "/api/v2/organizations/" + string(org) + "/projects/" + string(record.project) + "/work-items/" + record.reference.WorkItemID + "/artifacts/" + record.reference.ArtifactID + "/access"
			response := performHubAPIRequest(t, hub, http.MethodPost, grantPath, workerToken, struct {
				Revision int64 `json:"revision"`
			}{record.reference.Revision})
			requireNativeStatus(t, response, http.StatusOK)
			var grant artifact.Grant
			decodeHubResponse(t, response, &grant)
			path := publisher.server.URL + "/v1/artifacts/" + record.reference.ArtifactID + "/manifests/" + strconv.FormatInt(record.reference.Revision, 10) + "/objects/" + record.object.ID
			client := catalogJourneyHTTPClient(t)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+grant.Token)
			downloaded, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(downloaded.Body)
			closeErr := downloaded.Body.Close()
			if downloaded.StatusCode != http.StatusOK || readErr != nil || closeErr != nil || !bytes.Equal(data, record.part.Data) {
				t.Fatal("artifact read depended on an active runner", downloaded.StatusCode, readErr, closeErr)
			}
		}
		clock.Store(now().Add(2 * time.Hour))
		for _, publisher := range publishers {
			if err := publisher.service.Maintain(t.Context()); err != nil {
				t.Fatal(err)
			}
			for _, record := range publisher.records {
				current, err := publisher.service.Upload(t.Context(), record.upload.ArtifactID)
				if err != nil || current.State != "deleted" {
					t.Fatal("retention did not delete expired artifact", current.State, err)
				}
			}
		}
		foreignPublisher = publishers[1].token
		err = hub.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessCredential(t.Context(), publishers[0].id, now())
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := publishers[0].service.Usage(t.Context()); !errors.Is(err, artifact.ErrDenied) {
			t.Fatal("revoked publisher read catalog", err)
		}
		if _, err := publishers[1].service.Usage(t.Context()); err != nil {
			t.Fatal("publisher revocation crossed services", err)
		}
	}
}

func catalogJourneyHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func startCatalogJourneyPublisher(t *testing.T, publisher *catalogJourneyPublisher, origin string, storage artifact.Storage, drop *atomic.Bool) {
	t.Helper()
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	publisher.remote = &artifact.RemoteHub{Origin: origin, OrganizationID: publisher.config.OrganizationID, ServiceID: publisher.serviceID, PublisherToken: func() string { return publisher.token }, Client: &http.Client{Transport: catalogJourneyTransport{base: transport, drop: drop}}}
	publisher.config.Catalog = &artifact.RemoteCatalog{Hub: publisher.remote}
	var allowances artifact.Allowances
	if publisher.mode == "hosted" {
		allowances = publisher.remote
	}
	service, err := artifact.NewService(t.Context(), publisher.config, storage, allowances)
	if err != nil {
		t.Fatal(err)
	}
	publisher.service = service
	publisher.remote.Usage = service.Usage
	handler, err := artifact.NewHTTPServer(service, publisher.remote)
	if err != nil {
		t.Fatal(err)
	}
	publisher.handler.Store(handler)
}

func verifyCatalogJourneyExport(t *testing.T, path string, org tracker.OrganizationID, service string, uploads int) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	count, complete := 0, false
	for {
		var page artifact.CatalogResult
		err := decoder.Decode(&page)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if page.OrganizationID != string(org) || page.ServiceID != service {
			t.Fatal("export crossed catalog scope")
		}
		count += len(page.Uploads)
		complete = page.Complete
	}
	if !complete || count != uploads {
		t.Fatal("incomplete catalog export", complete, count, uploads)
	}
}
