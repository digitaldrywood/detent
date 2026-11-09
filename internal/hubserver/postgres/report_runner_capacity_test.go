package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type capacityQueryTrace struct {
	lease       string
	args        []any
	process     string
	processArgs []any
	reads       int
}

func (c *capacityQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	normalized := strings.ToLower(strings.Join(strings.Fields(data.SQL), " "))
	if strings.HasPrefix(normalized, "explain") || strings.HasPrefix(normalized, "prepare") {
		return ctx
	}
	if strings.Contains(normalized, "from leases") && strings.Contains(normalized, "runner_lease_assignments") && strings.Contains(normalized, "least(") {
		c.lease = data.SQL
		c.args = append([]any(nil), data.Args...)
		c.reads++
	} else if strings.Contains(normalized, "from native_attempts") && strings.Contains(normalized, "report_attempt_facts") {
		c.process = data.SQL
		c.processArgs = append([]any(nil), data.Args...)
		c.reads++
	}
	return ctx
}
func (*capacityQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRunnerCapacityFactsClipLeaseWindowsAndExcludeOldHistory(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	cfg := pool.Config()
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	trace := new(capacityQueryTrace)
	cfg.ConnConfig.Tracer = trace
	var err error
	pool, err = pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	repository, err = postgres.NewRepository(t.Context(), pool, smallPool())
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	from := historyAt().UTC().Truncate(time.Hour)
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedLeaseFamily(t, repository, org)
		for index := int64(1); index <= 2; index++ {
			runner := fmt.Sprintf("runner_%d", index)
			machine := fmt.Sprintf("machine_%d", index)
			seedRunnerIdentity(t, repository, org, runner, machine, from, false)
			receipt := func(operation, key string) eventstore.Receipt {
				t.Helper()
				value, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "production-import", Operation: operation, CommandKey: key, RequestHash: artifact.Digest([]byte(operation + key)), CreatedAt: from}, []byte(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				return value
			}
			add := func(id int, start, expires time.Time, released *time.Time) {
				t.Helper()
				key := fmt.Sprintf("%s_lease_%d", runner, id)
				lease := leasestore.Lease{OrganizationID: org, LeaseID: key, FencingToken: index*1000 + int64(id) + 1, IssueID: index, MachineID: machine, SessionID: key, WorkItemRevision: 1, AcquiredAt: start, RenewedAt: start, ExpiresAt: expires, ReleasedAt: released}
				if err := repository.ImportCoreLease(t.Context(), postgres.CoreLeaseImport{Lease: lease, Receipt: receipt("core-lease", key)}); err != nil {
					t.Fatal(err)
				}
				assignment := runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: key, RunnerID: runner, AssignedAt: start}
				if err := repository.ImportCoreRunnerLeaseAssignment(t.Context(), postgres.CoreRunnerLeaseAssignmentImport{Assignment: assignment, Receipt: receipt("core-runner-lease-assignment", key)}); err != nil {
					t.Fatal(err)
				}
				data := tracker.NativeRunData{AttemptID: key, RunID: key, LeaseID: tracker.LeaseID(key), FencingToken: tracker.FencingToken(lease.FencingToken), Sequence: 1, RunnerID: runner, MachineID: tracker.MachineID(machine), Outcome: "succeeded", ProcessUsage: &procgroup.Usage{PeakMemoryBytes: 4096, WallSeconds: 10, UserCPUSeconds: 8, SystemCPUSeconds: 2}}
				value := tracker.CollaborationEvent{ID: key, OrganizationID: org, ProjectID: tracker.ProjectID(fmt.Sprintf("prj_%d", index)), AggregateID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", index)), Type: "run.finished", SchemaVersion: 1, Actor: historyActor(), RecordedAt: expires, Data: tracker.CollaborationData{Run: &data}}
				event, err := content.PrepareEvent(t.Context(), org, historyJSON(t, value))
				if err != nil {
					t.Fatal(err)
				}
				attempt := leasestore.Attempt{OrganizationID: org, ID: key, IssueID: index, LeaseID: key, FencingToken: lease.FencingToken, RunID: key, Sequence: 1, Status: "succeeded", WorkItemRevision: 1, PayloadObjectKey: event.PayloadObjectKey, StartedAt: start, UpdatedAt: expires}
				if err := repository.ImportCoreAttempt(t.Context(), postgres.CoreAttemptImport{Attempt: attempt, Receipt: receipt("core-attempt", key)}); err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
					t.Fatal(err)
				}

			}
			add(0, from.Add(-30*time.Minute), from.Add(30*time.Minute), nil)
			late := from.Add(150 * time.Minute)
			add(1, from.Add(15*time.Minute), from.Add(90*time.Minute), &late)
			early := from.Add(5 * time.Hour)
			add(2, from.Add(4*time.Hour), from.Add(6*time.Hour), &early)
			release := from.Add(4 * time.Hour)
			add(3, from.Add(-2*time.Hour), from.Add(-time.Hour), &release)
			add(4, from.Add(-time.Hour), from, &from)
			end := from.Add(25 * time.Hour)
			add(5, from.Add(24*time.Hour), end, &end)
			read := func(limit int) reportstore.RunnerCapacityFacts {
				t.Helper()
				var facts reportstore.RunnerCapacityFacts
				err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					facts, err = r.(reportstore.RunnerCapacityReader).ReadRunnerCapacityFacts(t.Context(), reportstore.RunnerCapacityQuery{RunnerID: runner, MachineID: tracker.MachineID(machine), From: from, To: from.Add(24 * time.Hour), Limit: limit})
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				return facts
			}
			for _, history := range []int{0, 64, 256} {
				for i := 0; i < history; i++ {
					start := from.Add(-time.Duration(i+10) * 24 * time.Hour)
					end := start.Add(time.Hour)
					add(i+10, start, end, &end)
				}
				if _, err := admin.ExecContext(t.Context(), "ANALYZE"); err != nil {
					t.Fatal(err)
				}
				trace.lease, trace.args, trace.reads = "", nil, 0
				facts := read(10)
				if trace.lease == "" || trace.reads != 2 {
					t.Fatalf("capacity read statements=%d, want two bounded owner reads", trace.reads)
				}

				for _, query := range []struct {
					name, sql, relation string
					args                []any
				}{
					{"occupancy", trace.lease, "leases", trace.args},
					{"process", trace.process, "native_attempts", trace.processArgs},
				} {
					if query.sql == "" {
						t.Fatal("missing populated query trace")
					}
					for _, mode := range []string{"force_generic_plan", "force_custom_plan"} {
						tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{AccessMode: pgx.ReadOnly})
						if err != nil {
							t.Fatal(err)
						}
						if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, string(org)); err != nil {
							_ = tx.Rollback(t.Context())
							t.Fatal(err)
						}
						var runtimeTenant bool
						if err := tx.QueryRow(t.Context(), `SELECT pg_has_role(current_user,'detent_runtime_v2','USAGE') AND NOT pg_has_role(current_user,'detent_platform_v2','USAGE') AND NOT (SELECT rolsuper FROM pg_roles WHERE rolname=current_user) AND current_setting('detent.organization_id')=$1`, string(org)).Scan(&runtimeTenant); err != nil || !runtimeTenant {
							_ = tx.Rollback(t.Context())
							t.Fatalf("capacity plan lacks runtime tenant role: %v", err)
						}
						if _, err := tx.Exec(t.Context(), "SET LOCAL plan_cache_mode="+mode); err != nil {
							_ = tx.Rollback(t.Context())
							t.Fatal(err)
						}
						statement := fmt.Sprintf("capacity_%s_%s_%d_%s_%s", org, runner, history, query.name, mode)
						if _, err := tx.Exec(t.Context(), "PREPARE "+statement+" AS "+query.sql); err != nil {
							_ = tx.Rollback(t.Context())
							t.Fatal(err)
						}
						placeholders := make([]string, len(query.args))
						for i := range placeholders {
							placeholders[i] = fmt.Sprintf("$%d", i+1)
						}
						args := append([]any{pgx.QueryExecModeSimpleProtocol}, query.args...)
						var raw []byte
						if err := tx.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) EXECUTE "+statement+"("+strings.Join(placeholders, ",")+")", args...).Scan(&raw); err != nil {
							_ = tx.Rollback(t.Context())
							t.Fatal(err)
						}
						if err := tx.Rollback(t.Context()); err != nil {
							t.Fatal(err)
						}
						var plan []struct{ Plan searchPlanNode }
						if err := json.Unmarshal(raw, &plan); err != nil || len(plan) != 1 {
							t.Fatalf("decode plan: %v", err)
						}
						var visits func(searchPlanNode) float64
						visits = func(n searchPlanNode) float64 {
							v := float64(0)
							if n.Relation == query.relation {
								v = (n.Rows + n.Removed + n.IndexRemoved) * n.Loops
							}
							for _, child := range n.Plans {
								v += visits(child)
							}
							return v
						}
						count := visits(plan[0].Plan)
						t.Logf("org=%s runner=%s expired_history=%d query=%s mode=%s visited_rows=%.0f owner_reads=%d", org, runner, history, query.name, mode, count, trace.reads)
						bound := float64(4)
						if query.name == "process" {
							bound = 3
						}
						if err := os.WriteFile(filepath.Join(os.Getenv("TMPDIR"), fmt.Sprintf("capacity-r2-plan-%s.json", statement)), raw, 0600); err != nil {
							t.Fatal(err)
						}
						if history == 256 && query.name == "process" {
							for relation, limit := range map[string]float64{"report_attempt_facts": 6, "report_event_facts": 2, "collaboration_events": 2, "work_item_identities": 4} {
								var visited func(searchPlanNode) float64
								visited = func(n searchPlanNode) float64 {
									v := float64(0)
									if n.Relation == relation {
										v = (n.Rows + n.Removed + n.IndexRemoved) * n.Loops
									}
									for _, child := range n.Plans {
										v += visited(child)
									}
									return v
								}
								if n := visited(plan[0].Plan); n > limit {
									t.Fatalf("%s scanned %s history: visits=%.0f plan=%s", mode, relation, n, raw)
								}
							}
						}
						if history == 256 && count > bound {
							t.Fatalf("%s %s scanned old history: visits=%.0f plan=%s", query.name, mode, count, raw)
						}

					}
				}
				if facts.OccupancyPartial || facts.RunsPartial || len(facts.Occupancy) != 3 || len(facts.Runs) != 2 || facts.EligibleRuns != 2 || facts.UnknownRuns != 0 {
					t.Fatalf("history=%d runner=%s facts=%+v", history, runner, facts)
				}
				for i, wantEnd := range []time.Time{from.Add(30 * time.Minute), from.Add(90 * time.Minute), from.Add(5 * time.Hour)} {
					wantFrom := []time.Time{from.Add(-30 * time.Minute), from.Add(15 * time.Minute), from.Add(4 * time.Hour)}[i]
					if facts.Occupancy[i].LeaseID != fmt.Sprintf("%s_lease_%d", runner, i) || !facts.Occupancy[i].From.Equal(wantFrom) || !facts.Occupancy[i].To.Equal(wantEnd) {
						t.Fatalf("lease expiry/release/window clipped incorrectly: %+v", facts.Occupancy)
					}
				}
			}
			clipped := read(2)
			if !clipped.OccupancyPartial || len(clipped.Occupancy) != 2 {
				t.Fatalf("bounded leases=%+v", clipped)
			}
			err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				reader := r.(reportstore.RunnerCapacityReader)
				q := reportstore.RunnerCapacityQuery{RunnerID: runner, MachineID: "foreign_machine", From: from, To: from.Add(24 * time.Hour), Limit: 10}
				foreign, err := reader.ReadRunnerCapacityFacts(t.Context(), q)
				if err != nil {
					return err
				}
				if len(foreign.Occupancy) != 0 || len(foreign.Runs) != 0 {
					t.Fatalf("foreign machine facts=%+v", foreign)
				}
				q.MachineID = tracker.MachineID(machine)
				q.RunnerID = "foreign_runner"
				foreign, err = reader.ReadRunnerCapacityFacts(t.Context(), q)
				if err != nil {
					return err
				}
				if len(foreign.Occupancy) != 0 || len(foreign.Runs) != 0 {
					t.Fatal("foreign runner read occupied another runner's machine")
				}
				q.Limit = 0
				if _, err := reader.ReadRunnerCapacityFacts(t.Context(), q); !errors.Is(err, corestore.ErrInvalidPage) {
					t.Fatalf("unbounded capacity read accepted: %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}
