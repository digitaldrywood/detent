package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTenantServesRunnersWhileReadsAreSlow(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "tenant-load")
	items := []tracker.NativeIssue{f.create(t, "first"), f.create(t, "second")}
	worker := f.worker(t, "worker")
	f.service.tokenUseWork.Wait()
	ctx := testenv.WaitContext(t, t.Context())
	repository := f.service.database.postgres
	serving, background := repository.WorkloadCapacities()
	if background != 1 {
		t.Fatalf("background capacity=%d want=1", background)
	}
	writerReady, releaseWriter := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	rollback := errors.New("release held project write")
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			close(releaseWriter)
			if err := <-writerDone; !errors.Is(err, rollback) {
				t.Errorf("held project write: %v", err)
			}
		})
	}
	defer release()
	go func() {
		writerDone <- repository.WriteTenant(ctx, f.project.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(ctx, f.project.ID)
			if err != nil {
				return err
			}
			if _, err := w.UpdateProjectRecord(ctx, record); err != nil {
				return err
			}
			close(writerReady)
			select {
			case <-releaseWriter:
			case <-ctx.Done():
			}
			return rollback
		})
	}()
	select {
	case <-writerReady:
	case err := <-writerDone:
		writerDone <- err
		t.Fatalf("project write exited before readiness: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	releaseReaders := holdTenantLoadReads(t, repository, f.project.OrganizationID, corestore.Serving, serving)
	defer releaseReaders()
	if got := repository.Stats().AcquiredConns(); int(got) != serving+background {
		t.Fatalf("occupied PostgreSQL connections=%d want=%d", got, serving+background)
	}
	for _, token := range []string{f.token, worker} {
		credential, status, err := f.service.authenticateAPIToken(ctx, token, "", "")
		if err != nil || status != http.StatusOK || credential.ID == "" {
			t.Fatalf("authentication with occupied reader and writer pools: status=%d credential=%q error=%v", status, credential.ID, err)
		}
	}
	if credential, status, err := f.service.authenticateAPIToken(ctx, f.token+"-invalid", "", ""); status != http.StatusUnauthorized || err == nil || credential.ID != "" {
		t.Fatalf("invalid credential with occupied workloads: status=%d credential=%q error=%v", status, credential.ID, err)
	}
	if got := repository.Stats().AcquiredConns(); int(got) != serving+background {
		t.Fatalf("authentication released held workloads: acquired=%d want=%d", got, serving+background)
	}
	releaseReaders()
	for _, item := range items {
		for _, suffix := range []string{"comments", "runtime"} {
			request := newHubAPIRequest(t, http.MethodGet, f.base+"/work-items/"+string(item.WorkItemID)+"/"+suffix, f.token, nil).WithContext(ctx)
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, http.StatusOK)
		}
	}
}

func TestTenantRunnerWritesStayFlatWithHistory(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{RunDetails: &memoryRunDetails{}, Clock: func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	installReportsBudgetTrace(t, f.service)
	admin := hostedPostgresAdmin(t, f.service)
	f.service.config.Hosted = &hostedplans.HostedConfig{}
	f.service.database.hostedOrganization = f.project.OrganizationID
	hostedTestPlans(t, f.service, map[string]int64{"api_mutations": 1 << 40, "ingested_events": 1 << 40, "collaboration_bytes": 1 << 50, "history_records": 1 << 40,
		"concurrent_work": 10000, "connected_runners": 10000, "registered_runners": 10000, "unarchived_issues": 1 << 30})
	f.service.config.Hosted = nil
	items := []tracker.NativeIssue{f.create(t, "first"), f.create(t, "second")}
	var leases []tracker.NativeLease
	var workers []string
	for index, item := range items {
		worker := f.worker(t, fmt.Sprintf("worker-%d", index))
		workers = append(workers, worker)
		leases = append(leases, claimNativeAttempt(t, f, worker, fmt.Sprintf("machine-%d", index), fmt.Sprintf("session-%d", index), item.WorkItemID))
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", worker, nativeStartedEvent(leases[index])), http.StatusOK)
	}
	t.Run("history scan is rejected", func(t *testing.T) {
		statement := tracedStatement{"SELECT sum(payload_content_bytes) FROM collaboration_events WHERE organization_id=$1", []any{f.project.OrganizationID}}
		plan, raw := reportsExplainPostgresPlan(t, admin, f.project.OrganizationID, statement)
		if violations := reportsBudgetPlanViolations(statement, plan); len(violations) == 0 {
			t.Fatalf("plan check accepted a full history scan: %s", raw)
		}
	})
	foreign := newNativeFixture(t, f.service, "org_first_second", "Foreign project")
	foreignItem := foreign.create(t, "foreign")
	foreignWorker := foreign.worker(t, "foreign-worker")
	foreignLease := claimNativeAttempt(t, foreign, foreignWorker, "machine-foreign", "session-foreign", foreignItem.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, foreign.service, http.MethodPost, foreign.base+"/work-items/"+string(foreignItem.WorkItemID)+"/events", foreignWorker, nativeStartedEvent(foreignLease)), http.StatusOK)
	for _, relation := range tenantWriteHistoryRelations {
		if _, err := admin.ExecContext(t.Context(), "ALTER TABLE "+relation+" SET (autovacuum_enabled=false, toast.autovacuum_enabled=false)"); err != nil {
			t.Fatal(err)
		}
	}
	counts := map[string]int{}
	previous, retained, foreignRetained := 0, 0, 0
	for _, step := range []struct {
		name                       string
		history, retained, foreign int
	}{{"tiny", 0, 0, 0}, {"crossover", 20000, 48, 48}, {"large", 200000, 3000, 6000}} {
		t.Run(step.name, func(t *testing.T) {
			now := f.service.config.Clock().UTC()
			content := eventstore.NewContent(f.service.runDetailStore())
			for index, item := range items {
				var sequence int64
				if err := admin.QueryRowContext(t.Context(), "SELECT event_sequence FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", f.project.OrganizationID, f.project.ID, item.WorkItemID).Scan(&sequence); err != nil {
					t.Fatal(err)
				}
				for first := previous + 1; first <= step.history; first += 1000 {
					ids, keys, digests := []string{}, []string{}, []string{}
					sequences, sizes, contents := []int64{}, []int64{}, []int64{}
					for i := first; i <= min(first+999, step.history); i++ {
						id := fmt.Sprintf("evt_history_%d_%d", index, i)
						event, err := content.PrepareEvent(t.Context(), f.project.OrganizationID, historyTestJSON(t, tracker.CollaborationEvent{ID: id, OrganizationID: f.project.OrganizationID, ProjectID: f.project.ID, AggregateID: item.WorkItemID, AggregateSequence: sequence + int64(i-previous), Type: "comment.created", SchemaVersion: 1, Actor: tracker.Actor{Kind: "system", PrincipalID: "example"}, RecordedAt: now, Data: tracker.CollaborationData{Reason: strings.Repeat("h", 200)}}))
						if err != nil {
							t.Fatal(err)
						}
						ids = append(ids, event.ID)
						keys = append(keys, event.PayloadObjectKey)
						digests = append(digests, event.PayloadSHA256)
						sequences = append(sequences, event.Sequence)
						sizes = append(sizes, event.PayloadBytes)
						contents = append(contents, event.PayloadContentBytes)
					}
					if _, err := admin.ExecContext(t.Context(), `INSERT INTO collaboration_events(organization_id,project_id,work_item_id,id,sequence,type,schema_version,actor_kind,actor_principal_id,recorded_at,payload_object_key,payload_sha256,payload_bytes,payload_content_bytes,actor_envelope_known)
SELECT $1,$2,$3,e.id,e.sequence,'comment.created',1,'system','example',$4,e.key,e.digest,e.bytes,e.content_bytes,true
FROM unnest($5::text[],$6::bigint[],$7::text[],$8::text[],$9::bigint[],$10::bigint[])e(id,sequence,key,digest,bytes,content_bytes)`, f.project.OrganizationID, f.project.ID, item.WorkItemID, now, ids, sequences, keys, digests, sizes, contents); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := admin.ExecContext(t.Context(), "UPDATE issues SET event_sequence=$4 WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", f.project.OrganizationID, f.project.ID, item.WorkItemID, sequence+int64(step.history-previous)); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND id LIKE $4", f.project.OrganizationID, f.project.ID, item.WorkItemID, fmt.Sprintf("evt_history_%d_%%", index)).Scan(&count); err != nil || count != step.history {
					t.Fatalf("historical population=%d want=%d error=%v", count, step.history, err)
				}
				seedTenantLeaseHistory(t, admin, f.project.OrganizationID, leases[index].ID, fmt.Sprintf("lease_history_%d_", index), retained+1, step.retained)
			}
			seedTenantLeaseHistory(t, admin, foreign.project.OrganizationID, foreignLease.ID, "lease_foreign_", foreignRetained+1, step.foreign)
			var statements []tracedStatement
			for index, item := range items {
				lease := leases[index]
				for _, operation := range []struct {
					name, path, token string
					payload           any
				}{
					{"renew", "/leases/" + string(lease.ID) + "/renew", workers[index], tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}},
					{"validate", "/leases/" + string(lease.ID) + "/validate", workers[index], tracker.NativeLeaseMutation{FencingToken: lease.FencingToken}},
					{"comment", "/work-items/" + string(item.WorkItemID) + "/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("history-%s-%d", step.name, index)}, Body: "progress"}},
				} {
					var mu sync.Mutex
					var traced []tracedStatement
					timing := &requestTiming{Trace: func(query string, args []driver.NamedValue) {
						values := make([]any, 0, len(args))
						for _, arg := range args {
							if arg.Name != "" {
								values = append(values, sql.Named(arg.Name, arg.Value))
							} else {
								values = append(values, arg.Value)
							}
						}
						mu.Lock()
						traced = append(traced, tracedStatement{query, values})
						mu.Unlock()
					}}
					request := newHubAPIRequest(t, http.MethodPost, f.base+operation.path, operation.token, operation.payload).WithContext(withRequestTiming(t.Context(), timing))
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					requireNativeStatus(t, response, http.StatusOK)
					f.service.tokenUseWork.Wait()
					mu.Lock()
					traced = append([]tracedStatement(nil), traced...)
					mu.Unlock()
					key := fmt.Sprintf("%s/%d", operation.name, index)
					if len(traced) == 0 {
						t.Fatal("no request statements were traced")
					}
					if previous == 0 {
						counts[key] = len(traced)
					} else if len(traced) > counts[key] {
						t.Errorf("%s queries grew with history: %d -> %d", key, counts[key], len(traced))
					}
					for _, statement := range traced {
						statements = append(statements, statement)
						for i, arg := range statement.args {
							if arg == string(lease.ID) {
								absent := append([]any(nil), statement.args...)
								absent[i] = "lease_absent"
								statements = append(statements, tracedStatement{statement.query, absent})
							}
						}
					}
					t.Logf("%s %s queries=%d", step.name, key, len(traced))
				}
			}
			for _, state := range []string{"statistics before this growth", "ANALYZE", "VACUUM (ANALYZE)"} {
				if state != "statistics before this growth" {
					if _, err := admin.ExecContext(t.Context(), state+" "+strings.Join(tenantWriteHistoryRelations, ",")); err != nil {
						t.Fatal(err)
					}
				}
				for _, statement := range statements {
					assertTenantWriteWork(t, admin, f.project.OrganizationID, statement, step.name+"/"+state)
				}
			}
			if step.name == "large" {
				for _, control := range []tracedStatement{
					{"SELECT sum(payload_content_bytes) FROM collaboration_events WHERE organization_id=$1", []any{f.project.OrganizationID}},
					{"SELECT count(*) FROM leases l JOIN native_attempts a ON a.organization_id=l.organization_id AND a.lease_id=l.lease_id WHERE l.organization_id=$1 AND l.issue_id=(SELECT issue_id FROM leases WHERE organization_id=$1 AND lease_id=$2)", []any{f.project.OrganizationID, leases[0].ID}},
				} {
					if violations, _, raw := tenantWriteWorkViolations(t, admin, f.project.OrganizationID, control); len(violations) == 0 {
						t.Fatalf("work budget accepted an unbounded history query: %s", raw)
					}
				}
			}
		})
		previous, retained, foreignRetained = step.history, step.retained, step.foreign
	}
}

var tenantWriteHistoryRelations = []string{"collaboration_events", "native_attempts", "attempt_usage", "leases"}

const tenantWriteRowVisits = 256

type tenantWritePlan struct {
	Node         string            `json:"Node Type"`
	Relation     string            `json:"Relation Name"`
	Index        string            `json:"Index Name"`
	Rows         float64           `json:"Actual Rows"`
	Estimate     float64           `json:"Plan Rows"`
	Loops        float64           `json:"Actual Loops"`
	Removed      float64           `json:"Rows Removed by Filter"`
	IndexRemoved float64           `json:"Rows Removed by Index Recheck"`
	Hit          float64           `json:"Shared Hit Blocks"`
	Read         float64           `json:"Shared Read Blocks"`
	Plans        []tenantWritePlan `json:"Plans"`
}

func seedTenantLeaseHistory(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, template tracker.LeaseID, prefix string, first, last int) {
	t.Helper()
	if first > last {
		return
	}
	overrides := map[string]map[string]string{
		"leases":          {"lease_id": "$3||n", "session_id": "$3||'session_'||n", "released_at": "t.acquired_at", "release_reason": "'completed'"},
		"native_attempts": {"id": "'att_'||h.lease_id", "lease_id": "h.lease_id", "fencing_token": "h.fencing_token", "status": "'succeeded'"},
	}
	from := map[string]string{
		"leases":          "FROM leases t CROSS JOIN generate_series($4::int,$5::int) n WHERE t.organization_id=$1 AND t.lease_id=$2",
		"native_attempts": "FROM native_attempts t JOIN leases h ON h.organization_id=t.organization_id AND h.lease_id=ANY(ARRAY(SELECT $3||n FROM generate_series($4::int,$5::int) n)) WHERE t.organization_id=$1 AND t.lease_id=$2",
	}
	for _, relation := range []string{"leases", "native_attempts"} {
		rows, err := admin.QueryContext(t.Context(), "SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND is_generated='NEVER' AND identity_generation IS DISTINCT FROM 'ALWAYS' ORDER BY ordinal_position", relation)
		if err != nil {
			t.Fatal(err)
		}
		var columns, values []string
		for rows.Next() {
			var column string
			if err := rows.Scan(&column); err != nil {
				t.Fatal(err)
			}
			value := "t." + column
			if override, ok := overrides[relation][column]; ok {
				value = override
			}
			columns, values = append(columns, column), append(values, value)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			t.Fatal(err)
		}
		result, err := admin.ExecContext(t.Context(), "INSERT INTO "+relation+"("+strings.Join(columns, ",")+") SELECT "+strings.Join(values, ",")+" "+from[relation], organization, template, prefix, first, last)
		if err != nil {
			t.Fatalf("seed %s history: %v", relation, err)
		}
		if inserted, err := result.RowsAffected(); err != nil || inserted != int64(last-first+1) {
			t.Fatalf("seed %s history inserted %d of %d: %v", relation, inserted, last-first+1, err)
		}
	}
}

func holdTenantLoadReads(t *testing.T, repository *postgres.Repository, organization tracker.OrganizationID, workload corestore.Workload, count int) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	release, done := make(chan struct{}), make(chan error, count)
	var once sync.Once
	held := 0
	finish := func() {
		once.Do(func() {
			close(release)
			for range held {
				if err := <-done; err != nil {
					t.Error(err)
				}
			}
			cancel()
		})
	}
	t.Cleanup(finish)
	for range count {
		ready := make(chan struct{})
		held++
		go func() {
			done <- repository.ReadTenant(ctx, organization, workload, func(r tenantstore.Reader) error {
				if _, err := r.AccessOrganization(ctx); err != nil {
					return err
				}
				close(ready)
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		}()
		select {
		case <-ready:
		case err := <-done:
			held--
			t.Fatalf("held PostgreSQL reader exited before readiness: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	return finish
}

func warmReaderPools(t *testing.T, repository *postgres.Repository, organization tracker.OrganizationID) {
	t.Helper()
	serving, background := repository.WorkloadCapacities()
	releaseServing := holdTenantLoadReads(t, repository, organization, corestore.Serving, serving)
	defer releaseServing()
	releaseBackground := holdTenantLoadReads(t, repository, organization, corestore.Background, background)
	defer releaseBackground()
	if err := repository.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	stats := repository.Stats()
	if stats.TotalConns() != stats.MaxConns() || int(stats.AcquiredConns()) != serving+background {
		t.Fatalf("PostgreSQL pool was not fully warmed: total=%d max=%d acquired=%d workloads=%d", stats.TotalConns(), stats.MaxConns(), stats.AcquiredConns(), serving+background)
	}
}

func assertTenantWriteWork(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, statement tracedStatement, state string) {
	t.Helper()
	violations, access, raw := tenantWriteWorkViolations(t, admin, organization, statement)
	if len(violations) > 0 {
		t.Errorf("%s: write path work exceeds %d history rows per node: %v\nstatement=%s\nplan=%s", state, tenantWriteRowVisits, violations, statement.query, raw)
	}
	if name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(statement.query), "-- name: "), " "); slices.Contains([]string{"GetLease", "LeaseMutationFacts", "MutateLeaseCommand"}, name) {
		t.Logf("%s %s args=%v leases: %s", state, name, statement.args, access)
	}
}

func tenantWriteWorkViolations(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, statement tracedStatement) ([]string, string, []byte) {
	t.Helper()
	query := strings.TrimSpace(statement.query)
	for strings.HasPrefix(query, "--") {
		_, query, _ = strings.Cut(query, "\n")
		query = strings.TrimSpace(query)
	}
	head := strings.ToUpper(query)
	if !strings.HasPrefix(head, "SELECT") && !strings.HasPrefix(head, "WITH") && !strings.HasPrefix(head, "UPDATE") && !strings.HasPrefix(head, "DELETE") {
		return nil, "", nil
	}
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", string(organization)); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if _, err := tx.ExecContext(t.Context(), "SAVEPOINT replay"); err != nil {
		t.Fatal(err)
	}
	estimated := false
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN(ANALYZE,BUFFERS,FORMAT JSON) "+statement.query, statement.args...).Scan(&raw); err != nil {
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || !strings.HasPrefix(failure.Code, "23") {
			t.Fatalf("write-path plan: %v statement=%s", err, statement.query)
		}
		if _, err := tx.ExecContext(t.Context(), "ROLLBACK TO SAVEPOINT replay"); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRowContext(t.Context(), "EXPLAIN(FORMAT JSON) "+statement.query, statement.args...).Scan(&raw); err != nil {
			t.Fatalf("write-path estimated plan: %v statement=%s", err, statement.query)
		}
		estimated = true
	}
	var plans []struct{ Plan tenantWritePlan }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("write plan=%s error=%v", raw, err)
	}
	var violations []string
	access := ""
	var inspect func(tenantWritePlan)
	inspect = func(node tenantWritePlan) {
		if node.Relation == "leases" || node.Relation == "native_attempts" {
			access += fmt.Sprintf("%s %s %s rows=%.0f removed=%.0f loops=%.0f buffers=%.0f; ", node.Relation, node.Node, node.Index, node.Rows, node.Removed+node.IndexRemoved, node.Loops, node.Hit+node.Read)
		}
		visits := (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
		if estimated {
			visits = node.Estimate
		}
		if visits > tenantWriteRowVisits && slices.Contains(tenantWriteHistoryRelations, node.Relation) {
			violations = append(violations, fmt.Sprintf("%s %s %s visits %.0f rows", node.Relation, node.Node, node.Index, visits))
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plans[0].Plan)
	return violations, access, raw
}
