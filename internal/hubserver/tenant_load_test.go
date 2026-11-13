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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestTenantServesRunnersWhileReadsAreSlow(t *testing.T) {
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
	f := newDefaultNativeFixture(t, Config{RunDetails: &memoryRunDetails{}, now: func() time.Time { return time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC) }})
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
	}
	t.Run("history scan is rejected", func(t *testing.T) {
		statement := tracedStatement{"SELECT sum(payload_content_bytes) FROM collaboration_events WHERE organization_id=$1", []any{f.project.OrganizationID}}
		plan, raw := reportsExplainPostgresPlan(t, admin, f.project.OrganizationID, statement)
		if violations := reportsBudgetPlanViolations(statement, plan); len(violations) == 0 {
			t.Fatalf("plan check accepted a full history scan: %s", raw)
		}
	})
	counts := map[string]int{}
	previous := 0
	for _, history := range []int{20000, 200000} {
		t.Run(strconv.Itoa(history), func(t *testing.T) {
			now := f.service.config.now().UTC()
			content := eventstore.NewContent(f.service.runDetailStore())
			for index, item := range items {
				var sequence int64
				if err := admin.QueryRowContext(t.Context(), "SELECT event_sequence FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", f.project.OrganizationID, f.project.ID, item.WorkItemID).Scan(&sequence); err != nil {
					t.Fatal(err)
				}
				for first := previous + 1; first <= history; first += 1000 {
					ids, keys, digests := []string{}, []string{}, []string{}
					sequences, sizes, contents := []int64{}, []int64{}, []int64{}
					for i := first; i <= min(first+999, history); i++ {
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
				if _, err := admin.ExecContext(t.Context(), "UPDATE issues SET event_sequence=$4 WHERE organization_id=$1 AND project_id=$2 AND native_id=$3", f.project.OrganizationID, f.project.ID, item.WorkItemID, sequence+int64(history-previous)); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND id LIKE $4", f.project.OrganizationID, f.project.ID, item.WorkItemID, fmt.Sprintf("evt_history_%d_%%", index)).Scan(&count); err != nil || count != history {
					t.Fatalf("historical population=%d want=%d error=%v", count, history, err)
				}
			}
			for index, item := range items {
				lease := leases[index]
				for _, operation := range []struct {
					name, path, token string
					payload           any
				}{
					{"renew", "/leases/" + string(lease.ID) + "/renew", workers[index], tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 90}},
					{"validate", "/leases/" + string(lease.ID) + "/validate", workers[index], tracker.NativeLeaseMutation{FencingToken: lease.FencingToken}},
					{"comment", "/work-items/" + string(item.WorkItemID) + "/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("history-%d-%d", history, index)}, Body: "progress"}},
				} {
					var mu sync.Mutex
					var statements []tracedStatement
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
						statements = append(statements, tracedStatement{query, values})
						mu.Unlock()
					}}
					request := newHubAPIRequest(t, http.MethodPost, f.base+operation.path, operation.token, operation.payload).WithContext(withRequestTiming(t.Context(), timing))
					response := httptest.NewRecorder()
					f.service.Handler().ServeHTTP(response, request)
					requireNativeStatus(t, response, http.StatusOK)
					f.service.tokenUseWork.Wait()
					mu.Lock()
					traced := append([]tracedStatement(nil), statements...)
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
						assertTenantWritePlan(t, admin, f.project.OrganizationID, statement)
					}
					t.Logf("history=%d %s queries=%d", history, key, len(traced))
				}
			}
		})
		previous = history
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

func assertTenantWritePlan(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, statement tracedStatement) {
	t.Helper()
	query := strings.TrimSpace(statement.query)
	for strings.HasPrefix(query, "--") {
		_, query, _ = strings.Cut(query, "\n")
		query = strings.TrimSpace(query)
	}
	head := strings.ToUpper(query)
	if !strings.HasPrefix(head, "SELECT") && !strings.HasPrefix(head, "WITH") && !strings.HasPrefix(head, "INSERT") && !strings.HasPrefix(head, "UPDATE") && !strings.HasPrefix(head, "DELETE") {
		return
	}
	tx, err := admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
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
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN(FORMAT JSON) "+statement.query, statement.args...).Scan(&raw); err != nil {
		t.Fatalf("write-path plan: %v statement=%s", err, statement.query)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var plans []struct{ Plan reportsBudgetPlan }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("write plan=%s error=%v", raw, err)
	}
	var inspect func(reportsBudgetPlan)
	inspect = func(node reportsBudgetPlan) {
		if strings.Contains(node.Node, "Scan") && (node.Relation == "collaboration_events" || node.Relation == "native_attempts" || node.Relation == "attempt_usage" || node.Relation == "leases") {
			conditions, indexed := reportsBudgetConditions(node, node.Relation)
			if !indexed || !strings.Contains(conditions, "organization_id =") || !reportsBudgetPointKey.MatchString(conditions) {
				t.Errorf("write path reads unkeyed history: relation=%s conditions=%s statement=%s plan=%s", node.Relation, conditions, statement.query, raw)
			}
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plans[0].Plan)
}
