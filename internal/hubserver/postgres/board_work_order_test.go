package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardQueryTracer struct {
	queries  int
	page     string
	args     []any
	lanes    string
	laneArgs []any
}

func (c *boardQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := data.SQL
	c.queries++
	if strings.Contains(query, "GROUP BY b.state") || strings.Contains(query, "FROM native_board_counts c") {
		c.lanes = query
		c.laneArgs = append([]any(nil), data.Args...)
	}
	if strings.HasPrefix(query, "SELECT b.organization_id") || strings.HasPrefix(query, "WITH search_candidates AS MATERIALIZED") {
		c.page = query
		c.args = append([]any(nil), data.Args...)
	}
	return ctx
}
func (c *boardQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestBoardWorkOrderReadsBoundedProjection(t *testing.T) {
	admin := template.Database(t)
	repository, pool := repositoryFor(t, admin, smallPool())
	ctx := t.Context()
	objects := newHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, org)
		if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "prj_example", Name: "Example", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		ref, err := objects.StoreDetail(ctx, string(org), []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		for _, index := range []int{1, 512} {
			first := 1
			if index == 512 {
				first = 2
			}
			if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				last := index
				if index == 512 {
					last = 2
				}
				for number := first; number <= last; number++ {
					id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%04d", number))
					item, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: "prj_example", NativeID: id, Number: int64(number), Title: "Example", WorkflowStateName: new("Todo")})
					if err != nil {
						return err
					}
					row := viewstore.BoardRecord{OrganizationID: org, ProjectID: item.ProjectID, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("prj_example#%d", number)}, Dispatchable: true, CreatedAt: historyAt(), UpdatedAt: historyAt(), Payload: ref}
					if number%2 == 0 {
						row.Labels = []string{"detent:workspace"}
					}
					if err := w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: item.ProjectID, WorkItemID: id, ExpectedRevision: item.Revision, Current: &row}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if index == 512 {
				if err := bulkBoardWorkOrderSeed(ctx, admin, string(org), 3, 512); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					for number := 3; number <= 4; number++ {
						id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%04d", number))
						ownerID := tracker.NativeWorkItemID(fmt.Sprintf("wi_%04d", 2-number%2))
						owner, err := r.BoardItem(ctx, "prj_example", ownerID)
						if err != nil {
							return err
						}
						bulk, err := r.BoardItem(ctx, "prj_example", id)
						if err != nil {
							return err
						}
						owner.WorkItemID, owner.Position.Identifier, owner.Display.Number = bulk.WorkItemID, bulk.Position.Identifier, bulk.Display.Number
						if !reflect.DeepEqual(owner, bulk) {
							return errors.New("bulk board differs from owner")
						}
						item, err := r.WorkItem(ctx, "prj_example", id)
						if err != nil {
							return err
						}
						expected, err := r.WorkItem(ctx, "prj_example", "wi_0002")
						if err != nil {
							return err
						}
						expected.ID, expected.NativeID, expected.Number = item.ID, item.NativeID, item.Number
						if !reflect.DeepEqual(expected, item) {
							return errors.New("bulk issue differs from owner")
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%04d", index))
			probe := &boardQueryTracer{}
			counted, err := postgres.NewRepository(ctx, tracedFeaturePool(t, pool, probe), smallPool())
			if err != nil {
				t.Fatal(err)
			}
			err = counted.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				probe.queries = 0
				rows, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_example", WorkOrder: true, Limit: 1})
				if err != nil {
					return err
				}
				if len(rows) != 1 || rows[0].WorkItemID != id || rows[0].OrganizationID != org {
					t.Errorf("work page=%+v, want %s/%s", rows, org, id)
				}
				if probe.queries > 2 {
					t.Errorf("board size %d: %d queries, want <=2", index, probe.queries)
				}
				t.Logf("%s size=%d queries=%d", org, index, probe.queries)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			err = counted.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				probe.queries = 0
				started := time.Now()
				deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				lanes, err := r.BoardLanes(deadline, viewstore.BoardSelection{ProjectID: "prj_example", Archived: new(false), ExcludeWorkspace: true}, historyAt())
				if err != nil {
					return err
				}
				if len(lanes) != 1 || lanes[0].Total != int64((index+1)/2) || lanes[0].Running != 0 {
					t.Fatalf("workspace-excluded totals: %+v", lanes)
				}
				if probe.queries > 2 {
					t.Errorf("lane queries=%d, want <=2", probe.queries)
				}
				t.Logf("lane tenant=%s size=%d queries=%d elapsed=%s", org, index, probe.queries, time.Since(started))
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(probe.lanes, "FROM native_board_counts c") {
				t.Error("default lane totals scan board items instead of the typed background counts")
			}
			if index == 1 {
				if _, err := admin.ExecContext(ctx, "UPDATE native_board_items SET issue_number=NULL WHERE organization_id=$1", org); err != nil {
					t.Fatal(err)
				}
				if err := counted.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_example", WorkOrder: true, Limit: 1})
					if err == nil && (len(rows) != 1 || rows[0].WorkItemID != id || rows[0].OrganizationID != org) {
						t.Errorf("warmup lost tenant card: %+v", rows)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(probe.page, "JOIN issues") {
					t.Error("unrefreshed projection lost source ordering fallback")
				}
				if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
					row, err := w.BoardItem(ctx, "prj_example", id)
					if err != nil {
						return err
					}
					return w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: row.ProjectID, WorkItemID: id, ExpectedRevision: row.SourceRevision, Current: &row})
				}); err != nil {
					t.Fatal(err)
				}
				if err := counted.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_example", WorkOrder: true, Limit: 1})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := counted.Close(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(probe.page, "JOIN issues") {
				t.Errorf("work page still joins live issues to sort the entire board")
			}
			if index == 512 {
				if _, err := admin.ExecContext(ctx, "ANALYZE native_board_items, issues"); err != nil {
					t.Fatal(err)
				}
				rows, err := admin.QueryContext(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+probe.page, probe.args...)
				if err != nil {
					t.Fatal(err)
				}
				var plan strings.Builder
				for rows.Next() {
					var line string
					if err := rows.Scan(&line); err != nil {
						t.Fatal(err)
					}
					fmt.Fprintln(&plan, line)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				if err := rows.Close(); err != nil {
					t.Fatal(err)
				}
				t.Log(plan.String())
				var raw []byte
				if err := admin.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+probe.lanes, probe.laneArgs...).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var plans []struct {
					Plan          boardLanePlan
					ExecutionTime float64 `json:"Execution Time"`
				}
				if err := json.Unmarshal(raw, &plans); err != nil {
					t.Fatal(err)
				}
				var inspect func(boardLanePlan)
				inspect = func(node boardLanePlan) {
					if node.Relation == "native_board_items" && (node.Rows+node.Removed)*node.Loops > 16 {
						t.Errorf("lane query scanned board population: %+v", node)
					}
					for _, child := range node.Plans {
						inspect(child)
					}
				}
				inspect(plans[0].Plan)
				t.Logf("lane plan tenant=%s size=%d execution_ms=%f", org, index, plans[0].ExecutionTime)
				if !strings.Contains(plan.String(), "native_board_items_work_order") || strings.Contains(plan.String(), "Sort Key: b.running") {
					t.Error("work page does not use bounded projection order")
				}
			}
		}
		if _, err := admin.ExecContext(ctx, "UPDATE native_board_items SET workspace_counted=false WHERE organization_id=$1", org); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, "UPDATE native_board_counts SET workspace_total=0 WHERE organization_id=$1", org); err != nil {
			t.Fatal(err)
		}
		checkTotals := func(want int64) {
			t.Helper()
			if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				lanes, err := r.BoardLanes(ctx, viewstore.BoardSelection{ProjectID: "prj_example", ExcludeWorkspace: true}, historyAt())
				var total int64
				for _, lane := range lanes {
					total += lane.Total
				}
				if err == nil && total != want {
					t.Errorf("tenant=%s total=%d want=%d", org, total, want)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		checkTotals(256)
		for _, last := range []int{1, 512} {
			first := 1
			if last == 512 {
				first = 2
			}
			if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				for index := first; index <= last; index++ {
					row, err := w.BoardItem(ctx, "prj_example", tracker.NativeWorkItemID(fmt.Sprintf("wi_%04d", index)))
					if err != nil {
						return err
					}
					if err := w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, ExpectedRevision: row.SourceRevision, Current: &row}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if last == 1 {
				checkTotals(256)
			}
		}
		checkTotals(256)
		rollback := errors.New("rollback count update")
		for _, change := range []struct {
			index  int
			action string
			want   int64
		}{{2, "remove label", 257}, {1, "move archive lane", 256}, {4, "delete", 256}, {6, "rollback", 256}, {6, "remove label", 257}, {8, "repeat", 257}} {
			err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				row, err := w.BoardItem(ctx, "prj_example", tracker.NativeWorkItemID(fmt.Sprintf("wi_%04d", change.index)))
				if err != nil {
					return err
				}
				current := &row
				switch change.action {
				case "remove label", "rollback":
					row.Labels = nil
				case "move archive lane":
					row.Labels = []string{"detent:workspace"}
					row.Archived = true
					row.Position.State = "Done"
				case "delete":
					current = nil
				}
				if err := w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, ExpectedRevision: row.SourceRevision, Current: current}); err != nil {
					return err
				}
				if change.action == "rollback" {
					return rollback
				}
				return nil
			})
			if err != nil && (change.action != "rollback" || !errors.Is(err, rollback)) {
				t.Fatal(err)
			}
			checkTotals(change.want)
		}
	}
}

type boardLanePlan struct {
	Index    string  `json:"Index Name"`
	Relation string  `json:"Relation Name"`
	Rows     float64 `json:"Actual Rows"`
	Removed  float64 `json:"Rows Removed by Filter"`
	Loops    float64 `json:"Actual Loops"`
	Plans    []boardLanePlan
}

func withPlanSeedTransaction(ctx context.Context, admin *sql.DB, write func(pgx.Tx) error) (resultErr error) {
	connection, err := admin.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, connection.Close()) }()
	return connection.Raw(func(driverConnection any) (resultErr error) {
		native, ok := driverConnection.(*stdlib.Conn)
		if !ok {
			return errors.New("native PostgreSQL fixture connection is required")
		}
		tx, err := native.Conn().Begin(ctx)
		if err != nil {
			return err
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := tx.Rollback(cleanup); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		if err := write(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	})
}

func planSeedColumns(ctx context.Context, tx pgx.Tx, table string, replace map[string]string) (string, string, error) {
	if table != "issues" && table != "native_board_items" && table != "native_work_item_pages" && table != "search_postings" && table != "search_documents" {
		return "", "", errors.New("unsupported projection fixture table")
	}
	rows, err := tx.Query(ctx, `SELECT attname FROM pg_attribute WHERE attrelid=$1::regclass AND attnum>0 AND NOT attisdropped AND attgenerated='' ORDER BY attnum`, "public."+table)
	if err != nil {
		return "", "", err
	}
	defer rows.Close()
	var columns, values []string
	found := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return "", "", err
		}
		quoted := pgx.Identifier{name}.Sanitize()
		value := "seed." + quoted
		if replacement, ok := replace[name]; ok {
			value = replacement
			found[name] = true
		}
		columns = append(columns, quoted)
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	for name := range replace {
		if !found[name] {
			return "", "", fmt.Errorf("fixture column %s missing", name)
		}
	}
	return strings.Join(columns, ","), strings.Join(values, ","), nil
}

func bulkBoardWorkOrderSeed(ctx context.Context, admin *sql.DB, organization string, first, last int) error {
	return withPlanSeedTransaction(ctx, admin, func(tx pgx.Tx) error {
		id := "'wi_'||lpad(n::text,4,'0')"
		columns, values, err := planSeedColumns(ctx, tx, "issues", map[string]string{"id": "public.allocate_core_record_id('public.issues_id_seq'::regclass,0)", "native_id": id, "number": "n"})
		if err != nil {
			return err
		}
		result, err := tx.Exec(ctx, "INSERT INTO issues("+columns+") SELECT "+values+" FROM issues seed CROSS JOIN generate_series($2::int,$3::int)n WHERE seed.organization_id=$1 AND seed.project_id='prj_example' AND seed.native_id='wi_0002'", organization, first, last)
		if err != nil {
			return err
		}
		if result.RowsAffected() != int64(last-first+1) {
			return errors.New("bulk work item cardinality changed")
		}
		columns, values, err = planSeedColumns(ctx, tx, "native_board_items", map[string]string{"work_item_id": id, "identifier": "'prj_example#'||n", "issue_number": "n"})
		if err != nil {
			return err
		}
		result, err = tx.Exec(ctx, "INSERT INTO native_board_items("+columns+") SELECT "+values+" FROM native_board_items seed CROSS JOIN generate_series($2::int,$3::int)n WHERE seed.organization_id=$1 AND seed.project_id='prj_example' AND seed.work_item_id=CASE WHEN n%2=0 THEN 'wi_0002' ELSE 'wi_0001' END", organization, first, last)
		if err != nil {
			return err
		}
		if result.RowsAffected() != int64(last-first+1) {
			return errors.New("bulk board cardinality changed")
		}
		statements := []string{
			`INSERT INTO native_board_filter_values(organization_id,project_id,work_item_id,kind,value) SELECT seed.organization_id,seed.project_id,'wi_'||lpad(n::text,4,'0'),seed.kind,seed.value FROM native_board_filter_values seed CROSS JOIN generate_series($2::int,$3::int)n WHERE seed.organization_id=$1 AND seed.project_id='prj_example' AND seed.work_item_id='wi_0002' AND n%2=0`,
			`UPDATE native_board_counts SET total=total+($3::int-$2::int+1),workspace_total=workspace_total+($3::int/2-($2::int-1)/2) WHERE organization_id=$1 AND project_id='prj_example' AND NOT archived AND state='Todo'`,
			`INSERT INTO native_board_deltas(organization_id,project_id,sequence,work_item_id,current_object_key,current_sha256,current_bytes,current_content_bytes) SELECT seed.organization_id,seed.project_id,n,'wi_'||lpad(n::text,4,'0'),seed.current_object_key,seed.current_sha256,seed.current_bytes,seed.current_content_bytes FROM native_board_deltas seed CROSS JOIN generate_series($2::int,$3::int)n WHERE seed.organization_id=$1 AND seed.project_id='prj_example' AND seed.sequence=2`,
			`UPDATE native_board_sequences SET sequence=sequence+($3::int-$2::int+1) WHERE organization_id=$1 AND project_id='prj_example'`,
			`UPDATE projects SET duplicate_source_sequence=duplicate_source_sequence+($3::int-$2::int+1) WHERE organization_id=$1 AND id='prj_example'`,
			`DELETE FROM native_board_dirty WHERE organization_id=$1 AND project_id='prj_example' AND work_item_id IN (SELECT 'wi_'||lpad(n::text,4,'0') FROM generate_series($2::int,$3::int)n)`,
		}
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement, organization, first, last); err != nil {
				return err
			}
		}
		var issues, boards, filters, deltas, dirty, identities, total, workspace, sequence, floor, invalid int64
		err = tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM native_board_items WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM native_board_filter_values WHERE organization_id=$1 AND project_id='prj_example' AND kind='label' AND value='detent:workspace'),
 (SELECT count(*) FROM native_board_deltas WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM native_board_dirty WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM work_item_identities WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT total FROM native_board_counts WHERE organization_id=$1 AND project_id='prj_example' AND state='Todo' AND NOT archived),
 (SELECT workspace_total FROM native_board_counts WHERE organization_id=$1 AND project_id='prj_example' AND state='Todo' AND NOT archived),
 (SELECT sequence FROM native_board_sequences WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT replay_floor FROM native_board_sequences WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM native_board_deltas d JOIN native_board_items b USING(organization_id,project_id,work_item_id) WHERE d.organization_id=$1 AND d.project_id='prj_example' AND (d.work_item_id<>'wi_'||lpad(d.sequence::text,4,'0') OR (d.current_object_key,d.current_sha256,d.current_bytes,d.current_content_bytes) IS DISTINCT FROM (b.payload_object_key,b.payload_sha256,b.payload_bytes,b.payload_content_bytes)))`, organization).Scan(&issues, &boards, &filters, &deltas, &dirty, &identities, &total, &workspace, &sequence, &floor, &invalid)
		if err != nil {
			return err
		}
		if issues != int64(last) || boards != int64(last) || filters != int64(last/2) || deltas != int64(last) || dirty != 0 || identities != int64(last) || total != int64(last) || workspace != int64(last/2) || sequence != int64(last) || floor != 0 || invalid != 0 {
			return fmt.Errorf("bulk board effects differ: issues=%d boards=%d filters=%d deltas=%d dirty=%d identities=%d total=%d workspace=%d sequence=%d floor=%d invalid=%d", issues, boards, filters, deltas, dirty, identities, total, workspace, sequence, floor, invalid)
		}
		return nil
	})
}
