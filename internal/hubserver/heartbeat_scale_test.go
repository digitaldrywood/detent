package hubserver

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHeartbeatScaleBenchmark(t *testing.T) {
	t.Parallel()
	f := newRunnerPGFixture(t)
	const organization tracker.OrganizationID = "org_startup"
	const project tracker.ProjectID = "prj_runner"
	projects := []tracker.ProjectID{project, "prj_scale_1", "prj_scale_2", "prj_scale_3"}
	binding := runnerauth.NewBinding()
	token, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	f.bindings = append(f.bindings, binding)
	f.tokens = append(f.tokens, token)
	now := f.service.config.Clock()
	content := eventstore.NewContent(f.service.runDetailStore())
	versions := make([]eventstore.CollaborationVersion, 0, 1500)
	for i := range 1500 {
		state := "Done"
		if i < 50 {
			state = "Todo"
		}
		item := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(fmt.Sprintf("wi_scale_%d", i)), Number: i + 3, Revision: 1}, Title: "Example issue", State: state, CreatedAt: now, UpdatedAt: now}
		raw, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: project, WorkItemID: item.WorkItemID, RecordID: string(item.WorkItemID), Revision: 1}, raw)
		if err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	var items []corestore.WorkItem
	var leases []leasestore.Lease
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		for _, id := range projects[1:] {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: string(id), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
		}
		credential, err := w.AccessCredential(t.Context(), "credential_1")
		if err != nil {
			return err
		}
		credential.ID, credential.Name, credential.Hash = "credential_3", "Example runner 3", apikey.HashToken(token)
		credential.Fingerprint = hubapi.TokenFingerprint(credential.Hash)
		if err := w.CreateAccessCredential(t.Context(), credential); err != nil {
			return fmt.Errorf("create scale runner credential: %w", err)
		}
		enrollment, err := w.RunnerEnrollment(t.Context(), "enrollment_1")
		if err != nil {
			return err
		}
		enrollment.ID, enrollment.RunnerID, enrollment.MachineID = "enrollment_3", binding.RunnerID, binding.MachineID
		enrollment.TokenHash, enrollment.RedeemedAt = apikey.HashToken("example_enrollment_3"), nil
		if err := w.CreateRunnerEnrollment(t.Context(), enrollment); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollment.ID, binding.RunnerID, binding.MachineID, now); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: string(binding.MachineID), Hostname: "example", Capacity: 4, Version: "test"}); err != nil {
			return err
		}
		identity, err := w.RunnerIdentity(t.Context(), f.bindings[0].RunnerID)
		if err != nil {
			return err
		}
		identity.RunnerID, identity.MachineID, identity.TokenID, identity.EnrollmentID = binding.RunnerID, binding.MachineID, credential.ID, enrollment.ID
		if err := w.CreateRunnerIdentity(t.Context(), identity); err != nil {
			return err
		}
		for index, binding := range f.bindings {
			identity, err := w.RunnerIdentity(t.Context(), binding.RunnerID)
			if err != nil {
				return err
			}
			identity.ProjectIDs, identity.CapacityLimit, identity.ReportedCapacity, identity.DisplayName = projects, 4, 4, "Example"
			if err := w.UpdateRunnerRouting(t.Context(), identity.Revision, identity); err != nil {
				return err
			}
			for _, id := range projects {
				if id == project && index < 2 {
					continue
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: fmt.Sprintf("credential_%d", index+1), ProjectID: id}); err != nil {
					return err
				}
			}
		}
		for i := range 1500 {
			state := "Done"
			if i < 50 {
				state = "Todo"
			}
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: project, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_scale_%d", i)), Number: int64(i + 3), Title: "Example issue", WorkflowStateName: &state})
			if err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), versions[i]); err != nil {
				return err
			}
			items = append(items, item)
			if i >= 300 {
				continue
			}
			id := fmt.Sprintf("scale-attempt-%04d", i)
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: string(f.bindings[0].MachineID), SessionID: id, LeaseID: id, TTL: time.Hour})
			if err != nil {
				return err
			}
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: id, RunnerID: f.bindings[0].RunnerID, AssignedAt: now}); err != nil {
				return err
			}
			leases = append(leases, lease)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	attempts := make([]leasestore.AttemptEvent, 0, len(leases))
	events := make([]eventstore.Event, 0, len(leases))
	for index, lease := range leases {
		data := tracker.NativeRunData{AttemptID: lease.LeaseID, RunID: lease.LeaseID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: f.policy.PolicyID, Sequence: 1,
			MachineID: tracker.MachineID(lease.MachineID), SessionID: lease.SessionID, RunnerID: f.bindings[0].RunnerID, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example", Model: "example"},
			Runtime: &tracker.NativeRuntimeObservation{WorkerLaunch: &tracker.NativeWorkerLaunch{PID: 1, ProcessStart: now}}}
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := f.service.runDetailStore().StoreDetail(t.Context(), string(organization), raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateNativeDetailReference(t.Context(), f.service.runDetailStore(), organization, ref, raw); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, leasestore.AttemptEvent{ID: data.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: data.RunID, Sequence: 1, Status: "interrupted", PayloadObjectKey: ref.Key, PayloadReference: ref, WorkerCleanupRequired: new(nativeRunCleanupRequired(data))})
		raw, err = json.Marshal(tracker.CollaborationEvent{ID: "event_" + lease.LeaseID, OrganizationID: organization, ProjectID: project, AggregateID: items[index].NativeID, Type: "run.started", SchemaVersion: 1, Actor: tracker.Actor{Kind: "service", PrincipalID: "credential_1"}, RecordedAt: now, Data: tracker.CollaborationData{Run: &data}})
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		for index, attempt := range attempts {
			if _, err := w.RecordAttempt(t.Context(), attempt); err != nil {
				return fmt.Errorf("record scale attempt %d: %w", index, err)
			}
			if _, err := w.AppendEvent(t.Context(), events[index]); err != nil {
				return err
			}
			if _, err := w.Release(t.Context(), attempt.Fence, "example"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for index, binding := range f.bindings {
		for _, project := range projects {
			view := runnerauth.ProjectConfiguration{ProjectID: string(project), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now}
			path := fmt.Sprintf("/api/v2/organizations/%s/projects/%s/machines/%s/heartbeat", organization, project, binding.MachineID)
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, f.tokens[index], nativeMachineHeartbeatRequest{DisplayName: "Example", Capacity: 4, Version: "test", OS: "linux", Architecture: "amd64", ProjectConfiguration: &view})
			requireNativeStatus(t, response, http.StatusOK)
		}
	}
	path := fmt.Sprintf("/api/v2/organizations/%s/projects/%s/machines/%s/heartbeat", organization, project, f.bindings[0].MachineID)
	var heartbeat, claimState, callback, db time.Duration
	var count, released int
	result := testing.Benchmark(func(b *testing.B) {
		heartbeat, claimState, callback, db, count, released = 0, 0, 0, 0, 0, 0
		for b.Loop() {
			started := time.Now()
			timing := &requestTiming{}
			request := newHubAPIRequest(t, http.MethodPost, path, f.tokens[0], nativeMachineHeartbeatRequest{DisplayName: "Example", Capacity: 4, Version: "test", OS: "linux", Architecture: "amd64"})
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
			requireNativeStatus(t, response, http.StatusOK)
			found := false
			for _, span := range timing.NamedSpans() {
				if span.Name == "pg_callback" {
					callback += span.Duration
					found = true
				}
			}
			if !found {
				t.Fatal("missing PostgreSQL transaction callback timing")
			}
			db += time.Duration(timing.DB.Load())
			var snapshot runnerauth.RoutingSnapshot
			decodeHubResponse(t, response, &snapshot)
			if snapshot.RunnerID != f.bindings[0].RunnerID {
				t.Fatalf("heartbeat routing = %+v", snapshot)
			}
			heartbeat += time.Since(started)
			started = time.Now()
			registrationPath := fmt.Sprintf("/api/v2/organizations/%s/projects/%s/machines/register", organization, project)
			response = performHubAPIRequest(t, f.service, http.MethodPost, registrationPath, f.tokens[0], map[string]any{"id": f.bindings[0].MachineID, "hostname": "example", "display_name": "Example", "capacity": 4, "version": "test", "os": "linux", "architecture": "amd64"})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &snapshot)
			if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) > releasedExecutionsPageSize {
				t.Fatalf("released executions = %+v", snapshot.ClaimState)
			}
			released += len(snapshot.ClaimState.ReleasedExecutions)
			claimState += time.Since(started)
			count++
		}
	})
	if released == 0 {
		t.Fatal("registration did not return seeded released executions")
	}
	t.Logf("benchmark=%s issues=1500 nonterminal=50 released=300 runners=3 projects=4 heartbeat_mean_ms=%.3f registration_mean_ms=%.3f pg_callback_mean_ms=%.3f db_mean_ms=%.3f", result.String(), float64(heartbeat)/float64(count)/1e6, float64(claimState)/float64(count)/1e6, float64(callback)/float64(count)/1e6, float64(db)/float64(count)/1e6)
}

func TestHeartbeatResponseReaderSnapshot(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		capacity      int
		failRead      bool
		revokeProject bool
	}{{name: "committed observation", capacity: 2}, {name: "committed observation survives response failure", capacity: 1, failRead: true}, {name: "project removed before response snapshot", capacity: 2, revokeProject: true}} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "heartbeat-reader")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
			r.enroll(t)
			if !test.revokeProject {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					identity.ProjectRanks = map[tracker.ProjectID]int{f.project.ID: 3}
					identity.CapacityRequest = &runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("c", 64), Capacity: 2, Backend: "codex"}
					return w.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
				}); err != nil {
					t.Fatal(err)
				}
			}

			path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, map[string]any{"display_name": "Example runner", "capacity": 0, "version": "test"}), http.StatusOK)
			f.service.tokenUseWork.Wait()
			installReportsBudgetTrace(t, f.service)
			var repositoryRequest struct {
				RequestID string `json:"request_id"`
			}
			if test.revokeProject {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/repositories", testHubAdminToken, map[string]any{"runner_id": r.binding.RunnerID, "idempotency_key": "heartbeat-repositories"})
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &repositoryRequest)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx = context.WithValue(ctx, heartbeatResponseContextKey{}, true)
			observed, removed := false, false
			var traceMu sync.Mutex
			postCommit := false
			totalPostCommitReads := 0
			repository := f.service.config.TenantRepository
			snapshotRepository := repository.(tenantstore.SnapshotRepository)
			owner := &heartbeatResponseRepository{Repository: repository, SnapshotRepository: snapshotRepository, heartbeat: repository.(interface {
				WriteRunnerHeartbeat(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
			})}
			readCapacity := func() int64 {
				t.Helper()
				var capacity int64
				if err := repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(rdr tenantstore.Reader) error {
					row, err := rdr.RunnerIdentity(t.Context(), r.binding.RunnerID)
					capacity = row.ReportedCapacity
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return capacity
			}
			owner.afterCommit = func() error {
				f.service.tokenUseWork.Wait()
				if held := f.service.database.postgres.Stats().AcquiredConns(); held != 0 {
					return fmt.Errorf("heartbeat response still holds %d PostgreSQL connections after write", held)
				}
				traceMu.Lock()
				postCommit = true
				traceMu.Unlock()

				if capacity := readCapacity(); capacity != int64(test.capacity) {
					return fmt.Errorf("committed capacity=%d want=%d", capacity, test.capacity)
				}
				if test.revokeProject && !removed {
					removed = true
					change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Example runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{}}}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
				}
				return nil
			}
			owner.beforeClaim = func() error {
				if observed {
					return nil
				}
				observed = true
				if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
					row, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
					if err != nil {
						return err
					}
					row.ReportedCapacity = 0
					return w.UpdateRunnerIdentity(t.Context(), row)
				}); err != nil {
					return err
				}
				if test.failRead {
					cancel()
				}
				return nil
			}
			f.service.config.TenantRepository = owner
			t.Cleanup(func() { f.service.config.TenantRepository = repository })
			timing := &requestTiming{Trace: func(query string, _ []driver.NamedValue) {
				if !reportsBudgetReadStatement(query) {
					return
				}
				traceMu.Lock()
				defer traceMu.Unlock()
				if postCommit {
					totalPostCommitReads++
				}
			}}
			summary := testHostSummary(f.service.config.Clock())
			request := newHubAPIRequest(t, http.MethodPost, path, r.redemption.Credential, map[string]any{"display_name": "Example runner", "capacity": test.capacity, "version": "test", "change_cursor": "", "host_metrics": []hostmetrics.Summary{summary}})
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(ctx, timing)))
			traceMu.Lock()
			reads := totalPostCommitReads
			committed := postCommit
			traceMu.Unlock()
			if !committed || reads == 0 {
				t.Fatalf("postcommit response read probe missing: committed=%t reads=%d", committed, reads)
			}
			t.Logf("total_post_commit_reads=%d response_case=%s", reads, test.name)

			if test.revokeProject {
				if !removed || observed {
					t.Fatalf("project removed=%t claim state read=%t", removed, observed)
				}
				requireNativeStatus(t, response, http.StatusOK)
				var snapshot runnerauth.RoutingSnapshot
				decodeHubResponse(t, response, &snapshot)
				if snapshot.RunnerID != r.binding.RunnerID || snapshot.Revision != 2 || len(snapshot.Routing.ProjectIDs) != 0 || snapshot.Routing.DisplayName != "Example runner" {
					t.Fatalf("response did not preserve current routing: %+v", snapshot)
				}
				if snapshot.RepositoryListRequest != repositoryRequest.RequestID {
					t.Fatalf("repository request=%q want=%q", snapshot.RepositoryListRequest, repositoryRequest.RequestID)
				}
				if len(snapshot.HostMetricsAcknowledged) != 1 || !snapshot.HostMetricsAcknowledged[0].Hour.Equal(summary.Hour) || !snapshot.HostMetricsAcknowledged[0].SegmentID.Equal(summary.SegmentID) {
					t.Fatalf("committed metric acknowledgements=%+v", snapshot.HostMetricsAcknowledged)
				}
				denied := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items", r.redemption.Credential, nil)
				requireNativeStatus(t, denied, http.StatusNotFound)
				for _, field := range []string{"claim_state", "changes", "github_intake", "project_configuration_request", "target_runner_version"} {
					if strings.Contains(response.Body.String(), `"`+field+`"`) {
						t.Fatalf("response disclosed %s: %s", field, response.Body.String())
					}
				}
				if capacity := readCapacity(); capacity != int64(test.capacity) {
					t.Fatalf("committed capacity=%d want=%d", capacity, test.capacity)
				}
				return
			}
			if !observed {
				t.Fatal("response did not read claim state")
			}
			if test.failRead {
				if response.Code < 400 {
					t.Fatalf("canceled response status=%d", response.Code)
				}
			} else {
				requireNativeStatus(t, response, http.StatusOK)
				var snapshot runnerauth.RoutingSnapshot
				decodeHubResponse(t, response, &snapshot)
				if snapshot.Routing.CapacityRequest == nil || snapshot.Routing.CapacityRequest.Capacity != 2 || snapshot.Routing.CapacityRequest.ExpectedConfigRevision != strings.Repeat("c", 64) || snapshot.Routing.ProjectRanks[f.project.ID] != 3 {
					t.Fatalf("heartbeat dropped current routing fields: %+v", snapshot.Routing)
				}
				if snapshot.ClaimState == nil || snapshot.ClaimState.RunnerCapacity != test.capacity {
					t.Fatalf("response did not preserve reader snapshot: %+v", snapshot.ClaimState)
				}
			}
			capacity := readCapacity()
			if capacity != 0 {
				t.Fatalf("concurrent writer capacity=%d want=0", capacity)
			}
		})
	}
}

type heartbeatResponseContextKey struct{}

type heartbeatResponseRepository struct {
	tenantstore.Repository
	tenantstore.SnapshotRepository
	heartbeat interface {
		WriteRunnerHeartbeat(context.Context, tracker.OrganizationID, string, string, func(context.Context, tenantstore.Writer) error) error
	}
	afterCommit func() error
	beforeClaim func() error
}

func (r *heartbeatResponseRepository) WriteRunnerHeartbeat(ctx context.Context, org tracker.OrganizationID, credential, runner string, write func(context.Context, tenantstore.Writer) error) error {
	if err := r.heartbeat.WriteRunnerHeartbeat(ctx, org, credential, runner, write); err != nil {
		return err
	}
	if ctx.Value(heartbeatResponseContextKey{}) == true && r.afterCommit != nil {
		return r.afterCommit()
	}
	return nil
}

func (r *heartbeatResponseRepository) ReadTenantSnapshot(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.SnapshotRepository.ReadTenantSnapshot(ctx, org, workload, func(reader tenantstore.Reader) error {
		if ctx.Value(heartbeatResponseContextKey{}) != true {
			return read(reader)
		}
		return read(heartbeatResponseReader{Reader: reader, RoutingReader: reader.(runnerstore.RoutingReader), owner: r})
	})
}

func (r *heartbeatResponseRepository) ReadRunnerHeartbeatSnapshot(ctx context.Context, org tracker.OrganizationID, credential, runner string, project tracker.ProjectID, clock func() time.Time, read func(context.Context, tenantstore.Reader, time.Time) error) error {
	owner, ok := r.Repository.(interface {
		ReadRunnerHeartbeatSnapshot(context.Context, tracker.OrganizationID, string, string, tracker.ProjectID, func() time.Time, func(context.Context, tenantstore.Reader, time.Time) error) error
	})
	if !ok {
		return r.ReadTenantSnapshot(ctx, org, corestore.Serving, func(reader tenantstore.Reader) error { return read(ctx, reader, clock().UTC()) })
	}
	return owner.ReadRunnerHeartbeatSnapshot(ctx, org, credential, runner, project, clock, func(ctx context.Context, reader tenantstore.Reader, now time.Time) error {
		if ctx.Value(heartbeatResponseContextKey{}) == true {
			reader = heartbeatResponseReader{Reader: reader, RoutingReader: reader.(runnerstore.RoutingReader), owner: r}
		}
		return read(ctx, reader, now)
	})
}

type heartbeatResponseReader struct {
	tenantstore.Reader
	runnerstore.RoutingReader
	owner *heartbeatResponseRepository
}

func (r heartbeatResponseReader) RunnerClaimState(ctx context.Context, runner string, project tracker.ProjectID, after string, limit int, now time.Time) (runnerstore.ClaimStatePage, error) {
	if r.owner.beforeClaim != nil {
		if err := r.owner.beforeClaim(); err != nil {
			return runnerstore.ClaimStatePage{}, err
		}
	}
	return r.Reader.(runnerstore.ClaimStateReader).RunnerClaimState(ctx, runner, project, after, limit, now)
}

func TestHeartbeatResponseArchivedCleanup(t *testing.T) {
	t.Parallel()
	for _, concurrent := range []int{1, 32} {
		t.Run(fmt.Sprintf("responses=%d", concurrent), func(t *testing.T) {
			store := &memoryRunDetails{}
			f := newPostgresDefaultNativeFixture(t, Config{RunDetails: store})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			r.enroll(t)
			r.reportCapacity(t)
			issue := f.create(t, "archived checkpoint")
			worker := r.redemption.Credential
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{
				PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: "checkpoint-session",
				TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
			})
			requireNativeStatus(t, response, http.StatusOK)
			var lease tracker.NativeLease
			decodeHubResponse(t, response, &lease)
			event := nativeStartedEvent(lease)
			now := f.service.config.Clock()
			event.Data.Runtime = &tracker.NativeRuntimeObservation{LocalAttemptID: 1, Generation: 1, HeartbeatAt: now, Phase: "implementation", WorkerLaunch: &tracker.NativeWorkerLaunch{
				PID: 1, GroupID: 1, ProcessStart: now, PolicyID: hubTestPolicy().ID, ExecutionVersion: "test", ControlVersion: "test",
				ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64), Protocol: 6, SelectedAt: now,
			}}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
			event.Data.Handoff = nativeTestCheckpoint()
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("cleanup authority status=%d error=%v", status, err)
			}
			scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
			archived, err := f.service.readNativeAttemptSnapshotTenant(t.Context(), scope, string(issue.WorkItemID), event.Data.AttemptID)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err := f.service.readNativeAttemptCheckpointTenant(t.Context(), archived)
			if err != nil || checkpoint == nil || archived.CheckpointPayload == nil {
				t.Fatalf("checkpoint is not archived: %+v error=%v", archived.Attempt, err)
			}
			if archived.Attempt.WorkerCleanupRequired == nil || !*archived.Attempt.WorkerCleanupRequired || archived.Attempt.RuntimeSummarySequence != archived.Attempt.Sequence {
				t.Fatalf("verified launch did not publish current cleanup fact: %+v", archived.Attempt)
			}
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			if f.service.outbox != nil {
				f.service.outbox.stop()
			}
			f.service.tokenUseWork.Wait()
			store.mu.Lock()
			store.reads = 0
			store.mu.Unlock()
			results := make(chan *httptest.ResponseRecorder, concurrent)
			for range concurrent {
				request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "Runner", "capacity": 1, "version": "test"})
				go func() {
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					results <- response
				}()
			}
			for range concurrent {
				response := <-results
				requireNativeStatus(t, response, http.StatusOK)
				var snapshot runnerauth.RoutingSnapshot
				decodeHubResponse(t, response, &snapshot)
				if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 1 || snapshot.ClaimState.ReleasedExecutions[0].AttemptID != event.Data.AttemptID {
					t.Fatalf("archived execution missing from response: %+v", snapshot.ClaimState)
				}
			}
			store.mu.Lock()
			reads := store.reads
			store.mu.Unlock()
			if reads != 0 {
				t.Fatalf("cleanup polling hydrated %d archive entries", reads)
			}
			store.mu.Lock()
			originalCheckpoint := append([]byte(nil), store.data[archived.CheckpointPayload.Key]...)
			store.data[archived.CheckpointPayload.Key] = []byte("{")
			store.keyReads = map[string]int{}
			store.mu.Unlock()
			t.Cleanup(func() {
				store.mu.Lock()
				store.data[archived.CheckpointPayload.Key] = originalCheckpoint
				store.mu.Unlock()
			})
			checkpointReads := func() int {
				store.mu.Lock()
				defer store.mu.Unlock()
				return store.keyReads[archived.CheckpointPayload.Key]
			}
			attemptPath := f.base + "/work-items/" + string(issue.WorkItemID) + "/attempts/" + event.Data.AttemptID
			response = performHubAPIRequest(t, f.service, http.MethodGet, attemptPath, worker, nil)
			requireNativeStatus(t, response, http.StatusServiceUnavailable)
			if !strings.Contains(response.Body.String(), "archive_invalid") {
				t.Fatalf("expected invalid checkpoint archive: %s", response.Body.String())
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, attemptPath+"?view=cleanup", worker, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var attempt tracker.NativeAttempt
			decodeHubResponse(t, response, &attempt)
			if attempt.Runtime == nil || attempt.Runtime.WorkerLaunch == nil || attempt.Runtime.LocalAttemptID != 1 || attempt.Identity == nil || *attempt.Identity != *event.Data.Identity || attempt.ClaimReleasedAt == nil || attempt.Checkpoint != nil || len(attempt.ArtifactIDs) != 0 || checkpointReads() != 1 {
				t.Fatalf("cleanup projection lost proof or loaded historical evidence: %+v archive reads=%d", attempt, checkpointReads())
			}
			launch, original := attempt.Runtime.WorkerLaunch, event.Data.Runtime.WorkerLaunch
			if launch.PID != original.PID || launch.GroupID != original.GroupID || !launch.ProcessStart.Equal(original.ProcessStart) || launch.PolicyID != original.PolicyID || launch.ExecutionSHA256 != original.ExecutionSHA256 {
				t.Fatalf("cleanup worker proof changed: got=%+v want=%+v", launch, original)
			}
			other := prepareRunner(t, f, runnerauth.Read)
			other.enroll(t)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, attemptPath+"?view=cleanup", other.redemption.Credential, nil), http.StatusNotFound)
			event.Type, event.IdempotencyKey = "run.finished", "cleanup-acknowledged"
			event.Data = tracker.NativeRunData{AttemptID: attempt.AttemptID, RunID: attempt.RunID, LeaseID: attempt.LeaseID, FencingToken: attempt.FencingToken, PolicyID: attempt.PolicyID, Identity: attempt.Identity, Sequence: attempt.Sequence + 1, Outcome: "cancelled"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
			response = performHubAPIRequest(t, f.service, http.MethodGet, attemptPath+"?view=cleanup", worker, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var receipt tracker.NativeAttempt
			decodeHubResponse(t, response, &receipt)
			if receipt.CleanupAcknowledgedAt == nil || receipt.CleanupAcknowledgedAt.IsZero() || receipt.AttemptID != attempt.AttemptID || receipt.LeaseID != attempt.LeaseID || receipt.FencingToken != attempt.FencingToken || receipt.Runtime == nil || receipt.Runtime.WorkerLaunch == nil || *receipt.Runtime.WorkerLaunch != *attempt.Runtime.WorkerLaunch {
				t.Fatalf("acknowledged cleanup receipt = %+v", receipt)
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
				receipt, err := reader.Attempt(t.Context(), event.Data.AttemptID)
				if err == nil && (receipt.CleanupAcknowledgedAt == nil || receipt.CleanupAcknowledgedAt.IsZero() || receipt.Sequence != event.Data.Sequence) {
					t.Fatalf("cleanup receipt missing: %+v", receipt)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if reads := checkpointReads(); reads != 1 {
				t.Fatalf("cleanup acknowledgment hydrated checkpoint %d times", reads)
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", worker, map[string]any{"display_name": "Runner", "capacity": 1, "version": "test"})
			requireNativeStatus(t, response, http.StatusOK)
			var snapshot runnerauth.RoutingSnapshot
			decodeHubResponse(t, response, &snapshot)
			if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 0 {
				t.Fatalf("acknowledged execution was polled again: %+v", snapshot.ClaimState)
			}
		})
	}
}

func TestHeartbeatClaimSkipsUnknownLegacySource(t *testing.T) {
	t.Parallel()
	f := newPostgresDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if f.service.outbox != nil {
		f.service.outbox.stop()
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	r := placementFixtureRunner(t, f, "legacy-worker", "local", false, 1, "gpt-6.1-sol", "test-model")
	unknown := f.create(t, "Unknown legacy source")
	clean := f.create(t, "Known clean repository source")
	known := f.create(t, "Known dirty repository source")
	org, project := f.project.OrganizationID, f.project.ID
	now := f.service.config.Clock().UTC().Truncate(time.Microsecond)
	content := eventstore.NewContent(f.service.runDetailStore())
	unknownRef, err := content.PreparePayload(t.Context(), org, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	cleanCheckpoint := *nativeTestCheckpoint()
	cleanCheckpoint.WorktreeState, cleanCheckpoint.Resume = "clean", "fresh_checkout"
	cleanRef, err := content.PreparePayload(t.Context(), org, historyTestJSON(t, checkpointPayload{Checkpoint: &cleanCheckpoint}))
	if err != nil {
		t.Fatal(err)
	}
	dirtyRef, err := content.PreparePayload(t.Context(), org, historyTestJSON(t, checkpointPayload{Checkpoint: nativeTestCheckpoint()}))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		for _, ref := range []attachment.ArchiveReference{unknownRef, cleanRef, dirtyRef} {
			if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: now}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	admin := hostedPostgresAdmin(t, f.service)
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, seed := range []struct {
		item       tracker.NativeIssue
		count      int
		prefix     string
		checkpoint attachment.ArchiveReference
	}{
		{unknown, 11481, "legacy_unknown_", unknownRef}, {clean, 1, "z_legacy_clean_", cleanRef}, {known, 1, "z_legacy_dirty_", dirtyRef},
	} {
		var issueID int64
		if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", org, project, seed.item.WorkItemID).Scan(&issueID); err != nil {
			t.Fatal(err)
		}
		for i := range seed.count {
			id := fmt.Sprintf("%s%05d", seed.prefix, i)
			var fence int64
			if err := tx.QueryRowContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,expires_at,acquired_at,renewed_at,released_at,project_id,runner_id) VALUES($1,$2,$3,$4,$2,1,$5,$6,$6,$6,$7,$8) RETURNING fencing_token`, org, id, issueID, r.binding.MachineID, now.Add(time.Hour), now, project, r.binding.RunnerID).Scan(&fence); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), "INSERT INTO runner_lease_assignments(organization_id,lease_id,runner_id,assigned_at) VALUES($1,$2,$3,$4)", org, id, r.binding.RunnerID, now); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), "INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) VALUES($1,$2,$3)", org, id, issueID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,started_at,updated_at) VALUES($1,$2,$3,$4,$3,$5,$3,1,'interrupted',1,0,$6,$7,$8,$8)`, org, project, id, issueID, fence, unknownRef.Key, seed.checkpoint.Key, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, positive := range []struct {
		id         string
		issue      tracker.NativeIssue
		checkpoint *tracker.NativeCheckpoint
	}{{"z_legacy_clean_00000", clean, &cleanCheckpoint}, {"z_legacy_dirty_00000", known, nativeTestCheckpoint()}} {
		var attempt leasestore.Attempt
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			attempt, err = r.Attempt(t.Context(), positive.id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		run := tracker.NativeRunData{AttemptID: attempt.ID, RunID: attempt.RunID, LeaseID: tracker.LeaseID(attempt.LeaseID), FencingToken: tracker.FencingToken(attempt.FencingToken), Sequence: attempt.Sequence, RunnerID: r.binding.RunnerID, MachineID: r.binding.MachineID, SessionID: attempt.LeaseID, Outcome: "cancelled", Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "completed", HeartbeatAt: now}, Handoff: positive.checkpoint}
		ref, err := content.PreparePayload(t.Context(), org, historyTestJSON(t, run))
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(t.Context(), org, historyTestJSON(t, tracker.CollaborationEvent{ID: "event_" + attempt.ID, OrganizationID: org, ProjectID: project, AggregateID: positive.issue.WorkItemID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: r.binding.RunnerID}, RecordedAt: now, Data: tracker.CollaborationData{Run: &run}}))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: now}); err != nil {
				return err
			}
			_, err := w.AppendEvent(t.Context(), event)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET payload_object_key=$3,status='cancelled' WHERE organization_id=$1 AND id=$2", org, attempt.ID, ref.Key); err != nil {
			t.Fatal(err)
		}
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"display_name": "Example runner", "capacity": 1, "version": "test", "backend_isolation": r.redemption.BackendIsolation})
	requireNativeStatus(t, response, http.StatusOK)
	var snapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, response, &snapshot)
	if snapshot.RunnerID != r.binding.RunnerID || snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 0 {
		t.Fatalf("heartbeat lost routing or offered unverified historical workers: %+v", snapshot)
	}
	after := ""
	processed := 0
	for tick := range 12 {
		for range min(1000, 11483-processed) {
			next, err := f.service.populateCheckpointSource(t.Context(), org, after)
			if err != nil && !errors.Is(err, artifact.ErrIntegrity) && !errors.Is(err, corestore.ErrNotFound) {
				t.Fatal(err)
			}
			if next == "" || next <= after {
				t.Fatalf("historical classifier did not advance: previous=%q next=%q error=%v", after, next, err)
			}
			after = next
			processed++
		}
		var unknownRows int
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND project_id=$2 AND checkpoint_preserves_source IS NULL", org, project).Scan(&unknownRows); err != nil {
			t.Fatal(err)
		}
		if want := 11483 - min(2, max(0, processed-11481)); unknownRows != want {
			t.Fatalf("tick %d: unknown=%d want=%d", tick+1, unknownRows, want)
		}
	}
	if processed != 11483 {
		t.Fatalf("historical population processed=%d want=11483", processed)
	}
	var validClean, validDirty, unknownCleanup int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FILTER (WHERE checkpoint_preserves_source IS FALSE),count(*) FILTER (WHERE checkpoint_preserves_source IS TRUE),count(*) FILTER (WHERE worker_cleanup_required IS NULL) FROM native_attempts WHERE organization_id=$1 AND project_id=$2", org, project).Scan(&validClean, &validDirty, &unknownCleanup); err != nil {
		t.Fatal(err)
	}
	if validClean != 1 || validDirty != 1 || unknownCleanup != 11481 {
		t.Fatalf("classification clean=%d dirty=%d unknown_cleanup=%d", validClean, validDirty, unknownCleanup)
	}
	r.reportCapacity(t)
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, tracker.NativeClaim{
		PolicyID: hubTestPolicy().ID, MachineID: r.binding.MachineID, SessionID: "next-candidate", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
		ProviderCandidates: append(providerClaim(r, unknown, "").ProviderCandidates, providerClaim(r, known, "").ProviderCandidates...),
	})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != known.WorkItemID {
		t.Fatalf("claimed %s, want %s", lease.WorkItemID, known.WorkItemID)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", r.redemption.Credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "released"}), http.StatusNoContent)
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", r.redemption.Credential, providerClaim(r, clean, "clean-candidate"))
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &lease)
	if lease.WorkItemID != clean.WorkItemID {
		t.Fatalf("known clean source was not eligible: got=%s want=%s", lease.WorkItemID, clean.WorkItemID)
	}
}

type cleanupPollingContextKey struct{}
type cleanupPollingDetails struct {
	attachment.DetailStore
	checkpoint string
	reads      atomic.Int64
}

func (d *cleanupPollingDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ctx.Value(cleanupPollingContextKey{}) == true && ref.Key == d.checkpoint {
		d.reads.Add(1)
	}
	return d.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestReleasedExecutionPollingBudget(t *testing.T) {
	for _, test := range []struct {
		history    int
		unresolved bool
		pending    int
	}{{0, false, 101}, {1000, false, 101}, {0, true, 101}, {1000, true, 101}, {0, true, 100}} {
		t.Run(fmt.Sprintf("history=%d/unresolved=%v/pending=%d", test.history, test.unresolved, test.pending), func(t *testing.T) {
			t.Parallel()
			f := newPostgresDefaultNativeFixture(t, Config{RunDetails: &memoryRunDetails{}})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			installReportsBudgetTrace(t, f.service)
			admin := hostedPostgresAdmin(t, f.service)
			now := f.service.config.Clock().UTC().Truncate(time.Microsecond)
			org, project := f.project.OrganizationID, f.project.ID
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				identity, err := w.RunnerIdentity(t.Context(), r.binding.RunnerID)
				if err != nil {
					return err
				}
				identity.DisplayName = "Example snapshot runner"
				identity.Tags = []string{"example", "snapshot"}
				identity.ProjectCapacityLimits = map[tracker.ProjectID]int{project: 1}
				identity.ProjectRanks = map[tracker.ProjectID]int{project: 3}
				identity.CapacityRequest = &runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("c", 64), Capacity: 2, Backend: "codex"}
				identity.Availability = runnerstore.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}, WindowSlots: map[string]int{"Mon-Sun 00:00-24:00": 1}, HardDeadline: "30m"}
				return w.UpdateRunnerRouting(t.Context(), identity.Revision, identity)
			}); err != nil {
				t.Fatal(err)
			}
			var repositoryRequest struct {
				RequestID string `json:"request_id"`
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/onboarding/repositories", testHubAdminToken, map[string]any{"runner_id": r.binding.RunnerID, "idempotency_key": "snapshot-repositories"})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &repositoryRequest)
			var expectedRouting runnerauth.Routing
			var expectedRevision int64
			if err := f.service.config.TenantRepository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				record, err := reader.(runnerstore.RoutingReader).RunnerRoutingRecord(t.Context(), r.binding.RunnerID, now)
				if err != nil {
					return err
				}
				urgent, err := reader.RunnerUrgentUpdate(t.Context())
				if err != nil {
					return err
				}
				previous := runnerFromRoutingRecord(record, now, urgent)
				expectedRouting, expectedRevision = previous.Routing, previous.Revision
				var direct runnerauth.RoutingSnapshot
				if err := readRunnerResponseRouting(t.Context(), reader, record.Record.Identity, &direct); err != nil {
					return err
				}
				if direct.Revision != expectedRevision || !reflect.DeepEqual(direct.Routing, expectedRouting) || direct.RepositoryListRequest != repositoryRequest.RequestID {
					t.Fatalf("registration routing changed: previous=%+v direct=%+v", expectedRouting, direct)
				}
				if direct.Routing.CapacityRequest == nil || direct.Routing.CapacityRequest.Capacity != 2 || direct.Routing.ProjectRankOverrides[project] != 3 {
					t.Fatalf("nonempty routing fields missing: %+v", direct.Routing)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			credential, status, err := f.service.authenticateTenantAPIHash(t.Context(), org, apikey.HashToken(r.redemption.Credential), "", "")
			if err != nil || status != http.StatusOK {
				t.Fatalf("cleanup authority status=%d error=%v", status, err)
			}
			scope := hubscope.NativeScope{Organization: org, Project: project, Credential: credential}
			content := eventstore.NewContent(f.service.runDetailStore())
			checkpoint, err := content.PreparePayload(t.Context(), org, historyTestJSON(t, checkpointPayload{Checkpoint: nativeTestCheckpoint()}))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: "archive_" + checkpoint.SHA256, Reference: checkpoint, CreatedAt: now})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			pending := map[string]bool{}
			for i := range test.pending + test.history {
				id := fmt.Sprintf("cleanup-%04d", i)
				issue := seedAnalyticsTestWorkItem(t, f.service, org, project, "wi_"+id, 100000+i, "Example issue", "Todo", now.Add(-time.Hour))
				var issueRow int64
				if err := admin.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", org, project, issue.WorkItemID).Scan(&issueRow); err != nil {
					t.Fatal(err)
				}
				required := i < test.pending || i%2 == 0
				var acknowledged any
				role := "implement"
				switch {
				case i < test.pending:
					pending[id] = true
				case i%4 == 2:
					role = ""
				case required:
					acknowledged = now
				}
				if _, err := admin.ExecContext(t.Context(), "INSERT INTO native_attempt_runs(organization_id,run_id,issue_id)VALUES($1,$2,$3)", org, "run_"+id, issueRow); err != nil {
					t.Fatal(err)
				}
				var fence int64
				if err := admin.QueryRowContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,expires_at,acquired_at,renewed_at)VALUES($1,$2,$3,$4,$2,1,$5,$6,$6)RETURNING fencing_token`, org, id, issueRow, r.binding.MachineID, now.Add(time.Hour), now.Add(-time.Hour)).Scan(&fence); err != nil {
					t.Fatal(err)
				}
				data := tracker.NativeRunData{AttemptID: id, RunID: "run_" + id, LeaseID: tracker.LeaseID(id), FencingToken: tracker.FencingToken(fence), RunnerID: r.binding.RunnerID, MachineID: r.binding.MachineID, SessionID: id, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: role, Backend: "codex", Model: "test-model"}, Runtime: &tracker.NativeRuntimeObservation{Phase: "implementation"}}
				if required {
					data.Runtime.WorkerLaunch = &tracker.NativeWorkerLaunch{PID: 123, ProcessStart: now.Add(-time.Hour), ExecutionVersion: "example", ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64), ControlVersion: "example", Protocol: 1, SelectedAt: now.Add(-time.Hour)}
				}
				ref, err := content.PreparePayload(t.Context(), org, historyTestJSON(t, data))
				if err != nil {
					t.Fatal(err)
				}
				event, err := content.PrepareEvent(t.Context(), org, historyTestJSON(t, tracker.CollaborationEvent{ID: "event_" + id, OrganizationID: org, ProjectID: project, AggregateID: issue.WorkItemID, Type: "run.observed", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: r.binding.RunnerID}, RecordedAt: now, Data: tracker.CollaborationData{Run: &data}}))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,checkpoint_object_key,started_at,updated_at,cleanup_acknowledged_at)VALUES($1,$2,$3,$4,$3,$5,$6,1,'cancelled',1,1,$7,$8,$9,$10,$11)`, org, project, id, issueRow, fence, data.RunID, ref.Key, checkpoint.Key, now.Add(-time.Hour), now, acknowledged); err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: now}); err != nil {
						return err
					}
					if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: id, RunnerID: r.binding.RunnerID, AssignedAt: now.Add(-time.Hour)}); err != nil {
						return err
					}
					_, err := w.AppendEvent(t.Context(), event)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), "UPDATE leases SET released_at=$3 WHERE organization_id=$1 AND lease_id=$2", org, id, now); err != nil {
					t.Fatal(err)
				}
			}
			after := ""
			for range test.pending + test.history {
				next, err := f.service.populateCheckpointSource(t.Context(), org, after)
				if err != nil {
					t.Fatal(err)
				}
				if next == "" || next <= after {
					t.Fatalf("cleanup summary recovery did not advance: previous=%q next=%q", after, next)
				}
				after = next
			}
			var unknownSummaries int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_attempts WHERE organization_id=$1 AND project_id=$2 AND (worker_cleanup_required IS NULL OR runtime_summary_sequence IS DISTINCT FROM sequence)", org, project).Scan(&unknownSummaries); err != nil {
				t.Fatal(err)
			}
			if unknownSummaries != 0 {
				t.Fatalf("verified run summaries remain unknown: %d", unknownSummaries)
			}
			if test.history == 1000 {
				inputs := `WITH inputs AS (SELECT $1::text AS organization_id,$2::text AS project_id,$3::text AS runner_id,$4::text AS machine_id,$5::timestamptz AS observed_at) `
				for _, statement := range []string{
					`INSERT INTO machines(organization_id,id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at) SELECT m.organization_id,'plan_machine_'||n,'Example peer machine','Example peer',m.capacity,m.version,m.last_heartbeat_at,m.registered_at,m.updated_at FROM inputs x JOIN machines m ON m.organization_id=x.organization_id AND m.id=x.machine_id CROSS JOIN generate_series(1,1000)n`,
					`INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at,expires_at,key_kind) SELECT r.organization_id,'plan_token_'||n,'Example peer token '||n,repeat(md5('plan-token-'||n),2),md5('plan-token-'||n),c.scope,c.native_only,c.created_at,c.updated_at,c.expires_at,c.key_kind FROM inputs x JOIN runner_identities r ON r.organization_id=x.organization_id AND r.runner_id=x.runner_id JOIN api_tokens c ON c.organization_id=r.organization_id AND c.id=r.token_id CROSS JOIN generate_series(1,1000)n`,
					`INSERT INTO runner_enrollments(organization_id,id,runner_id,machine_id,token_hash,scope,shared_machine,operations,created_at,expires_at,created_by,redeemed_at) SELECT e.organization_id,'plan_enrollment_'||n,'plan_runner_'||n,'plan_machine_'||n,repeat(md5('plan-enrollment-'||n),2),e.scope,e.shared_machine,e.operations,e.created_at,e.expires_at,e.created_by,e.redeemed_at FROM inputs x JOIN runner_identities r ON r.organization_id=x.organization_id AND r.runner_id=x.runner_id JOIN runner_enrollments e ON e.organization_id=r.organization_id AND e.id=r.enrollment_id CROSS JOIN generate_series(1,1000)n`,
					`INSERT INTO runner_identities(organization_id,runner_id,machine_id,token_id,enrollment_id,scope,operations,display_name,capacity_limit,reported_capacity,os,architecture,created_at,expires_at,last_heartbeat_at,state,revision,tags,machine_use,isolation_tier,host_services,availability_timezone,availability_hard_deadline) SELECT r.organization_id,'plan_runner_'||n,'plan_machine_'||n,'plan_token_'||n,'plan_enrollment_'||n,r.scope,r.operations,'Example peer runner',r.capacity_limit,r.reported_capacity,r.os,r.architecture,r.created_at,r.expires_at,r.last_heartbeat_at,r.state,1,r.tags,r.machine_use,r.isolation_tier,r.host_services,r.availability_timezone,r.availability_hard_deadline FROM inputs x JOIN runner_identities r ON r.organization_id=x.organization_id AND r.runner_id=x.runner_id CROSS JOIN generate_series(1,1000)n`,
					`INSERT INTO runner_repository_lists(organization_id,runner_id,request_id,pending,reason,listed_at) SELECT l.organization_id,'plan_runner_'||n,'plan_request_'||n,l.pending,l.reason,l.listed_at FROM inputs x JOIN runner_repository_lists l ON l.organization_id=x.organization_id AND l.runner_id=x.runner_id CROSS JOIN generate_series(1,1000)n`,
					`INSERT INTO projects(organization_id,id,name,slug,profile) SELECT x.organization_id,'plan_project_'||n,'Example plan project '||n,'example-plan-project-'||n,'native' FROM inputs x CROSS JOIN generate_series(1,1000)n`,
					`INSERT INTO policy_revisions(organization_id,project_id,policy_id,approved_by,approved_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,gate_github_pull_request) SELECT p.organization_id,'plan_project_'||n,p.policy_id,p.approved_by,p.approved_at,p.payload_object_key,p.payload_sha256,p.payload_bytes,p.payload_content_bytes,p.gate_github_pull_request FROM inputs x JOIN project_policies selected_policy ON selected_policy.organization_id=x.organization_id AND selected_policy.project_id=x.project_id JOIN policy_revisions p ON p.organization_id=selected_policy.organization_id AND p.project_id=selected_policy.project_id AND p.policy_id=selected_policy.policy_id CROSS JOIN generate_series(1,1000)n`,
					`INSERT INTO policy_revisions(organization_id,project_id,policy_id,approved_by,approved_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,gate_github_pull_request) SELECT p.organization_id,p.project_id,p.policy_id||'_superseded_'||n,p.approved_by,p.approved_at,p.payload_object_key,p.payload_sha256,p.payload_bytes,p.payload_content_bytes,p.gate_github_pull_request FROM inputs x JOIN project_policies selected_policy ON selected_policy.organization_id=x.organization_id AND selected_policy.project_id=x.project_id JOIN policy_revisions p ON p.organization_id=selected_policy.organization_id AND p.project_id=selected_policy.project_id AND p.policy_id=selected_policy.policy_id CROSS JOIN generate_series(1,1000)n`,
					`INSERT INTO project_policies(organization_id,project_id,policy_id) SELECT p.organization_id,'plan_project_'||n,p.policy_id FROM inputs x JOIN project_policies p ON p.organization_id=x.organization_id AND p.project_id=x.project_id CROSS JOIN generate_series(1,1000)n`,
					`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,expires_at,acquired_at,renewed_at,project_id,runner_id) SELECT organization_id,'plan_lease_'||n,id,'plan_machine_'||n,'plan_session_'||n,revision,observed_at+interval '1 hour',observed_at,observed_at,project_id,'plan_runner_'||n FROM (SELECT i.organization_id,i.id,i.revision,i.project_id,x.observed_at,row_number()OVER(ORDER BY i.id)n FROM inputs x JOIN issues i ON i.organization_id=x.organization_id AND i.project_id=x.project_id ORDER BY i.id LIMIT 1000) peers`,
					`INSERT INTO runner_lease_assignments(organization_id,lease_id,runner_id,assigned_at) SELECT l.organization_id,l.lease_id,l.runner_id,l.acquired_at FROM inputs x JOIN leases l ON l.organization_id=x.organization_id WHERE l.lease_id IN(SELECT 'plan_lease_'||n FROM generate_series(1,1000)n)`,
				} {
					if _, err := admin.ExecContext(t.Context(), inputs+statement, org, project, r.binding.RunnerID, r.binding.MachineID, now); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := admin.ExecContext(t.Context(), "ANALYZE machines,runner_repository_lists,project_policies,policy_revisions,runner_identities,api_tokens,projects,runner_enrollments"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := admin.ExecContext(t.Context(), "ANALYZE issues,native_attempts,leases,runner_lease_assignments"); err != nil {
				t.Fatal(err)
			}
			original := f.service.config.RunDetails
			observed := &cleanupPollingDetails{DetailStore: f.service.runDetailStore(), checkpoint: checkpoint.Key}
			f.service.config.RunDetails = observed
			t.Cleanup(func() { f.service.config.RunDetails = original })
			pages := []int{100, 1, 0}
			if test.unresolved {
				pages = []int{100, test.pending - 100, 100, 0}
			}
			for poll, want := range pages {
				var mu sync.Mutex
				var statements []tracedStatement
				timing := &requestTiming{Trace: func(query string, args []driver.NamedValue) {
					values := make([]any, len(args))
					for i, arg := range args {
						values[i] = arg.Value
					}
					mu.Lock()
					statements = append(statements, tracedStatement{query, values})
					mu.Unlock()
				}}
				ctx := context.WithValue(withRequestTiming(t.Context(), timing), cleanupPollingContextKey{}, true)
				snapshot := runnerauth.RoutingSnapshot{RunnerID: r.binding.RunnerID}
				if err := f.service.readRegistrationClaimStateTenant(ctx, scope, &snapshot); err != nil {
					t.Fatal(err)
				}
				if snapshot.ClaimState == nil {
					t.Fatal("cleanup poll omitted claim state")
				}
				if snapshot.Revision != expectedRevision || !reflect.DeepEqual(snapshot.Routing, expectedRouting) || snapshot.RepositoryListRequest != repositoryRequest.RequestID {
					t.Fatalf("optimized registration routing changed: expected=%+v actual=%+v", expectedRouting, snapshot)
				}
				mu.Lock()
				captured := append([]tracedStatement(nil), statements...)
				mu.Unlock()
				queryCount := 0
				combinedReads := 0
				for _, statement := range captured {
					if !reportsBudgetReadStatement(statement.query) {
						continue
					}
					queryCount++
					if strings.Contains(statement.query, "checkpoint_json") {
						t.Errorf("cleanup poll read an inline source checkpoint: %s", statement.query)
					}

					if strings.HasPrefix(strings.TrimSpace(statement.query), "WITH routing AS MATERIALIZED (") {
						combinedReads++
						if poll == 0 && test.history == 1000 {
							plan, raw := reportsExplainPostgresPlan(t, admin, org, statement, "enable_seqscan=off", "enable_bitmapscan=off", "enable_sort=off", "enable_hashjoin=off", "enable_mergejoin=off")
							maximumLimit := 0
							for _, match := range reportsBudgetLimit.FindAllStringSubmatch(statement.query, -1) {
								value := match[2]
								if match[1] != "" {
									position, err := strconv.Atoi(match[1])
									if err != nil || position < 1 || position > len(statement.args) {
										t.Fatalf("snapshot limit bind missing: %v", match)
									}
									value = fmt.Sprint(statement.args[position-1])
								}
								limit, err := strconv.Atoi(value)
								if err != nil || limit < 1 || limit > 10001 {
									t.Fatalf("snapshot source limit exceeds owner bound: %s", value)
								}
								maximumLimit = max(maximumLimit, limit)
							}
							if maximumLimit != 10001 {
								t.Fatalf("snapshot omitted lease sentinel bound: %d", maximumLimit)
							}
							pointKeys := map[string][]string{"machines": {"organization_id", "id"}, "runner_repository_lists": {"organization_id", "runner_id"}, "project_policies": {"organization_id", "project_id"}, "policy_revisions": {"organization_id", "project_id", "policy_id"}, "leases": {"organization_id", "machine_id"}}
							seen := map[string]bool{}
							var inspect func(reportsBudgetPlan, bool)
							inspect = func(node reportsBudgetPlan, limited bool) {
								limited = limited || node.Node == "Limit"
								if keys, required := pointKeys[node.Relation]; required && node.Loops > 0 {
									seen[node.Relation] = true
									conditions, indexed := reportsBudgetConditions(node, node.Relation)
									if strings.Contains(node.Node, "Seq Scan") || !indexed {
										t.Errorf("snapshot %s is not an indexed seek: node=%s plan=%s", node.Relation, node.Node, raw)
									}
									for _, key := range keys {
										if !strings.Contains(conditions, key+" =") {
											t.Errorf("snapshot %s lacks point key %s: %s plan=%s", node.Relation, key, conditions, raw)
										}
									}
									visits := (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
									if node.Relation == "leases" {
										if !limited || visits > 10001 {
											t.Errorf("snapshot leases are unbounded: limited=%v visits=%v plan=%s", limited, visits, raw)
										}
									} else if visits > 1 {
										t.Errorf("snapshot %s visits %v rows for one key: plan=%s", node.Relation, visits, raw)
									}
								}
								for _, child := range node.Plans {
									inspect(child, limited)
								}
							}
							inspect(plan, false)
							for relation := range pointKeys {
								if !seen[relation] {
									t.Errorf("snapshot plan did not execute %s lookup: %s", relation, raw)
								}
							}
						}
					}
					if strings.HasPrefix(statement.query, "-- name: RunnerReleasedAttemptCandidates :many") {
						plan, raw := reportsExplainPostgresPlan(t, admin, org, statement, "enable_seqscan=off", "enable_bitmapscan=off", "enable_sort=off", "enable_hashjoin=off", "enable_mergejoin=off")
						if violations := reportsBudgetPlanViolations(statement, plan); len(violations) > 0 {
							t.Errorf("cleanup poll scans history or sorts: %v plan=%s", violations, raw)
						}
					}
				}
				if combinedReads != 1 {
					t.Errorf("poll omitted or repeated actual combined snapshot read: %d", combinedReads)
				}
				if queryCount != 4 || len(snapshot.ClaimState.ReleasedExecutions) != want {
					t.Errorf("poll %d with %d history: queries=%d executions=%d want=4,%d", poll, test.history, queryCount, len(snapshot.ClaimState.ReleasedExecutions), want)
				}
				if got := observed.reads.Swap(0); got != 0 {
					t.Errorf("cleanup poll hydrated %d source checkpoints", got)
				}
				for _, reference := range snapshot.ClaimState.ReleasedExecutions {
					if !pending[reference.AttemptID] {
						t.Fatalf("history or acknowledged cleanup returned: %+v", reference)
					}
					if test.unresolved && poll == 0 {
						continue
					}
					delete(pending, reference.AttemptID)
					if _, err := admin.ExecContext(t.Context(), "UPDATE native_attempts SET cleanup_acknowledged_at=$3 WHERE organization_id=$1 AND id=$2", org, reference.AttemptID, now); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
