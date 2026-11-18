package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReportsLongRangeReadBudget(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	f := newUsageHostedFixture(t, func(cfg *Config) { cfg.Clock = func() time.Time { return now } })
	installReportsBudgetTrace(t, f.service)
	scope := hubscope.NativeScope{Organization: "org_browser_preview", Project: tracker.ProjectID(f.project)}
	seedProductionProject(t, f.service, scope, productionProjectShape{issues: 990, attempts: 990, usage: 660, leases: 1682, comments: 7000, days: 180})
	content := eventstore.NewContent(f.service.runDetailStore())
	admin := hostedPostgresAdmin(t, f.service)
	rows, err := admin.QueryContext(t.Context(), `SELECT id FROM change_requests WHERE organization_id=$1 AND project_id=$2 AND id LIKE 'change_wi_shaped_%' ORDER BY id`, scope.Organization, scope.Project)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	for _, id := range ids {
		var change eventstore.Change
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error { var err error; change, err = r.Change(t.Context(), id); return err }); err != nil {
			t.Fatal(err)
		}
		for number := 1; number <= 6; number++ {
			kind := "review"
			var value any = tracker.ChangeReview{ID: fmt.Sprintf("%s_%d", id, number), VersionID: *change.LandedVersionID, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, Decision: "changes_requested", CreatedAt: *change.LandedAt, Body: strings.Repeat("x", 4096)}
			if number%2 == 0 {
				kind = "discussion"
				value = tracker.ChangeDiscussion{ID: fmt.Sprintf("%s_%d", id, number), VersionID: *change.LandedVersionID, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, CreatedAt: *change.LandedAt, Body: strings.Repeat("x", 4096), Escape: &tracker.QualityEscape{OccurrenceID: fmt.Sprintf("%s_%d", id, number), Kind: "reopened", Cause: "missing_criterion", ObservedAt: *change.LandedAt, EvidenceReference: "record", EvidenceQuote: "Recorded failure", BasisQuote: "Frozen contract"}}
			}
			evidence, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: scope.Organization, ChangeID: id, Kind: kind}, historyTestJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), evidence); return err }); err != nil {
				t.Fatal(err)
			}
		}
	}

	pgstoretest.SeedNeighborReportRows(t, admin, string(scope.Organization), 50)
	freezeReportPlannerInputs(t, admin)
	statementCounts := map[string]int{}
	for _, test := range []struct {
		name      string
		rangeName string
		hour      int
		days      int
	}{
		{"30d/midnight", "30d", 0, 30},
		{"30d/daytime", "30d", 10, 29},
		{"90d/midnight", "90d", 0, 90},
		{"90d/daytime", "90d", 10, 89},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.service.config.Clock = func() time.Time { return now.Add(time.Duration(test.hour) * time.Hour) }
			var mu sync.Mutex
			var statements []tracedStatement
			timing := &requestTiming{Trace: func(query string, args []driver.NamedValue) {
				if args == nil {
					return
				}
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
			ctx, cancel := context.WithTimeout(withRequestTiming(t.Context(), timing), hubapi.ReadTimeout)
			defer cancel()
			request := httptest.NewRequest(http.MethodGet, browserHostedOrganizationBase+"/reports?project="+f.project+"&range="+test.rangeName, nil).WithContext(ctx)
			request.AddCookie(f.cookies["owner"])
			response := httptest.NewRecorder()
			started := time.Now()
			f.service.Handler().ServeHTTP(response, request)
			elapsed := time.Since(started)
			if response.Code != http.StatusOK || elapsed >= hubapi.ReadTimeout {
				t.Fatalf("report took %s: HTTP %d: %s", elapsed, response.Code, response.Body.String())
			}
			var report hostedReportsReport
			if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			now := f.service.config.Clock().UTC()
			wantFrom := now.Truncate(24*time.Hour).AddDate(0, 0, -test.days)
			if !report.Analytics.Window.From.Equal(wantFrom) || !report.Analytics.Window.To.Equal(now) || report.Completion.Done < test.days {
				t.Fatalf("window=%+v shipped=%d, want %s through %s and at least %d shipped", report.Analytics.Window, report.Completion.Done, wantFrom, now, test.days)
			}
			t.Logf("%s report: %s, %d shipped", test.rangeName, elapsed, report.Completion.Done)
			mu.Lock()
			captured := append([]tracedStatement(nil), statements...)
			mu.Unlock()
			statementCounts[test.name] = len(captured)
			reportsAssertPostgresPlans(t, admin, scope.Organization, captured, "reports", "issues", "collaboration_events", "native_attempts", "change_requests", "report_escape_facts")

		})
	}
	if statementCounts["30d/midnight"] == 0 || statementCounts["90d/midnight"] != statementCounts["30d/midnight"] || statementCounts["90d/daytime"] != statementCounts["30d/daytime"] {
		t.Errorf("report statements grow with the window: %v", statementCounts)
	}
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
func freezeReportPlannerInputs(t *testing.T, admin *sql.DB) {
	t.Helper()
	relations, autovacuum := strings.Join(reportsBudgetHotRelations, ","), " SET (autovacuum_enabled=false, toast.autovacuum_enabled=false)"
	for _, statement := range []string{"ALTER TABLE " + strings.Join(reportsBudgetHotRelations, autovacuum+";ALTER TABLE ") + autovacuum, "VACUUM (FREEZE) " + relations, "VACUUM (ANALYZE) " + relations, `DO $$DECLARE lagging text; BEGIN SELECT string_agg(relname||' '||relallvisible||'/'||relpages,',') INTO lagging FROM pg_class WHERE oid=ANY(string_to_array('` + relations + `',',')::regclass[]) AND relallvisible<relpages; IF lagging IS NOT NULL THEN RAISE EXCEPTION 'planner inputs not all-visible after VACUUM: %', lagging; END IF; END$$`} {
		if _, err := admin.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
}

func reportsAssertPostgresPlans(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, statements []tracedStatement, request string, required ...string) {
	t.Helper()
	plans := make([]pgstoretest.ReportStatement, len(statements))
	for i, statement := range statements {
		plans[i] = pgstoretest.ReportStatement{Query: statement.query, Args: statement.args}
	}
	report, err := pgstoretest.CheckReportRequest(t.Context(), admin, string(organization), plans, reportsBudgetHotRelations, pgstoretest.ReportRequestBudgets[request])
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range report.Problems {
		t.Errorf("actual %s read violates report source bounds: %s", request, problem)
	}
	for _, relation := range required {
		if !report.Relations[relation] {
			t.Errorf("actual report did not exercise %s facts", relation)
		}
	}
}

func reportsExplainPostgresPlan(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, statement tracedStatement, planner ...string) (reportsBudgetPlan, []byte) {
	t.Helper()
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
	for _, setting := range planner {
		if _, err := tx.ExecContext(t.Context(), "SET LOCAL "+setting); err != nil {
			t.Fatal(err)
		}
	}
	var raw []byte
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN(ANALYZE,BUFFERS,FORMAT JSON) "+statement.query, statement.args...).Scan(&raw); err != nil {
		t.Fatalf("explain actual report: %v\nstatement: %s", err, statement.query)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var plans []struct{ Plan reportsBudgetPlan }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("report plan=%s error=%v", raw, err)
	}
	return plans[0].Plan, raw
}

var reportsBudgetHotRelations = []string{"issues", "collaboration_events", "native_attempts", "change_requests", "change_evidence", "attempt_usage", "report_usage_current", "report_escape_facts"}
var reportsBudgetPointKey = regexp.MustCompile(`\b(?:id|native_id|work_item_id|issue_id|attempt_id|change_id|source_key|sequence)\b\s*(?:=|<|>)`)
var reportsBudgetLimit = regexp.MustCompile(`(?i)\bLIMIT\s+(?:\$([0-9]+)|([0-9]+))`)

type reportsBudgetPlan struct {
	Node         string              `json:"Node Type"`
	Index        string              `json:"Index Name"`
	Relation     string              `json:"Relation Name"`
	Condition    string              `json:"Index Cond"`
	Rows         float64             `json:"Actual Rows"`
	Loops        float64             `json:"Actual Loops"`
	Removed      float64             `json:"Rows Removed by Filter"`
	IndexRemoved float64             `json:"Rows Removed by Index Recheck"`
	SortSpace    string              `json:"Sort Space Type"`
	Plans        []reportsBudgetPlan `json:"Plans"`
}

func reportsBudgetPlanViolations(statement tracedStatement, plan reportsBudgetPlan) []string {
	budget := 0
	for _, match := range reportsBudgetLimit.FindAllStringSubmatch(statement.query, -1) {
		value := match[2]
		if match[1] != "" {
			position, err := strconv.Atoi(match[1])
			if err != nil || position < 1 || position > len(statement.args) {
				continue
			}
			value = fmt.Sprint(statement.args[position-1])
		}
		limit, err := strconv.Atoi(value)
		if err == nil && limit > budget {
			budget = limit
		}
	}
	if budget == 0 {
		budget = 1001
	}
	if budget > 100001 {
		return []string{fmt.Sprintf("source request exceeds existing 100001-row owner budget: %d", budget)}
	}
	violations := []string{}
	var inspect func(reportsBudgetPlan)
	inspect = func(node reportsBudgetPlan) {
		if slices.Contains(reportsBudgetHotRelations, node.Relation) && node.Loops > 0 {
			conditions, indexed := reportsBudgetConditions(node, node.Relation)
			tenantKey := strings.Contains(conditions, "organization_id =")
			pointKey := reportsBudgetPointKey.MatchString(conditions)
			windowKey := false
			for _, column := range []string{"recorded_at", "landed_at", "period", "started_at", "created_at", "native_updated_at", "updated_at", "observed_at"} {
				windowKey = windowKey || (strings.Contains(conditions, column+" >=") && strings.Contains(conditions, column+" <"))
			}
			if strings.Contains(node.Node, "Seq Scan") || !indexed || !tenantKey || (!pointKey && !windowKey) {
				violations = append(violations, fmt.Sprintf("%s uses %s without an indexed tenant and item/cursor or paired window key: %s", node.Relation, node.Node, conditions))
			}
			visits := (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
			if visits > float64(budget) {
				violations = append(violations, fmt.Sprintf("%s visits %.0f rows above the request source budget %d", node.Relation, visits, budget))
			}
		}
		if node.SortSpace == "Disk" {
			violations = append(violations, "report source ordering spilled to disk")
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plan)
	return violations
}

func reportsBudgetConditions(node reportsBudgetPlan, relation string) (string, bool) {
	if node.Relation != "" && node.Relation != relation {
		return "", false
	}
	conditions, indexed := node.Condition, node.Index != ""
	for _, child := range node.Plans {
		value, childIndexed := reportsBudgetConditions(child, relation)
		conditions += " " + value
		indexed = indexed || childIndexed
	}
	return conditions, indexed
}

func reportsBudgetReadStatement(query string) bool {
	query = strings.TrimSpace(query)
	for strings.HasPrefix(query, "--") {
		_, rest, found := strings.Cut(query, "\n")
		if !found {
			return false
		}
		query = strings.TrimSpace(rest)
	}
	query = strings.ToUpper(query)
	return strings.HasPrefix(query, "SELECT") || strings.HasPrefix(query, "WITH")
}
