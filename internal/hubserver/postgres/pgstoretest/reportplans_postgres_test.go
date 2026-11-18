package pgstoretest

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
)

var reportPlanTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE plan_items(organization_id text NOT NULL,id bigint NOT NULL,state text NOT NULL,archived boolean NOT NULL,title text NOT NULL,PRIMARY KEY(organization_id,id));
CREATE INDEX plan_items_open ON plan_items(organization_id,state,id) WHERE NOT archived;
CREATE TABLE plan_events(organization_id text NOT NULL,id bigint NOT NULL,item_id bigint NOT NULL,kind text NOT NULL,recorded_at timestamptz NOT NULL,PRIMARY KEY(organization_id,id));
CREATE INDEX plan_events_item ON plan_events(organization_id,item_id,kind,recorded_at);
CREATE INDEX plan_events_window ON plan_events(organization_id,recorded_at);
INSERT INTO plan_items SELECT org,n,CASE WHEN n%10=0 THEN 'open' ELSE 'done' END,n%10<>0,'item '||n FROM unnest(ARRAY['org_a','org_b']) org CROSS JOIN generate_series(1,4000) n;
INSERT INTO plan_events SELECT i.organization_id,i.id*10+k,i.id,CASE WHEN k=0 THEN 'created' ELSE 'moved' END,timestamptz '2026-01-01'+(i.id*10+k)*interval '1 minute' FROM plan_items i CROSS JOIN generate_series(0,9) k;
ANALYZE plan_items;
ANALYZE plan_events;`)
	return err
})

func TestMain(m *testing.M) {
	code := m.Run()
	if err := reportPlanTemplate.Close(context.Background()); err != nil {
		code = 1
	}
	os.Exit(code)
}

func TestReportPlanControlsOnPostgreSQL(t *testing.T) {
	t.Parallel()
	db := reportPlanTemplate.Database(t)
	indexes, err := LoadReportIndexes(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	contract := ReportPlanContract{Relations: []string{"plan_items", "plan_events"}, Indexes: indexes, Cap: 50000}
	for _, test := range []struct {
		name      string
		statement string
		args      []any
		want      string
	}{
		{"bounded ordered candidate scan", `SELECT i.id FROM plan_items i WHERE i.organization_id=$1 AND NOT i.archived AND i.state='open' ORDER BY i.id LIMIT $2`, []any{"org_a", 51}, ""},
		{"keyed probes multiplied by candidates", `SELECT c.id,latest.recorded_at FROM unnest($2::bigint[]) c(id) CROSS JOIN LATERAL (SELECT * FROM plan_items i WHERE i.organization_id=$1 AND i.id=c.id LIMIT 1) i LEFT JOIN LATERAL (SELECT e.recorded_at FROM plan_events e WHERE e.organization_id=$1 AND e.item_id=i.id AND e.kind='moved' ORDER BY e.recorded_at DESC LIMIT 1) latest ON true`, []any{"org_a", []int64{10, 20, 30, 999999}}, ""},
		{"tenant-wide retained-history scan", `SELECT count(*) FROM plan_events e WHERE e.organization_id=$1`, []any{"org_a"}, "has no proven bound"},
		{"time range with growing density and no cap", `SELECT e.id FROM plan_events e WHERE e.organization_id=$1 AND e.recorded_at>=$2::timestamptz AND e.recorded_at<$2::timestamptz+interval '30 days'`, []any{"org_a", "2026-01-02"}, "has no proven bound"},
		{"limit above a consuming sort", `SELECT e.id FROM plan_events e WHERE e.organization_id=$1 ORDER BY e.kind DESC,e.item_id DESC LIMIT 5`, []any{"org_a"}, "has no proven bound"},
		{"limit with arbitrarily many rejected rows", `SELECT i.id FROM plan_items i WHERE i.organization_id=$1 AND NOT i.archived AND i.state='open' AND i.title LIKE '%never%' ORDER BY i.id LIMIT 1`, []any{"org_a"}, "residual filter"},
		{"nonzero offset", `SELECT i.id FROM plan_items i WHERE i.organization_id=$1 AND NOT i.archived AND i.state='open' ORDER BY i.id LIMIT 5 OFFSET 5`, []any{"org_a"}, "OFFSET 5"},
		{"missing tenant predicate", `SELECT i.id FROM plan_items i WHERE i.id=$1`, []any{int64(10)}, "plan_items i"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var raw []byte
			if err := db.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE,BUFFERS,VERBOSE,FORMAT JSON) "+test.statement, test.args...).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			result, err := CheckReportPlan(raw, test.statement, test.args, contract)
			if err != nil {
				t.Fatal(err)
			}
			problems := strings.Join(result.Problems, "\n")
			if test.want == "" && problems != "" || test.want != "" && !strings.Contains(problems, test.want) {
				t.Fatalf("problems=%q want %q\nplan: %s", problems, test.want, raw)
			}
			if test.want == "" && len(result.Scans) == 0 {
				t.Fatalf("positive control examined no relation: %s", raw)
			}
		})
	}
}

func TestReportSeedVisibility(t *testing.T) {
	t.Parallel()
	db := reportPlanTemplate.Database(t)
	ctx := t.Context()
	reader, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	if _, err := reader.ExecContext(ctx, "SELECT pg_current_snapshot()"); err != nil {
		t.Fatal(err)
	}
	var seed string
	if err := db.QueryRowContext(ctx, "SELECT pg_current_xact_id()::xid::text").Scan(&seed); err != nil {
		t.Fatal(err)
	}
	for _, state := range []struct {
		name     string
		released bool
	}{{"held snapshot", false}, {"released snapshot", true}} {
		t.Run(state.name, func(t *testing.T) {
			if state.released {
				if err := reader.Rollback(); err != nil {
					t.Fatal(err)
				}
			}
			visible, err := reportSeedVisible(t.Context(), db, seed)
			if err != nil || visible != state.released {
				t.Fatalf("seed visible = %t, want %t: %v", visible, state.released, err)
			}
		})
	}
}
