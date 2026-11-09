package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/claimstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type recoveryStatementTrace struct {
	query string
	args  []any
	conn  *pgx.Conn
}

func (p *recoveryStatementTrace) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "FROM native_attempts a JOIN leases l") {
		p.query, p.args, p.conn = data.SQL, append([]any(nil), data.Args...), conn
	}
	return ctx
}
func (*recoveryStatementTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestRecordedRecoveryUsesCurrentStatementWithoutBlockerLocks(t *testing.T) {
	admin := template.Database(t)
	original, pool := repositoryFor(t, admin, smallPool())
	admin.SetMaxOpenConns(2)
	var database string
	if err := admin.QueryRowContext(t.Context(), "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" CONNECTION LIMIT 8"); err != nil {
		t.Fatal(err)
	}
	trace := &recoveryStatementTrace{}
	repository, err := postgres.NewRepository(t.Context(), tracedFeaturePool(t, pool, trace), smallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	content := eventstore.NewContent(newHistoryObjects())
	recoveries := map[tracker.OrganizationID]leasestore.RecordedRecovery{}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		recoveries[org] = seedRecordedRecovery(t, original, admin, content, org)
		for _, query := range []string{
			`INSERT INTO project_settings(organization_id,project_id) VALUES($1,'prj_1')`,
			`UPDATE projects SET require_dependencies=true WHERE organization_id=$1 AND id='prj_1'`,
			`INSERT INTO project_workflow_states(organization_id,project_id,name,board_rank,terminal,dispatchable,operator_only) VALUES($1,'prj_1','Todo',1,false,true,false),($1,'prj_2','Done',0,true,false,false),($1,'prj_2','Todo',1,false,true,false)`,
			`INSERT INTO projects(organization_id,id,name,profile) SELECT $1,'prj_metadata_'||n,'Metadata '||n,'native' FROM generate_series(1,100) n`,
			`INSERT INTO project_workflow_states(organization_id,project_id,name,board_rank,terminal,dispatchable,operator_only) SELECT $1,'prj_metadata_'||p,'State '||n,n,true,false,false FROM generate_series(1,100) p CROSS JOIN generate_series(1,20) n`,
		} {
			if _, err := admin.ExecContext(t.Context(), query, org); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.PutWorkItemFacts(t.Context(), claimstore.WorkItemFacts{OrganizationID: org, ProjectID: "prj_1", WorkItemID: "wi_1", Assignees: []string{}})
		}); err != nil {
			t.Fatal(err)
		}
		seedRecoveryDependencies(t, admin, org, 20000)
		for _, query := range []string{
			`INSERT INTO issues(organization_id,id,native_id,project_id,number,title,workflow_state_name) VALUES($1,50000,'wi_unrelated','prj_2',2,'Example','Done')`,
			`ALTER TABLE issue_dependencies DISABLE TRIGGER USER`,
			`INSERT INTO issue_dependencies(organization_id,dependent_project_id,dependent_issue_id,blocker_project_id,blocker_issue_id) SELECT $1,'prj_2',50000,'prj_1',n FROM generate_series(3,20002) n`,
			`ALTER TABLE issue_dependencies ENABLE TRIGGER USER`,
		} {
			args := []any{}
			if strings.Contains(query, "$1") {
				args = append(args, org)
			}
			if _, err := admin.ExecContext(t.Context(), query, args...); err != nil {
				t.Fatal(err)
			}
		}
		seedRecoveryDependencies(t, admin, org, 1)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		recovery := recoveries[org]
		for _, test := range []struct{ count, reported int }{{201, 1}, {10001, 1}, {20000, 1}, {20000, corestore.MaxPageSize}} {
			count := test.count
			t.Run(fmt.Sprintf("%s/ready_D%d_R%d", org, count, test.reported), func(t *testing.T) {
				seedRecoveryDependencies(t, admin, org, count)
				requested := recovery
				requested.DependencyNumbers = make([]int64, test.reported)
				for i := range requested.DependencyNumbers {
					requested.DependencyNumbers[i] = int64(i + 2)
				}
				err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if err := lockRecordedRecovery(t.Context(), w, recovery); err != nil {
						return err
					}
					if err := w.(leasestore.RecordedRecoveryWriter).AuthorizeRecordedRecovery(t.Context(), requested); err != nil {
						return err
					}
					checkRecoveryStatementPlan(t, trace, org, count, test.reported)
					_, err := admin.ExecContext(t.Context(), `SELECT id FROM issues WHERE organization_id=$1 AND id=$2 FOR UPDATE NOWAIT`, org, count+2)
					if err != nil {
						t.Errorf("recovery locked an unreported blocker: %v", err)
					}
					if count == 20000 && test.reported == 1 {
						other := tracker.OrganizationID("org_first")
						if org == other {
							other = "org_second"
						}
						return repository.WriteLeases(t.Context(), other, corestore.Background, func(w leasestore.Writer) error { _, err := w.Claim(t.Context(), leaseClaim(2)); return err })
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			})
		}
		t.Run(string(org)+"/unfinished_omitted_last", func(t *testing.T) {
			if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET workflow_state_name='Todo' WHERE organization_id=$1 AND id=20002`, org); err != nil {
				t.Fatal(err)
			}
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := lockRecordedRecovery(t.Context(), w, recovery); err != nil {
					return err
				}
				return w.(leasestore.RecordedRecoveryWriter).AuthorizeRecordedRecovery(t.Context(), recovery)
			})
			if !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("unreported unfinished blocker error=%v", err)
			}
		})
		t.Run(string(org)+"/reopen_after_graph_wait", func(t *testing.T) {
			seedRecoveryDependencies(t, admin, org, 1)
			blocker, err := admin.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := blocker.ExecContext(t.Context(), `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR NO KEY UPDATE`, org); err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := blocker.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&pid); err != nil {
				t.Fatal(err)
			}
			task := startRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					if err := lockRecordedRecovery(ctx, w, recovery); err != nil {
						return err
					}
					return w.(leasestore.RecordedRecoveryWriter).AuthorizeRecordedRecovery(ctx, recovery)
				})
			})
			t.Cleanup(func() { _ = blocker.Rollback() })
			testenv.WaitUntil(t, t.Context(), func() error {
				var waiting bool
				if err := admin.QueryRowContext(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)) AND query LIKE '-- name: LockRelationshipGraph :one%')`, pid).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("recovery has not reached the relationship lock")
				}
				return nil
			})
			if _, err := blocker.ExecContext(t.Context(), `UPDATE issues SET workflow_state_name='Todo' WHERE organization_id=$1 AND id=3`, org); err != nil {
				t.Fatal(err)
			}
			if err := blocker.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := task.wait(t); !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("post-wait readiness error=%v", err)
			}
		})
		t.Run(string(org)+"/reopen_before_next_claim", func(t *testing.T) {
			seedRecoveryDependencies(t, admin, org, 1)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := lockRecordedRecovery(t.Context(), w, recovery); err != nil {
					return err
				}
				if err := w.(leasestore.RecordedRecoveryWriter).AuthorizeRecordedRecovery(t.Context(), recovery); err != nil {
					return err
				}
				row, err := w.WorkItem(t.Context(), "prj_1", "wi_1")
				if err != nil {
					return err
				}
				row.WorkflowStateName = new("Todo")
				_, err = w.UpdateWorkItem(t.Context(), row)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			check := func(want int) {
				t.Helper()
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.CandidatePage(t.Context(), claimstore.CandidateQuery{ProjectID: "prj_1", WorkItemID: "wi_1", AvailableAt: recovery.Now, Limit: 1})
					if err == nil && len(page.Items) != want {
						t.Errorf("next claim candidates=%d want=%d", len(page.Items), want)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			check(1)
			if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET workflow_state_name='Todo' WHERE organization_id=$1 AND id=3`, org); err != nil {
				t.Fatal(err)
			}
			check(0)
		})
	}
}

func lockRecordedRecovery(ctx context.Context, w tenantstore.Writer, r leasestore.RecordedRecovery) error {
	if err := w.LockRelationshipGraph(ctx); err != nil {
		return err
	}
	_, err := w.LockWorkItem(ctx, r.ProjectID, r.WorkItemID)
	return err
}

func seedRecoveryDependencies(t *testing.T, admin *sql.DB, org tracker.OrganizationID, count int) {
	t.Helper()
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, query := range []string{
		`ALTER TABLE issue_dependencies DISABLE TRIGGER USER`,
		`DELETE FROM issue_dependencies WHERE organization_id=$1 AND dependent_issue_id=1`,
		`INSERT INTO issues(organization_id,id,native_id,project_id,number,title,workflow_state_name) SELECT $1,n,'wi_dependency_'||n,'prj_1',n-1,'Example','Done' FROM generate_series(4,$2::bigint+2) n ON CONFLICT (organization_id,id) DO NOTHING`,
		`UPDATE issues SET workflow_state_name='Done' WHERE organization_id=$1 AND id>=3 AND workflow_state_name IS DISTINCT FROM 'Done'`,
		`ANALYZE issues`,
		`ANALYZE work_item_identities`,
		`INSERT INTO issue_dependencies(organization_id,dependent_project_id,dependent_issue_id,blocker_project_id,blocker_issue_id) SELECT $1,'prj_1',1,'prj_1',n FROM generate_series(3,$2::bigint+2) n`,
		`ALTER TABLE issue_dependencies ENABLE TRIGGER USER`,
		`ANALYZE issue_dependencies`,
		`ANALYZE project_workflow_states`,
	} {
		args := []any{}
		if strings.Contains(query, "$1") {
			args = append(args, org)
		}
		if strings.Contains(query, "$2") {
			args = append(args, count)
		}
		if _, err := tx.ExecContext(t.Context(), query, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

type recoveryStatementPlan struct {
	Kind     string  `json:"Node Type"`
	Relation string  `json:"Relation Name"`
	Rows     float64 `json:"Actual Rows"`
	Removed  float64 `json:"Rows Removed by Filter"`
	Loops    float64 `json:"Actual Loops"`
	Plans    []recoveryStatementPlan
}

func checkRecoveryStatementPlan(t *testing.T, trace *recoveryStatementTrace, org tracker.OrganizationID, count, reported int) {
	t.Helper()
	var raw []byte
	if err := trace.conn.QueryRow(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+trace.query, trace.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct{ Plan recoveryStatementPlan }
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	visits := map[string]float64{}
	var locks float64
	var walk func(recoveryStatementPlan)
	walk = func(p recoveryStatementPlan) {
		if p.Relation == "issue_dependencies" && p.Kind == "Seq Scan" {
			t.Errorf("dependency eligibility used an unrelated-table scan")
		}
		if p.Relation != "" {
			visits[p.Relation] += (p.Rows + p.Removed) * p.Loops
		}
		if p.Kind == "LockRows" {
			locks += p.Rows * p.Loops
		}
		for _, child := range p.Plans {
			walk(child)
		}
	}
	walk(plans[0].Plan)
	path := filepath.Join(os.Getenv("TMPDIR"), fmt.Sprintf("recovery-plan-%s-D%d-R%d.json", org, count, reported))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("valid recovery D=%d R=%d visits=%v LockRows=%.0f plan=%s", count, reported, visits, locks, path)
	if locks != 0 || visits["issue_dependencies"] > float64(count+reported) || visits["issues"] > float64(count+reported) {
		t.Errorf("recovery traversed unrelated rows or locked blockers: visits=%v LockRows=%.0f", visits, locks)
	}
}
