package executionrecords_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attemptPageTrace struct {
	mu         sync.Mutex
	statements []attemptPageStatement
}

type attemptPageStatement struct {
	query string
	args  []any
}

func (a *attemptPageTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "-- name: PageIssueAttempts :many") {
		a.mu.Lock()
		a.statements = append(a.statements, attemptPageStatement{data.SQL, append([]any(nil), data.Args...)})
		a.mu.Unlock()
	}
	return ctx
}

func (*attemptPageTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (a *attemptPageTrace) take() []attemptPageStatement {
	a.mu.Lock()
	defer a.mu.Unlock()
	statements := a.statements
	a.statements = nil
	return statements
}

type attemptPagePlan struct {
	Node     string            `json:"Node Type"`
	Relation string            `json:"Relation Name"`
	Index    string            `json:"Index Name"`
	Rows     float64           `json:"Actual Rows"`
	Removed  float64           `json:"Rows Removed by Filter"`
	Recheck  float64           `json:"Rows Removed by Index Recheck"`
	Loops    float64           `json:"Actual Loops"`
	Plans    []attemptPagePlan `json:"Plans"`
}

func TestAttemptPageWorkStaysBoundedInEveryStatisticsState(t *testing.T) {
	t.Parallel()
	const pageSize, rowVisits = 100, 512
	admin := pgstoretest.Template.Database(t)
	original, pool := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	trace := &attemptPageTrace{}
	repository, err := postgres.NewRepository(t.Context(), pgstoretest.TracedFeaturePool(t, pool, trace), pgstoretest.SmallPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, relation := range []string{"leases", "native_attempts"} {
		if _, err := admin.ExecContext(t.Context(), "ALTER TABLE "+relation+" SET (autovacuum_enabled=false, toast.autovacuum_enabled=false)"); err != nil {
			t.Fatal(err)
		}
	}
	templates := map[string]string{}
	for _, seed := range []struct {
		org      tracker.OrganizationID
		issue    int64
		attempts int
	}{{"org_first", 1, 15}, {"org_first", 2, 1}, {"org_second", 1, 1}} {
		if seed.issue == 1 {
			pgstoretest.SeedLeaseFamily(t, original, seed.org)
		}
		for i := range seed.attempts {
			claim := pgstoretest.LeaseClaim(seed.issue)
			claim.LeaseID, claim.SessionID = fmt.Sprintf("lease_%d_%d", seed.issue, i), fmt.Sprintf("session_%d_%d", seed.issue, i)
			lease := pgstoretest.ClaimLease(t, original, seed.org, claim)
			pgstoretest.RecordLeaseAttempt(t, original, seed.org, pgstoretest.AttemptEvent(lease))
			if err := original.WriteLeases(t.Context(), seed.org, corestore.Serving, func(w leasestore.Writer) error {
				_, err := w.Release(t.Context(), pgstoretest.LeaseFence(lease), "finished")
				return err
			}); err != nil {
				t.Fatal(err)
			}
			templates[fmt.Sprintf("%s/%d", seed.org, seed.issue)] = lease.LeaseID
		}
	}
	type history struct {
		org   tracker.OrganizationID
		issue int64
	}
	seeded := map[history]int{}
	for _, step := range []struct {
		name                     string
		target, sibling, foreign int
	}{{"tiny", 0, 0, 0}, {"crossover", 0, 200, 200}, {"large", 5000, 20000, 20000}} {
		for _, grow := range []struct {
			key   history
			count int
		}{{history{"org_first", 1}, step.target}, {history{"org_first", 2}, step.sibling}, {history{"org_second", 1}, step.foreign}} {
			seedAttemptPageHistory(t, admin, grow.key.org, templates[fmt.Sprintf("%s/%d", grow.key.org, grow.key.issue)], grow.key.issue, seeded[grow.key]+1, grow.count)
			seeded[grow.key] = max(seeded[grow.key], grow.count)
		}
		targetAttempts := 15 + seeded[history{"org_first", 1}]
		for _, state := range []string{"statistics before this growth", "ANALYZE", "VACUUM (ANALYZE)"} {
			if state != "statistics before this growth" {
				if _, err := admin.ExecContext(t.Context(), state+" leases,native_attempts"); err != nil {
					t.Fatal(err)
				}
			}
			t.Run(step.name+"/"+state, func(t *testing.T) {
				var pages []leasestore.AttemptPage
				cursor := leasestore.AttemptCursor{}
				for len(pages) < 2 {
					var page leasestore.AttemptPage
					if err := repository.ReadLeases(t.Context(), "org_first", corestore.Serving, func(r leasestore.Reader) error {
						var err error
						page, err = r.AttemptPage(t.Context(), 1, cursor, pageSize)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					pages = append(pages, page)
					if page.Next == (leasestore.AttemptCursor{}) {
						break
					}
					cursor = page.Next
				}
				want := min(targetAttempts, pageSize)
				if len(pages[0].Items) != want {
					t.Fatalf("first page attempts=%d want %d", len(pages[0].Items), want)
				}
				for _, page := range pages {
					for _, attempt := range page.Items {
						if attempt.IssueID != 1 || attempt.OrganizationID != "org_first" {
							t.Fatalf("page returned attempt %s of %s/%d", attempt.ID, attempt.OrganizationID, attempt.IssueID)
						}
					}
				}
				statements := trace.take()
				if len(statements) != len(pages) {
					t.Fatalf("attempt pages ran %d PageIssueAttempts statements for %d pages", len(statements), len(pages))
				}
				for _, statement := range statements {
					if visits, raw := attemptPageVisits(t, admin, statement, ""); visits > rowVisits {
						t.Errorf("attempt page visited %.0f native_attempts rows, above %d: %s", visits, rowVisits, raw)
					}
				}
				if step.name == "large" {
					if visits, raw := attemptPageVisits(t, admin, statements[0], "DROP INDEX native_attempts_item_idx"); visits <= rowVisits {
						t.Fatalf("work bound accepted the page without its item index: %s", raw)
					}
				}
			})
		}
	}
}

func attemptPageVisits(t *testing.T, admin *sql.DB, statement attemptPageStatement, prepare string) (float64, []byte) {
	t.Helper()
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, setup := range []string{prepare, "SET LOCAL ROLE detent_runtime_v2"} {
		if setup == "" {
			continue
		}
		if _, err := tx.ExecContext(t.Context(), setup); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.ExecContext(t.Context(), "SELECT set_config('detent.organization_id',$1,true)", "org_first"); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := tx.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) "+statement.query, statement.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct{ Plan attemptPagePlan }
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("plan=%s error=%v", raw, err)
	}
	visits := 0.0
	var walk func(attemptPagePlan)
	walk = func(node attemptPagePlan) {
		if node.Relation == "native_attempts" {
			visits = max(visits, (node.Rows+node.Removed+node.Recheck)*node.Loops)
		}
		for _, child := range node.Plans {
			walk(child)
		}
	}
	walk(plans[0].Plan)
	return visits, raw
}

func seedAttemptPageHistory(t *testing.T, admin *sql.DB, organization tracker.OrganizationID, template string, issue int64, first, last int) {
	t.Helper()
	if first > last {
		return
	}
	prefix := fmt.Sprintf("history_%d_", issue)
	overrides := map[string]map[string]string{
		"leases":          {"lease_id": "$3||n", "session_id": "$3||'session_'||n"},
		"native_attempts": {"id": "'attempt_'||h.lease_id", "lease_id": "h.lease_id", "fencing_token": "h.fencing_token", "status": "'succeeded'"},
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
