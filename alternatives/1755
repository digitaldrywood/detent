package hubserver

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHeartbeatScaleBenchmark(t *testing.T) {
	f := newNativeFixture(t, nil, "", "heartbeat-scale")
	projects := []nativeFixture{f}
	for i := 1; i < 4; i++ {
		projects = append(projects, newNativeFixture(t, f.service, f.project.OrganizationID, fmt.Sprintf("heartbeat-scale-%d", i)))
	}
	runners := make([]runnerFixture, 3)
	for i := range runners {
		runners[i] = prepareRunnerForScope(t, f, "organization", runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
		runners[i].enroll(t)
	}
	r := runners[0]
	now := f.service.config.now()
	at := formatHubTime(now)
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := range 1500 {
		state := "Done"
		if i < 50 {
			state = "Todo"
		}
		result, err := tx.ExecContext(t.Context(), `INSERT INTO issues(workflow_state_id,native_id,organization_id,project_id,number,title,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at,native_created_at,native_updated_at) VALUES((SELECT id FROM workflow_states WHERE project_id=? AND source_name=?),?,?,?,?,?,'','open','v1',?,?,?,?,?,?)`, f.project.ID, state, fmt.Sprintf("wi_scale_%d", i), f.project.OrganizationID, f.project.ID, i+1, "Example issue", at, at, at, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
		if i >= 300 {
			continue
		}
		issue, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("scale-attempt-%d", i)
		lease, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, issue, r.binding.MachineID, id, at, at, at, at, at, at)
		if err != nil {
			t.Fatal(err)
		}
		fence, err := lease.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)`, id, r.binding.RunnerID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,checkpoint_json,started_at,updated_at,worker_cleanup_required,runner_id) VALUES(?,?,?,?,?,?,?,1,'interrupted','{}',?,?,?,1,?)`, id, f.project.OrganizationID, f.project.ID, fmt.Sprintf("wi_scale_%d", i), id, fence, id, `{"worktree_state":"dirty","resume":"checkpoint","ref":"refs/heads/example","sha":"`+strings.Repeat("a", 40)+`"}`, at, at, r.binding.RunnerID); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, runner := range runners {
		for _, project := range projects {
			view := runnerauth.ProjectConfiguration{ProjectID: string(project.project.ID), Authority: "local_global_configuration", Source: "cloud_workflow", ObservedAt: now}
			response := performHubAPIRequest(t, f.service, http.MethodPost, project.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"capacity": 4, "version": "test", "project_configuration": view})
			requireNativeStatus(t, response, http.StatusOK)
		}
	}
	f.service.tokenUseWork.Wait()
	var writer *writerTiming
	f.service.echo.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			writer = writerTimingFrom(c.Request().Context())
			return err
		}
	})
	path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	totals := map[string]time.Duration{}
	var hold, db time.Duration
	var count int
	result := testing.Benchmark(func(b *testing.B) {
		totals = map[string]time.Duration{}
		hold, db, count = 0, 0, 0
		for b.Loop() {
			timing := &requestTiming{}
			previous, last := "", time.Now()
			timing.trace = func(_ string, _ []driver.NamedValue) {
				current := time.Now()
				if previous != "" {
					totals[previous] += current.Sub(last)
				}
				pcs := make([]uintptr, 48)
				n := runtime.Callers(2, pcs)
				frames := runtime.CallersFrames(pcs[:n])
				previous = "transaction_other"
				for {
					frame, more := frames.Next()
					for _, name := range []string{"readRunnerRoutingSnapshot", "mergeRunnerHostMetrics", "readRunnerClaimState", "readHeartbeatChanges", "runnerProjectConfiguration", "readGitHubBatchTask", "runnerDispatchFingerprint", "hostedConsumption", "checkHostedGrowth", "requireCredentialAuthority", "updateRunnerHeartbeat", "storeEncodedRunner", "updateRunnerProblems", "updateProviderReports", "storeRunnerUpdateObservation"} {
						if strings.Contains(frame.Function, name) {
							previous = name
						}
					}
					if !more {
						break
					}
				}
				last = time.Now()
			}
			request := newHubAPIRequest(t, http.MethodPost, path, r.redemption.Credential, map[string]any{"capacity": 4, "version": "test", "change_cursor": "", "backend_isolation": r.redemption.BackendIsolation})
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(request.Context(), timing)))
			requireNativeStatus(t, response, http.StatusOK)
			var snapshot runnerauth.RoutingSnapshot
			if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			if snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) > releasedExecutionsPageSize {
				t.Fatalf("released executions = %+v", snapshot.ClaimState)
			}
			if writer == nil {
				t.Fatal("missing writer timing")
			}
			hold += time.Duration(writer.hold.Load())
			db += time.Duration(timing.db.Load())
			count++
		}
	})
	t.Logf("benchmark=%s issues=1500 nonterminal=50 released=300 runners=3 projects=4 writer_mean_ms=%.3f db_mean_ms=%.3f", result.String(), float64(hold)/float64(count)/1e6, float64(db)/float64(count)/1e6)
	names := make([]string, 0, len(totals))
	for name := range totals {
		names = append(names, name)
	}
	slices.SortFunc(names, func(a, b string) int {
		if totals[a] > totals[b] {
			return -1
		}
		if totals[a] < totals[b] {
			return 1
		}
		return 0
	})
	for _, name := range names {
		t.Logf("operation=%s mean_ms=%.3f", name, float64(totals[name])/float64(count)/1e6)
	}
}

func TestHeartbeatResponseReaderSnapshot(t *testing.T) {
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
			path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, map[string]any{"capacity": 0, "version": "test"}), http.StatusOK)
			f.service.tokenUseWork.Wait()
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
			observed := false
			removed := false
			heartbeatWritten := false
			if test.revokeProject {
				now := f.service.database.now
				f.service.database.now = func() time.Time {
					if heartbeatWritten && !removed && f.service.database.db.Stats().InUse == 0 {
						removed = true
						change := runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{
							DisplayName: "Example runner", State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{},
						}}
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, change), http.StatusOK)
					}
					return now()
				}
				defer func() { f.service.database.now = now }()
			}
			timing := &requestTiming{}
			timing.trace = func(query string, _ []driver.NamedValue) {
				if strings.HasPrefix(query, "UPDATE runner_identities SET") {
					heartbeatWritten = true
				}
				if observed || !strings.HasPrefix(query, "SELECT m.capacity, min(r.capacity_limit, r.reported_capacity)") {
					return
				}
				observed = true
				if f.service.database.db.Stats().InUse != 0 {
					t.Fatal("heartbeat response still holds the writer")
				}
				var capacity int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT reported_capacity FROM runner_identities WHERE id=?", r.binding.RunnerID).Scan(&capacity); err != nil {
					t.Fatal(err)
				}
				if capacity != test.capacity {
					t.Fatalf("committed capacity=%d want=%d", capacity, test.capacity)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET reported_capacity=0 WHERE id=?", r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				if test.failRead {
					cancel()
				}
			}
			summary := testHostSummary(f.service.config.now())
			request := newHubAPIRequest(t, http.MethodPost, path, r.redemption.Credential, map[string]any{"capacity": test.capacity, "version": "test", "change_cursor": "", "host_metrics": []hostmetrics.Summary{summary}})
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request.WithContext(withRequestTiming(ctx, timing)))
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
				var capacity int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT reported_capacity FROM runner_identities WHERE id=?", r.binding.RunnerID).Scan(&capacity); err != nil || capacity != test.capacity {
					t.Fatalf("committed capacity=%d want=%d error=%v", capacity, test.capacity, err)
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
				if snapshot.ClaimState == nil || snapshot.ClaimState.RunnerCapacity != test.capacity {
					t.Fatalf("response did not preserve reader snapshot: %+v", snapshot.ClaimState)
				}
			}
			var capacity int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT reported_capacity FROM runner_identities WHERE id=?", r.binding.RunnerID).Scan(&capacity); err != nil {
				t.Fatal(err)
			}
			if capacity != 0 {
				t.Fatalf("concurrent writer capacity=%d want=0", capacity)
			}
		})
	}
}

func TestHeartbeatResponseArchivedCleanup(t *testing.T) {
	for _, concurrent := range []int{1, readerConnections} {
		t.Run(fmt.Sprintf("responses=%d", concurrent), func(t *testing.T) {
			store := &memoryRunDetails{}
			f := newDefaultNativeFixture(t, Config{RunDetails: store})
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events)
			r.enroll(t)
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
			now := f.service.config.now()
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
			var raw string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT checkpoint_object_key FROM native_attempts WHERE id=?", event.Data.AttemptID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			checkpoint, err := decodeRunCheckpoint(f.service.runDetailContext(t.Context()), f.service.database.reader, raw)
			if err != nil || checkpoint == nil {
				t.Fatalf("checkpoint is not archived: %s error=%v", raw, err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET worker_cleanup_required=NULL,execution_role='' WHERE id=?", event.Data.AttemptID); err != nil {
				t.Fatal(err)
			}
			if err := f.service.convertAttemptHotProjection(t.Context(), event.Data.AttemptID); err != nil {
				t.Fatal(err)
			}
			if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
				t.Fatal(err)
			}
			f.service.stopSpriteRunners()
			f.service.tokenUseWork.Wait()
			store.mu.Lock()
			store.reads = 0
			store.mu.Unlock()
			results := make(chan *httptest.ResponseRecorder, concurrent)
			for range concurrent {
				request := newHubAPIRequest(t, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"capacity": 1, "version": "test"})
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
			tables := make([]attachment.ArchiveTable, 0, 4)
			for _, name := range nativeArchiveTables("run") {
				table := archiveTestTable(t, f.service, name, "0")
				if name == "native_attempts" {
					table = archiveTestTable(t, f.service, name, "id=?", event.Data.AttemptID)
					index := slices.Index(table.Columns, "data_json")
					table.Columns = slices.Delete(table.Columns, index, index+1)
					for i := range table.Rows {
						table.Rows[i] = slices.Delete(table.Rows[i], index, index+1)
					}
				}
				tables = append(tables, table)
			}
			_, archived := publishTestSection(t, f, string(issue.WorkItemID), "run", event.Data.AttemptID, 0, 0, tables...)
			archiveReads := 0
			f.service.config.ArchiveReader = func(context.Context, attachment.ArchiveReference, bool) (entryauthority.ArchiveResponse, error) {
				archiveReads++
				return entryauthority.ArchiveResponse{Data: archived}, nil
			}
			attemptPath := f.base + "/work-items/" + string(issue.WorkItemID) + "/attempts/" + event.Data.AttemptID
			response = performHubAPIRequest(t, f.service, http.MethodGet, attemptPath, worker, nil)
			requireNativeStatus(t, response, http.StatusServiceUnavailable)
			if !strings.Contains(response.Body.String(), "archive_incompatible") {
				t.Fatalf("expected incompatible archive: %s", response.Body.String())
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, attemptPath+"?view=cleanup", worker, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var attempt tracker.NativeAttempt
			decodeHubResponse(t, response, &attempt)
			if attempt.Runtime == nil || attempt.Runtime.WorkerLaunch == nil || attempt.Runtime.LocalAttemptID != 1 || attempt.Identity == nil || *attempt.Identity != *event.Data.Identity || attempt.ClaimReleasedAt == nil || attempt.Checkpoint != nil || len(attempt.ArtifactIDs) != 0 || archiveReads != 1 {
				t.Fatalf("cleanup projection lost proof or loaded historical evidence: %+v archive reads=%d", attempt, archiveReads)
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
			decodeHubResponse(t, response, &attempt)
			if attempt.CleanupAcknowledgedAt == nil || archiveReads != 1 {
				t.Fatalf("cleanup receipt missing: %+v archive reads=%d", attempt, archiveReads)
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", worker, map[string]any{"capacity": 1, "version": "test"})
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
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	r := placementFixtureRunner(t, f, "legacy-worker", "local", false, 1, "gpt-6.1-sol", "test-model")
	unknown := f.create(t, "Unknown legacy source")
	known := f.create(t, "Known repository source")
	at := formatHubTime(f.service.config.now())
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for index, item := range []tracker.NativeWorkItemID{unknown.WorkItemID, known.WorkItemID} {
		count := 11482
		checkpoint := `{}`
		if index == 1 {
			count = 1
			checkpoint = `{"worktree_state":"dirty","resume":"checkpoint","ref":"refs/heads/example","sha":"` + strings.Repeat("a", 40) + `"}`
		}
		for i := range count {
			id := fmt.Sprintf("legacy-%d-%d", index, i)
			result, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES(?,(SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?),?,?,?,?,?,?,?,?)`, id, f.project.OrganizationID, f.project.ID, item, r.binding.MachineID, id, at, at, at, at, at, at)
			if err != nil {
				t.Fatal(err)
			}
			fence, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)`, id, r.binding.RunnerID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,checkpoint_json,started_at,updated_at) VALUES(?,?,?,?,?,?,?,1,'interrupted','{}',?,?,?)`, id, f.project.OrganizationID, f.project.ID, item, id, fence, id, checkpoint, at, at); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"capacity": 1, "version": "test", "backend_isolation": r.redemption.BackendIsolation})
	requireNativeStatus(t, response, http.StatusOK)
	var snapshot runnerauth.RoutingSnapshot
	decodeHubResponse(t, response, &snapshot)
	if snapshot.RunnerID != r.binding.RunnerID || snapshot.ClaimState == nil || len(snapshot.ClaimState.ReleasedExecutions) != 0 {
		t.Fatalf("heartbeat lost routing or polled attempts without workers: %+v", snapshot)
	}
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
	for tick := range 12 {
		completed := make(chan error, 1)
		go func() {
			completed <- f.service.database.populateClaimFacts(t.Context())
		}()
		if err := <-completed; err != nil {
			t.Fatal(err)
		}
		var unknownRows int
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM native_attempts WHERE checkpoint_preserves_source IS NULL`).Scan(&unknownRows); err != nil {
			t.Fatal(err)
		}
		if want := max(0, 11483-(tick+1)*claimFactsRowsPerTick); unknownRows != want {
			t.Fatalf("tick %d: unknown=%d want=%d", tick+1, unknownRows, want)
		}
	}
}

func TestReleasedExecutionPollingBudget(t *testing.T) {
	for _, test := range []struct {
		history    int
		unresolved bool
		pending    int
	}{{0, false, 101}, {1000, false, 101}, {0, true, 101}, {1000, true, 101}, {0, true, 100}} {
		t.Run(fmt.Sprintf("history=%d/unresolved=%v/pending=%d", test.history, test.unresolved, test.pending), func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "cleanup-budget")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
			r.enroll(t)
			now := f.service.config.now()
			at := formatHubTime(now)
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			pending := map[string]bool{}
			for i := range test.pending + test.history {
				id := fmt.Sprintf("cleanup-%04d", i)
				result, err := tx.ExecContext(t.Context(), `INSERT INTO issues(workflow_state_id,native_id,organization_id,project_id,number,title,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at,native_created_at,native_updated_at) VALUES((SELECT id FROM workflow_states WHERE project_id=? AND source_name='Todo'),?,?,?,?,?,'','open','v1',?,?,?,?,?,?)`, f.project.ID, "wi_"+id, f.project.OrganizationID, f.project.ID, i+1, "Example issue", at, at, at, at, at, at)
				if err != nil {
					t.Fatal(err)
				}
				issue, err := result.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				lease, err := tx.ExecContext(t.Context(), `INSERT INTO leases(lease_id,issue_id,machine_id,session_id,expires_at,acquired_at,renewed_at,released_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, id, issue, r.binding.MachineID, id, at, at, at, at, at, at)
				if err != nil {
					t.Fatal(err)
				}
				fence, err := lease.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO lease_runners(lease_id,runner_id) VALUES(?,?)`, id, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				required := i < test.pending || i%2 == 0
				var acknowledged any
				role := "code"
				switch {
				case i < test.pending:
					pending[id] = true
				case i%4 == 2:
					role = ""
				case required:
					acknowledged = at
				}
				checkpoint := `{"worktree_state":"dirty","resume":"checkpoint","ref":"refs/heads/example","sha":"` + strings.Repeat("a", 40) + `"}`
				if _, err := tx.ExecContext(t.Context(), `INSERT INTO native_attempts(id,organization_id,project_id,work_item_id,lease_id,fencing_token,run_id,sequence,status,data_json,checkpoint_json,started_at,updated_at,runner_id,worker_cleanup_required,cleanup_acknowledged_at,execution_role) VALUES(?,?,?,?,?,?,?,1,'cancelled','{}',?,?,?,?,?,?,?)`, id, f.project.OrganizationID, f.project.ID, "wi_"+id, id, fence, id, checkpoint, at, at, r.binding.RunnerID, required, acknowledged, role); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Runner: r.identity}}
			pages := []int{100, 1, 0}
			if test.unresolved {
				pages = []int{100, test.pending - 100, 100, 0}
			}
			for poll, want := range pages {
				query := &runtimeReadQuery{nativeQueryer: f.service.database.reader}
				snapshot := runnerauth.RoutingSnapshot{RunnerID: r.binding.RunnerID}
				if err := f.service.readRunnerClaimState(t.Context(), query, scope, &snapshot, now); err != nil {
					t.Fatal(err)
				}
				if len(query.statements) != 4 || len(snapshot.ClaimState.ReleasedExecutions) != want {
					t.Fatalf("poll %d with %d history: queries=%d executions=%d want=4,%d", poll, test.history, len(query.statements), len(snapshot.ClaimState.ReleasedExecutions), want)
				}
				for index, statement := range query.statements {
					if strings.Contains(statement, "checkpoint") {
						t.Errorf("cleanup poll read a source checkpoint: %s", statement)
					}
					if strings.Contains(statement, "worker_cleanup_required") {
						plan := strings.Join(explainQueryPlan(t, f.service.database.reader, statement, query.arguments[index]), "\n")
						if !strings.Contains(plan, "USING INDEX native_attempts_pending_cleanup_idx") || strings.Contains(plan, "USE TEMP B-TREE") {
							t.Fatalf("cleanup poll scans history or sorts: %s", plan)
						}
					}
				}
				for _, reference := range snapshot.ClaimState.ReleasedExecutions {
					if !pending[reference.AttemptID] {
						t.Fatalf("history or acknowledged cleanup returned: %+v", reference)
					}
					if test.unresolved && poll == 0 {
						continue
					}
					delete(pending, reference.AttemptID)
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET cleanup_acknowledged_at=? WHERE id=?", at, reference.AttemptID); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
