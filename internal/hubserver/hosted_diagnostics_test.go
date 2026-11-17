package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/diagnosticstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type diagnosticsPostgresFixture struct {
	repository   *postgres.Repository
	admin        *sql.DB
	details      *memoryRunDetails
	hosted       hostedSecurityFixture
	owner        hostedSecurityUser
	organization tracker.OrganizationID
	queries      *diagnosticsSQLCapture
}

func diagnosticsPostgresFixtures(t *testing.T) []diagnosticsPostgresFixture {
	t.Helper()
	capture := &diagnosticsSQLCapture{statements: make(map[string]diagnosticsSQLStatement)}
	repository, admin := projectRecordPostgresWithAdmin(t, 4, func(config *pgxpool.Config) {
		capture.QueryTracer = config.ConnConfig.Tracer
		config.ConnConfig.Tracer = capture
	})
	fixtures := []diagnosticsPostgresFixture{}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		details := &memoryRunDetails{}
		hosted, owner := newChangePolicyPostgresTenantFixture(t, repository, organization, details)
		hosted.service.workerCancel()
		<-hosted.service.workerDone
		<-hosted.service.secretRewrapDone
		if err := hosted.service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		hosted.service.dashboard.stop()
		hosted.service.outbox.stop()
		hosted.service.stopHostedBilling()
		hosted.service.stopIssueMailer()
		hosted.service.config.TenantRepository, hosted.service.config.AccessRepository = repository, repository
		fixtures = append(fixtures, diagnosticsPostgresFixture{repository: repository, admin: admin, details: details, hosted: hosted, owner: owner, organization: organization, queries: capture})
	}
	return fixtures
}

func diagnosticsFixtureAttempts(ctx context.Context, t testing.TB, f diagnosticsPostgresFixture, project tracker.ProjectID, item tracker.NativeWorkItemID, prefix string, population, objects int, started, updated time.Time, data tracker.NativeRunData) {
	t.Helper()
	if err := f.repository.WriteTenant(ctx, f.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.Project(ctx, project); errors.Is(err, corestore.ErrNotFound) {
			if _, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: f.organization, ID: project, Name: "Other project", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		state := "Todo"
		number, err := w.NextWorkItemNumber(ctx, project)
		if err != nil {
			return err
		}
		_, err = w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: f.organization, ProjectID: project, NativeID: item, Number: number, Title: "Diagnostic source", WorkflowStateName: &state, NativeCreatedAt: started, NativeUpdatedAt: updated, CreatedAt: started, UpdatedAt: updated})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES($1,'diagnostics-machine','example',1,'example',$6,$6,$6) ON CONFLICT DO NOTHING`,
		`INSERT INTO native_attempt_runs(organization_id,run_id,issue_id) SELECT $1,$4||'-run-'||n,i.id FROM generate_series(1,$5::integer)n JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3`,
		`INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,acquired_at,renewed_at,expires_at,released_at) SELECT $1,$4||'-lease-'||n,i.id,'diagnostics-machine',$4||'-session-'||n,1,$6,$6,greatest($6::timestamptz,$7::timestamptz)+interval '1 hour',greatest($6::timestamptz,$7::timestamptz) FROM generate_series(1,$5::integer)n JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3`,
		`INSERT INTO native_attempts(organization_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,updated_at,project_id) SELECT $1,$4||'-attempt-'||n,i.id,l.lease_id,l.fencing_token,$4||'-run-'||n,1,'failed',1,1,'fixture/payload',$6,$7,i.project_id FROM generate_series(1,$5::integer)n JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 JOIN leases l ON l.organization_id=$1 AND l.lease_id=$4||'-lease-'||n`,
	}
	for index, statement := range statements {
		args := []any{f.organization, project, item, prefix, population, started, updated}
		if index == 1 {
			args = args[:5]
		}
		if index == 0 {
			statement = `INSERT INTO machines(organization_id,id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES($1,'diagnostics-machine','example',1,'example',$2,$2,$2) ON CONFLICT DO NOTHING`
			args = []any{f.organization, started}
		}
		if _, err := f.admin.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	content := eventstore.NewContent(f.details)
	events := make([]eventstore.Event, 0, objects)
	for index := 1; index <= objects; index++ {
		value := data
		value.AttemptID, value.RunID, value.Sequence = fmt.Sprintf("%s-attempt-%d", prefix, index), fmt.Sprintf("%s-run-%d", prefix, index), 1
		raw, err := json.Marshal(tracker.CollaborationEvent{ID: fmt.Sprintf("%s-event-%d", prefix, index), OrganizationID: f.organization, ProjectID: project, AggregateID: item, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "example-runner"}, RecordedAt: updated, Data: tracker.CollaborationData{Run: &value}})
		if err != nil {
			t.Fatal(err)
		}
		event, err := content.PrepareEvent(ctx, f.organization, raw)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) > 0 {
		if _, err := f.admin.ExecContext(ctx, `UPDATE native_attempts SET updated_at=$5::timestamptz-(split_part(id,'-attempt-',2)::integer)*interval '1 microsecond' WHERE organization_id=$1 AND issue_id=(SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3) AND id LIKE $4||'-attempt-%'`, f.organization, project, item, prefix, updated); err != nil {
			t.Fatal(err)
		}
		if err := f.repository.WriteTenant(ctx, f.organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, event := range events {
				if _, err := w.AppendEvent(ctx, event); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func diagnosticsFixtureSnapshot(t *testing.T, f diagnosticsPostgresFixture, project tracker.ProjectID, w operatortool.AnalyticsWindow) diagnosticstore.Snapshot {
	t.Helper()
	var snapshot diagnosticstore.Snapshot
	if err := f.repository.ReadTenantSnapshot(t.Context(), f.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		snapshot, err = r.(diagnosticstore.Reader).DiagnosticsSnapshot(t.Context(), []tracker.ProjectID{project}, w.From, w.To, w.To, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestDiagnosticsProjectionReadBound(t *testing.T) {
	t.Parallel()
	for _, f := range diagnosticsPostgresFixtures(t) {
		t.Run(string(f.organization), func(t *testing.T) {
			now := f.hosted.service.config.Clock().UTC().Truncate(time.Hour)
			w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
			history := map[tracker.OrganizationID]int{"org_first": 4 * maxAnalyticsPopulation, "org_second": 40 * maxAnalyticsPopulation}[f.organization]
			for _, project := range []tracker.ProjectID{f.hosted.project, "prj_diagnostics_other"} {
				data := tracker.NativeRunData{Outcome: "failed", Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict"}}}
				diagnosticsFixtureAttempts(t.Context(), t, f, project, tracker.NativeWorkItemID(string(project)+"-diagnostic-source"), string(project)+"-recent", 4*maxAnalyticsPopulation, maxAnalyticsPopulation, now.Add(-2*time.Hour), now.Add(-time.Hour), data)
				diagnosticsFixtureAttempts(t.Context(), t, f, project, tracker.NativeWorkItemID(string(project)+"-historical-source"), string(project)+"-old", history, 0, now.Add(-2400*time.Hour), now.Add(-2400*time.Hour), data)
			}
			if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT a.organization_id,i.project_id,a.id,a.updated_at+n*interval '1 microsecond','provider','model',1,0,1,1,'USD',a.updated_at FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id CROSS JOIN generate_series(1,12)n WHERE a.organization_id=$1`, f.organization); err != nil {
				t.Fatal(err)
			}
			if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_attempts SET status='running' WHERE organization_id=$1 AND id=$2`, f.organization, string(f.hosted.project)+"-recent-attempt-1"); err != nil {
				t.Fatal(err)
			}
			for _, relation := range []string{"native_attempts", "attempt_usage", "issues"} {
				if _, err := f.admin.ExecContext(t.Context(), "ANALYZE "+relation); err != nil {
					t.Fatal(err)
				}
			}
			credential := hubapi.APICredential{Hosted: f.owner.identity.Hosted}
			report, err := f.hosted.service.diagnosticsTenantReport(t.Context(), credential, w, f.hosted.project)
			if err != nil {
				t.Fatal(err)
			}
			if report.Coverage[1].Observed == nil || *report.Coverage[1].Observed != maxAnalyticsPopulation || !report.Partial || len(report.Refused) != 1 || report.Refused[0].Reason != "conflict" || report.Refused[0].Count != maxAnalyticsPopulation {
				t.Fatalf("bounded diagnostics population: %+v", report)
			}
			for _, kind := range []string{"attempts", "usage"} {
				diagnosticsAssertIndexedPlan(t, f, kind)
			}
			if _, err := f.hosted.service.readDiagnosticsTenant(t.Context(), credential, w, "prj_foreign"); err != operatortool.ErrAccessDenied {
				t.Fatalf("unreadable diagnostic project error=%v", err)
			}
		})
	}
}

func TestDiagnosticsRefusalPageByteParity(t *testing.T) {
	t.Parallel()
	for _, f := range diagnosticsPostgresFixtures(t) {
		t.Run(string(f.organization), func(t *testing.T) {
			now := f.hosted.service.config.Clock().UTC().Truncate(time.Hour)
			w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
			signatures := make([]nativeFailureSignature, 0, 51)
			for i := range 50 {
				example := fmt.Sprintf("failure %c%c ", 'g'+i/20, 'g'+i%20) + strings.Repeat("z", 501)
				signatures = append(signatures, nativeFailureSignature{Signature: example[:256], Example: example[:512], Class: "attempt", Count: 1, WorkItems: []string{fmt.Sprintf("item-%d", i)}, WorkItemsCount: 1, FirstSeen: now.Add(-time.Hour), LastSeen: now.Add(-time.Hour), Cost: 123456789.125, Tokens: 999999999999, SlotMinutes: 60})
			}
			signatures = append(signatures, nativeFailureSignature{Signature: "landing refused: conflict", Example: "landing refused: conflict", Class: "merge", Count: 1, WorkItems: []string{"conflict-item"}, WorkItemsCount: 1, FirstSeen: now.Add(-time.Hour), LastSeen: now.Add(-time.Hour), Cost: 123456789.125, Tokens: 999999999999, SlotMinutes: 60})
			size := 2
			for _, signature := range signatures {
				raw, err := json.Marshal(signature)
				if err != nil {
					t.Fatal(err)
				}
				size += len(raw) + 1
			}
			padding := operatortool.WorkListPageBytes + 1 - size
			if padding < 0 {
				t.Fatalf("fixture already exceeds byte limit: %d", size)
			}
			for i := range 50 {
				signatures[i].WorkItems[0] += strings.Repeat("z", padding/50)
				if i < padding%50 {
					signatures[i].WorkItems[0] += "z"
				}
			}
			for i, signature := range signatures {
				data := tracker.NativeRunData{Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: signature.Example}}
				if signature.Class == "merge" {
					data = tracker.NativeRunData{Outcome: "failed", Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict"}}}
				}
				prefix := fmt.Sprintf("boundary-%d", i)
				diagnosticsFixtureAttempts(t.Context(), t, f, f.hosted.project, tracker.NativeWorkItemID(signature.WorkItems[0]), prefix, 1, 1, now.Add(-2*time.Hour), signature.LastSeen, data)
				if _, err := f.admin.ExecContext(t.Context(), `UPDATE native_attempts SET updated_at=$3 WHERE organization_id=$1 AND id=$2`, f.organization, prefix+"-attempt-1", signature.LastSeen); err != nil {
					t.Fatal(err)
				}
				if _, err := f.admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,$2,$3,$4,'provider','model',$5,0,0,$6,'USD',$4)`, f.organization, f.hosted.project, prefix+"-attempt-1", signature.LastSeen, signature.Tokens, signature.Cost); err != nil {
					t.Fatal(err)
				}
			}
			snapshot := diagnosticsFixtureSnapshot(t, f, f.hosted.project, w)
			values := make(map[string]tracker.NativeRunData)
			content := eventstore.NewContent(f.details)
			for _, attempt := range snapshot.AttemptRecords {
				event, err := content.Event(t.Context(), attempt.Event)
				if err != nil {
					t.Fatal(err)
				}
				if event.Data.Run == nil {
					t.Fatal("typed attempt lost payload")
				}
				values[attempt.ID] = *event.Data.Run
			}
			failures := diagnosticNativeFailures(snapshot, values, w)
			actual := groupNativeFailures(failures)
			if !reflect.DeepEqual(actual, signatures) {
				t.Fatal("typed PostgreSQL failures lost full signatures, cost, tokens, timing or identity")
			}
			for _, test := range []struct {
				name                       string
				offset, limit, count, trim int
			}{{"byte clipped before refusal", 0, 100, 50, 0}, {"exact byte limit includes refusal", 0, 100, 51, 1}, {"below byte limit includes refusal", 0, 100, 51, 2}, {"row clipped before refusal", 0, 50, 50, 0}, {"refusal on next page", 50, 100, 1, 0}} {
				t.Run(test.name, func(t *testing.T) {
					expected := append([]nativeFailureSignature(nil), signatures...)
					first := signatures[0].WorkItems[0]
					expected[0].WorkItems = []string{first[:len(first)-test.trim]}
					input := append([]nativeFailureSignature(nil), actual...)
					input[0].WorkItems = []string{actual[0].WorkItems[0][:len(actual[0].WorkItems[0])-test.trim]}
					got := nativeFailureSignaturePage(input, test.offset, test.limit)
					want := nativeFailureSignaturePage(expected, test.offset, test.limit)
					if len(got.Items) != test.count || !reflect.DeepEqual(got, want) {
						t.Fatalf("failure page differs from existing byte/row pagination: count=%d want=%d", len(got.Items), test.count)
					}
					if snapshot.Partial {
						t.Fatal("byte or row pagination changed population partial")
					}
				})
			}
		})
	}
}

type diagnosticsSnapshotGuard struct {
	tenantstore.Repository
	active atomic.Int64
}

func (g *diagnosticsSnapshotGuard) ReadTenantSnapshot(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	g.active.Add(1)
	defer g.active.Add(-1)
	return g.Repository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(ctx, organization, workload, read)
}

type diagnosticsSQLStatement struct {
	sql  string
	args []any
}
type diagnosticsSQLCapture struct {
	pgx.QueryTracer
	mu         sync.Mutex
	statements map[string]diagnosticsSQLStatement
}

func (c *diagnosticsSQLCapture) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	kind := ""
	if strings.Contains(data.SQL, "WITH completed AS MATERIALIZED") {
		kind = "attempts"
	}
	if strings.Contains(data.SQL, "FROM report_usage_current u") && strings.Contains(data.SQL, "LIMIT $4") {
		kind = "usage"
	}
	if kind != "" {
		c.mu.Lock()
		c.statements[kind] = diagnosticsSQLStatement{sql: data.SQL, args: append([]any(nil), data.Args...)}
		c.mu.Unlock()
	}
	if c.QueryTracer != nil {
		return c.QueryTracer.TraceQueryStart(ctx, conn, data)
	}
	return ctx
}
func (c *diagnosticsSQLCapture) TraceQueryEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryEndData) {
	if c.QueryTracer != nil {
		c.QueryTracer.TraceQueryEnd(ctx, conn, data)
	}
}

type diagnosticsSQLPlan struct {
	Index        string               `json:"Index Name"`
	Relation     string               `json:"Relation Name"`
	Rows         float64              `json:"Actual Rows"`
	Loops        float64              `json:"Actual Loops"`
	Removed      float64              `json:"Rows Removed by Filter"`
	IndexRemoved float64              `json:"Rows Removed by Index Recheck"`
	Plans        []diagnosticsSQLPlan `json:"Plans"`
}

func diagnosticsAssertIndexedPlan(t *testing.T, f diagnosticsPostgresFixture, kind string) {
	t.Helper()
	f.queries.mu.Lock()
	statement, ok := f.queries.statements[kind]
	f.queries.mu.Unlock()
	if !ok {
		t.Fatalf("diagnostics did not execute its %s owner query", kind)
	}
	tx, err := f.admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "SET LOCAL ROLE detent_runtime_v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", string(f.organization)); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statement.sql, statement.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct{ Plan diagnosticsSQLPlan }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("decode %s diagnostic plan: %v", kind, err)
	}
	indexes := make(map[string]bool)
	visits := make(map[string]float64)
	var inspect func(diagnosticsSQLPlan)
	inspect = func(node diagnosticsSQLPlan) {
		if node.Loops > 0 {
			indexes[node.Index] = true
		}
		visits[node.Relation] += (node.Rows + node.Removed + node.IndexRemoved) * node.Loops
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(plans[0].Plan)
	if kind == "attempts" {
		if visits["native_attempts"] > 2*float64(diagnosticstore.Limit+1) || visits["report_attempt_facts"] > diagnosticstore.AttemptLimit+1 || visits["collaboration_events"] > diagnosticstore.AttemptLimit+1 {
			t.Fatalf("attempt candidates lost indexed bounded reads: %s", raw)
		}
	} else if !indexes["attempt_usage_pkey"] && !indexes["attempt_usage_report_window_idx"] || kind == "usage" && visits["attempt_usage"] > 2*float64(diagnosticstore.Limit+1) {
		t.Fatalf("diagnostic usage lost indexed candidate join: %s", raw)
	}
}

func (c *diagnosticsSQLCapture) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if tracer, ok := c.QueryTracer.(pgx.BatchTracer); ok {
		return tracer.TraceBatchStart(ctx, conn, data)
	}
	return ctx
}
func (c *diagnosticsSQLCapture) TraceBatchQuery(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchQueryData) {
	if tracer, ok := c.QueryTracer.(pgx.BatchTracer); ok {
		tracer.TraceBatchQuery(ctx, conn, data)
	}
}
func (c *diagnosticsSQLCapture) TraceBatchEnd(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchEndData) {
	if tracer, ok := c.QueryTracer.(pgx.BatchTracer); ok {
		tracer.TraceBatchEnd(ctx, conn, data)
	}
}
