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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
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

type tracedStatement struct {
	query string
	args  []any
}

type reportsBudgetTrace struct{}

func (reportsBudgetTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if timing := requestTimingFrom(ctx); timing != nil && timing.Trace != nil {
		args := make([]driver.NamedValue, len(data.Args))
		for i, value := range data.Args {
			args[i] = driver.NamedValue{Ordinal: i + 1, Value: value}
		}
		timing.Trace(data.SQL, args)
	}
	return ctx
}
func (reportsBudgetTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (reportsBudgetTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}
func (trace reportsBudgetTrace) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	trace.TraceQueryStart(ctx, conn, pgx.TraceQueryStartData{SQL: data.SQL, Args: data.Args})
}
func (reportsBudgetTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func installReportsBudgetTrace(t *testing.T, s *Service) {
	t.Helper()
	admin := hostedPostgresAdmin(t, s)
	if err := s.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if s.outbox != nil {
		s.outbox.stop()
	}
	s.workerCancel()
	<-s.workerDone
	<-s.secretRewrapDone

	config, err := s.config.Postgres.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	serving, background := s.database.postgres.WorkloadCapacities()
	budgets := postgres.PoolConfig{MaxConnections: int(s.database.postgres.Stats().MaxConns()), ServingConnections: serving, BackgroundConnections: background}
	config.MaxConns = int32(budgets.MaxConnections)
	config.ConnConfig.Tracer = reportsBudgetTrace{}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := postgres.NewRepository(t.Context(), pool, budgets, s.config.Postgres.LeaseClock)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	projectRecordDatabases.Store(repository, admin)
	oldTenant, oldAccess, oldBilling := s.config.TenantRepository, s.config.AccessRepository, s.config.BillingRepository
	oldDatabase, oldMetrics, oldDatabaseBilling := s.database.postgres, s.requestMetrics.Repository, s.database.billingRepository
	s.config.TenantRepository, s.config.AccessRepository, s.config.BillingRepository = repository, repository, repository
	s.database.postgres, s.requestMetrics.Repository, s.database.billingRepository = repository, repository, repository
	t.Cleanup(func() {
		s.config.TenantRepository, s.config.AccessRepository, s.config.BillingRepository = oldTenant, oldAccess, oldBilling
		s.database.postgres, s.requestMetrics.Repository, s.database.billingRepository = oldDatabase, oldMetrics, oldDatabaseBilling
		if s.requestMetrics != nil {
			s.requestMetrics.RecordUsage(s.database.metricsBillingOwner(), requestmetrics.Key{}, requestmetrics.Usage{})
		}
		projectRecordDatabases.Delete(repository)
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
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
