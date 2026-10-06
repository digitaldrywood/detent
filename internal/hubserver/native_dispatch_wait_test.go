package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type dispatchQueryProbe struct {
	queries          atomic.Int64
	candidates       atomic.Int64
	rows             atomic.Int64
	query            atomic.Pointer[dispatchProbedQuery]
	completionBodies atomic.Int64
	maxArgumentBytes atomic.Int64
}

type dispatchProbedQuery struct {
	statement string
	args      []any
}

type dispatchProbeDriver struct {
	driver.Driver
	probe *dispatchQueryProbe
}
type dispatchProbeConn struct {
	driver.Conn
	probe *dispatchQueryProbe
}
type dispatchProbeRows struct {
	driver.Rows
	probe *dispatchQueryProbe
}

func (d dispatchProbeDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return dispatchProbeConn{conn, d.probe}, nil
}

func (c dispatchProbeConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
}

func (c dispatchProbeConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c dispatchProbeConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.probe.queries.Add(1)
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err == nil && strings.HasPrefix(query, "WITH candidates AS") {
		c.probe.candidates.Add(1)
		values := make([]any, 0, len(args))
		argumentBytes := int64(0)
		for _, arg := range args {
			values = append(values, arg.Value)
			if value, ok := arg.Value.(string); ok {
				argumentBytes += int64(len(value))
			}
		}
		for old := c.probe.maxArgumentBytes.Load(); argumentBytes > old; old = c.probe.maxArgumentBytes.Load() {
			if c.probe.maxArgumentBytes.CompareAndSwap(old, argumentBytes) {
				break
			}
		}
		c.probe.query.Store(&dispatchProbedQuery{query, values})
		return dispatchProbeRows{rows, c.probe}, nil
	}
	return rows, err
}

func (r dispatchProbeRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err == nil {
		r.probe.rows.Add(1)
		for index, column := range r.Columns() {
			if strings.Contains(column, "completion_body") && values[index] != nil && values[index] != "" {
				r.probe.completionBodies.Add(1)
			}
		}
	}
	return err
}

func measureDispatchQueries(t *testing.T, s *Service) *dispatchQueryProbe {
	t.Helper()
	if err := s.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := s.stopGitHubReconciliation(); err != nil {
		t.Fatal(err)
	}
	if err := s.database.db.Close(); err != nil {
		t.Fatal(err)
	}
	probe := &dispatchQueryProbe{}
	name := newNativeID("dispatch_probe")
	sql.Register(name, dispatchProbeDriver{&sqlite.Driver{}, probe})
	db, err := sql.Open(name, sqliteDSN(s.database.path, s.config.BusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	s.database.db = db
	return probe
}

func serveDispatchWait(ctx context.Context, f nativeFixture, token, cursor string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, f.base+"/claims/wait?wait=30&after="+url.QueryEscape(cursor), nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	f.service.echo.ServeHTTP(response, request)
	return response
}

func dispatchWaitCursor(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	requireNativeStatus(t, response, http.StatusOK)
	var page struct {
		Cursor string `json:"cursor"`
	}
	decodeHubResponse(t, response, &page)
	if page.Cursor == "" {
		t.Fatal("empty wait cursor")
	}
	return page.Cursor
}

func awaitDispatchSubscribers(t *testing.T, s *Service, organization tracker.OrganizationID, count int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.notifications.mu.Lock()
		got := len(s.notifications.subscribers[dispatchNotificationKey(organization)])
		s.notifications.mu.Unlock()
		if got == count && s.database.db.Stats().InUse == 0 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("subscribers=%d, want %d", got, count)
		case <-ticker.C:
		}
	}
}

func TestNativeDispatchWaitLifecycle(t *testing.T) {
	for _, action := range []string{"queue", "dependency", "capacity", "unchanged heartbeat", "routing", "revocation", "cancel", "shutdown", "reconnect"} {
		t.Run(action, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "wait")
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			worker := r.redemption.Credential
			probe := measureDispatchQueries(t, f.service)
			item, blocker := f.create(t, "dependent"), f.create(t, "blocker")
			cursor := dispatchWaitCursor(t, serveDispatchWait(t.Context(), f, worker, ""))
			if action == "reconnect" {
				f.create(t, "gap")
				if next := dispatchWaitCursor(t, serveDispatchWait(t.Context(), f, worker, cursor)); next == cursor {
					t.Fatal("lost reconnect notification")
				}
				return
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { result <- serveDispatchWait(ctx, f, worker, cursor) }()
			awaitDispatchSubscribers(t, f.service, f.project.OrganizationID, 1)
			queries := probe.queries.Load()
			if probe.candidates.Load() != 0 {
				t.Fatal("wait scanned candidates")
			}
			switch action {
			case "queue":
				f.create(t, "ready")
			case "dependency":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "relation"}, ExpectedRevision: item.Revision, RelatedWorkItemID: blocker.WorkItemID, Operation: "add"}), http.StatusOK)
			case "capacity", "unchanged heartbeat":
				capacity := 3
				if action == "unchanged heartbeat" {
					capacity = 2
				}
				heartbeat := map[string]any{"capacity": capacity, "version": "test", "backend_isolation": r.redemption.BackendIsolation}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", worker, heartbeat), http.StatusOK)
				if action == "unchanged heartbeat" {
					if next := f.service.notifications.dispatchCursor(dispatchNotificationKey(f.project.OrganizationID)); next != cursor {
						t.Fatal("unchanged heartbeat woke admission")
					}
					cancel()
				}
			case "routing":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", testHubAdminToken, runnerauth.RoutingChange{ExpectedRevision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "draining", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{f.project.ID}}}), http.StatusOK)
			case "revocation":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, r.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
			case "cancel":
				cancel()
			case "shutdown":
				shutdownCtx, stop := context.WithTimeout(t.Context(), time.Second)
				defer stop()
				if err := f.service.Shutdown(shutdownCtx); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case response := <-result:
				switch action {
				case "cancel", "unchanged heartbeat":
				case "shutdown":
					requireNativeStatus(t, response, http.StatusServiceUnavailable)
				case "revocation":
					requireNativeStatus(t, response, http.StatusUnauthorized)
				default:
					if next := dispatchWaitCursor(t, response); next == cursor {
						t.Fatal("notification did not advance cursor")
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("held wait did not finish")
			}
			awaitDispatchSubscribers(t, f.service, f.project.OrganizationID, 0)
			t.Logf("queries=%d candidate_queries=%d", probe.queries.Load()-queries, probe.candidates.Load())
		})
	}
}

func TestNativeDispatchWaitScale(t *testing.T) {
	for _, runners := range []int{1, 16, 64, 256} {
		for _, admissions := range []int{1, 8} {
			if admissions > runners {
				continue
			}
			t.Run(fmt.Sprintf("runners_%d_admissions_%d", runners, admissions), func(t *testing.T) {
				f := newNativeFixture(t, nil, "", "scale")
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				worker := f.worker(t, "scale-worker")
				for index := range runners {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": fmt.Sprintf("machine-%d", index), "hostname": "isolated", "capacity": 1, "version": "test"}), http.StatusOK)
				}
				probe := measureDispatchQueries(t, f.service)
				cursor := dispatchWaitCursor(t, serveDispatchWait(t.Context(), f, worker, ""))
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				results := make(chan time.Duration, runners)
				failures := make(chan int, runners)
				var began atomic.Int64
				var memoryBefore, memoryHeld runtime.MemStats
				runtime.ReadMemStats(&memoryBefore)
				requestsBefore := probe.queries.Load()
				for index := range runners {
					go func() {
						response := serveDispatchWait(ctx, f, worker, cursor)
						if response.Code != http.StatusOK {
							failures <- response.Code
							results <- 0
							return
						}
						claim := tracker.NativeClaim{MachineID: tracker.MachineID(fmt.Sprintf("machine-%d", index)), SessionID: fmt.Sprintf("session-%d", index), PolicyID: hubTestPolicy().ID, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}, WorkflowStates: []string{"Todo"}}
						claimed := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim)
						if claimed.Code == http.StatusOK {
							results <- time.Since(time.Unix(0, began.Load()))
							return
						}
						if claimed.Code != http.StatusConflict {
							failures <- claimed.Code
						}
						results <- 0
					}()
				}
				awaitDispatchSubscribers(t, f.service, f.project.OrganizationID, runners)
				runtime.ReadMemStats(&memoryHeld)
				idleReads := probe.queries.Load()
				idlePool := f.service.database.db.Stats()
				time.Sleep(50 * time.Millisecond)
				if probe.queries.Load() != idleReads || probe.candidates.Load() != 0 {
					t.Fatal("held waits performed database work")
				}
				began.Store(time.Now().UnixNano())
				credential, _, err := f.service.authenticateAPIToken(t.Context(), testHubAdminToken, "", "")
				if err != nil {
					t.Fatal(err)
				}
				scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
				_, err = f.service.executeNativeMutation(t.Context(), scope, nativeCommandOptions{OperationID: "dispatch-scale"}, tracker.Mutation{IdempotencyKey: "burst"}, admissions, func(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) (any, error) {
					var items []tracker.NativeIssue
					for index := range admissions {
						item, err := createNativeIssueTx(ctx, tx, scope, tracker.CreateIssue{Title: fmt.Sprintf("ready-%d", index), State: "Todo"}, now)
						if err != nil {
							return nil, err
						}
						items = append(items, item)
					}
					return items, nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for range 8 {
					f.service.notifications.notify(dispatchNotificationKey(f.project.OrganizationID))
				}
				granted := 0
				var worst time.Duration
				for range runners {
					select {
					case latency := <-results:
						if latency > 0 {
							granted++
							worst = max(worst, latency)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("scale request did not settle")
					}
				}
				if len(failures) > 0 {
					t.Fatalf("unexpected HTTP status %d", <-failures)
				}
				var leases, issues int
				if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*), count(DISTINCT issue_id) FROM leases WHERE released_at IS NULL").Scan(&leases, &issues); err != nil {
					t.Fatal(err)
				}
				if granted != admissions || leases != admissions || issues != admissions {
					t.Fatalf("grants=%d leases=%d unique=%d want=%d", granted, leases, issues, admissions)
				}
				pool := f.service.database.db.Stats()
				t.Logf("wait_requests=%d claim_requests=%d entry_queries=%d idle_queries=0 idle_candidates=0 held_heap_bytes=%d wait_entry_alloc_bytes=%d total_queries=%d candidate_queries=%d candidate_rows=%d idle_pool_wait=%s total_pool_wait=%s wake_to_claim_max=%s", runners, runners, idleReads-requestsBefore, memoryHeld.HeapAlloc, memoryHeld.TotalAlloc-memoryBefore.TotalAlloc, probe.queries.Load()-requestsBefore, probe.candidates.Load(), probe.rows.Load(), idlePool.WaitDuration, pool.WaitDuration, worst)
			})
		}
	}
}

func TestNativeCandidateQueryBound(t *testing.T) {
	var bindBytes int64
	for _, test := range []struct {
		size        int
		unpublished bool
	}{{size: 16}, {size: 256}, {size: 2048}, {size: 256, unpublished: true}} {
		t.Run(fmt.Sprintf("queue_%d_unpublished_%t", test.size, test.unpublished), func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "candidate-bound")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{ID: bootstrapTokenID, Scope: apiScopeAdmin}}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var tail tracker.NativeIssue
			var issues []tracker.NativeIssue
			for index := range test.size {
				request := tracker.CreateIssue{Title: fmt.Sprintf("candidate-%d", index), State: "Todo"}
				if index == test.size-1 {
					request.Priority = new(1)
					request.Labels = []string{"hotfix"}
				}
				tail, err = createNativeIssueTx(t.Context(), tx, scope, request, f.service.config.now())
				if err != nil {
					t.Fatal(err)
				}
				issues = append(issues, tail)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if test.unpublished {
				approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
				worker := f.worker(t, "source-worker")
				lease := claimNativeAttempt(t, f, worker, "source-machine", "source-session", tail.WorkItemID)
				event := nativeStartedEvent(lease)
				path := f.base + "/work-items/" + string(tail.WorkItemID) + "/events"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.checkpointed", "checkpoint", 2
				event.Data.Handoff = nativeTestCheckpoint()
				event.Data.Handoff.WorktreeState = "unpushed"
				event.Data.Handoff.HeadSHA = strings.Repeat("b", 40)
				event.Data.Handoff.WorkspaceDigest = strings.Repeat("d", 64)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
				event.Type, event.IdempotencyKey, event.Data.Sequence = "run.finished", "finish", 3
				event.Data.Handoff, event.Data.Outcome = nil, "succeeded"
				event.Data.CompletionBody = "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, event), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE native_attempts SET data_json = json_remove(data_json, '$.disposition') WHERE work_item_id = ?", tail.WorkItemID); err != nil {
					t.Fatal(err)
				}
				for _, issue := range issues[:len(issues)-1] {
					leaseID := newNativeID("lease")
					result, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases
(lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at)
SELECT ?, i.id, l.machine_id, ?, l.expires_at, l.acquired_at, l.renewed_at, l.released_at, l.created_at, l.updated_at
FROM leases l JOIN issues i ON i.native_id = ? WHERE l.lease_id = ?`, leaseID, newNativeID("session"), issue.WorkItemID, lease.ID)
					if err != nil {
						t.Fatal(err)
					}
					fence, err := result.LastInsertId()
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts
(id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, checkpoint_json, started_at, updated_at, work_item_revision, dispatch_generation)
SELECT ?, a.organization_id, a.project_id, i.native_id, ?, ?, ?, a.sequence, a.status, a.data_json, a.checkpoint_json, a.started_at, a.updated_at, i.revision, i.dispatch_generation
FROM native_attempts a JOIN issues i ON i.native_id = ? WHERE a.work_item_id = ?`, newNativeID("attempt"), leaseID, fence, newNativeID("run"), issue.WorkItemID, tail.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE leases SET released_at = NULL, expires_at = ? WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)`, formatHubTime(f.service.config.now().Add(time.Hour)), issues[0].WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			probe := measureDispatchQueries(t, f.service)
			tx, err = f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var tailID tracker.WorkItemID
			if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", tail.WorkItemID).Scan(&tailID); err != nil {
				t.Fatal(err)
			}
			query := claimCandidateQuery{NativeScope: &scope, Scope: string(scope.project), Limit: 9, AvailableAt: f.service.config.now(), DispatchPriorityByState: []string{"Todo"}, DispatchPriorityByLabel: []string{"hotfix"}}
			began := time.Now()
			ids, err := claimCandidateIDs(t.Context(), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(began)
			wantRows := int64(9)
			if len(ids) != 9 || ids[0] != tailID || probe.rows.Load() != wantRows {
				t.Fatalf("ids=%v tail=%d returned_rows=%d", ids, tailID, probe.rows.Load())
			}
			if bindBytes == 0 {
				bindBytes = probe.maxArgumentBytes.Load()
			}
			if probe.maxArgumentBytes.Load() != bindBytes {
				t.Fatalf("bind payload grew with candidate cohort: got=%d want=%d", probe.maxArgumentBytes.Load(), bindBytes)
			}
			if test.unpublished {
				if probe.completionBodies.Load() != 9 {
					t.Fatalf("first page completion_bodies=%d bind_bytes=%d", probe.completionBodies.Load(), probe.maxArgumentBytes.Load())
				}
				seen := slices.Clone(ids)
				pages := 1
				for len(ids) == query.Limit {
					query.After = ids[len(ids)-1]
					before := probe.completionBodies.Load()
					ids, err = claimCandidateIDs(t.Context(), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					if work := probe.completionBodies.Load() - before; work != int64(len(ids)+1) {
						t.Fatalf("page=%d bodies=%d want=%d including cursor", pages, work, len(ids)+1)
					}
					for _, id := range ids {
						if slices.Contains(seen, id) {
							t.Fatalf("page repeated candidate %d", id)
						}
					}
					seen = append(seen, ids...)
					pages++
				}
				var leasedID tracker.WorkItemID
				if err := tx.QueryRowContext(t.Context(), "SELECT id FROM issues WHERE native_id = ?", issues[0].WorkItemID).Scan(&leasedID); err != nil {
					t.Fatal(err)
				}
				if len(seen) != test.size-1 || slices.Contains(seen, leasedID) || probe.maxArgumentBytes.Load() != bindBytes {
					t.Fatalf("paged=%d leased=%d bind_bytes=%d", len(seen), leasedID, probe.maxArgumentBytes.Load())
				}
				t.Logf("pages=%d candidates=%d legacy_bodies=%d max_bind_bytes=%d", pages, len(seen), probe.completionBodies.Load(), probe.maxArgumentBytes.Load())
				for index, issue := range issues[:200] {
					body := "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: null\n```"
					if index%2 == 0 {
						body = strings.Replace(body, "schema: 1", "schema: 2", 1)
					}
					if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = json_set(data_json, '$.completion_body', ?) WHERE work_item_id = ?`, body, issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
					if _, err := tx.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = 0 WHERE issue_id = (SELECT id FROM issues WHERE native_id = ?)", issue.WorkItemID); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE queue_entries SET priority_override = CASE WHEN issue_id = ? THEN 1 ELSE COALESCE(priority_override, 2) END", tailID); err != nil {
					t.Fatal(err)
				}
				query.After, query.Limit = 0, 1
				before := probe.completionBodies.Load()
				ids, err = claimCandidateIDs(t.Context(), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil)
				if err != nil || !slices.Equal(ids, []tracker.WorkItemID{tailID}) || probe.completionBodies.Load()-before != 200 {
					t.Fatalf("after nonqualifying reports ids=%v bodies=%d err=%v", ids, probe.completionBodies.Load()-before, err)
				}
				t.Logf("claim-sized selection examined %d reports before ranked qualifying candidate", probe.completionBodies.Load()-before)
			}
			captured := probe.query.Load()
			plan, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+captured.statement, captured.args...)
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Close()
			for plan.Next() {
				var id, parent, unused int
				var detail string
				if err := plan.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(detail, "SEARCH i ") || strings.Contains(detail, "SEARCH candidate ") || strings.Contains(detail, "ORDER BY") {
					t.Log(detail)
				}
			}
			if err := plan.Err(); err != nil {
				t.Fatal(err)
			}
			if err := plan.Close(); err != nil {
				t.Fatal(err)
			}
			query.After = ids[0]
			if test.unpublished {
				if _, err := tx.ExecContext(t.Context(), `UPDATE native_attempts SET data_json = json_set(data_json, '$.completion_body', ?) WHERE work_item_id = ?`, "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: null\n```", tail.WorkItemID); err != nil {
					t.Fatal(err)
				}
				before := probe.completionBodies.Load()
				if _, err := claimCandidateIDs(t.Context(), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil); !isProviderWait(err) || probe.completionBodies.Load()-before != 1 {
					t.Fatalf("answered cursor must refresh without reading next page: bodies=%d err=%v", probe.completionBodies.Load()-before, err)
				}
			}
			if _, err := tx.ExecContext(t.Context(), "UPDATE issues SET archived = 1 WHERE id = ?", query.After); err != nil {
				t.Fatal(err)
			}
			if _, err := claimCandidateIDs(t.Context(), tx, query, nil, nil, []string{"todo"}, nil, nil, nil, nil, nil); err == nil {
				t.Fatal("removed cursor did not request a fresh preview")
			}
			t.Logf("queue=%d page_rows=9 select_duration=%s", test.size, elapsed)
		})
	}
}
