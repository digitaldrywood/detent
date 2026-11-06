package postgres_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type boardQueryConnector struct {
	driver.Conn
	queries  int
	page     string
	args     []any
	lanes    string
	laneArgs []any
}

func (c *boardQueryConnector) Connect(context.Context) (driver.Conn, error) { return c, nil }
func (c *boardQueryConnector) Driver() driver.Driver                        { return nil }
func (c *boardQueryConnector) Close() error                                 { return nil }
func (c *boardQueryConnector) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
}
func (c *boardQueryConnector) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *boardQueryConnector) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.queries++
	if strings.Contains(query, "GROUP BY b.state") || strings.Contains(query, "FROM native_board_counts c") {
		c.lanes = query
		c.laneArgs = nil
		for _, arg := range args {
			c.laneArgs = append(c.laneArgs, arg.Value)
		}
	}
	if strings.HasPrefix(query, "SELECT b.organization_id") {
		c.page = query
		c.args = nil
		for _, arg := range args {
			c.args = append(c.args, arg.Value)
		}
	}
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

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
		for index := 1; index <= 512; index++ {
			id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%04d", index))
			if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				item, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: "prj_example", NativeID: id, Number: int64(index), Title: "Example", WorkflowStateName: new("Todo")})
				if err != nil {
					return err
				}
				row := viewstore.BoardRecord{OrganizationID: org, ProjectID: item.ProjectID, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("prj_example#%d", index)}, Dispatchable: true, CreatedAt: historyAt(), UpdatedAt: historyAt(), Payload: ref}
				if index%2 == 0 {
					row.Labels = []string{"detent:workspace"}
				}
				return w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: item.ProjectID, WorkItemID: id, ExpectedRevision: item.Revision, Current: &row})
			}); err != nil {
				t.Fatal(err)
			}
			if index != 1 && index != 512 {
				continue
			}
			conn, err := pool.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			probe := &boardQueryConnector{}
			if err := conn.Raw(func(raw any) error { probe.Conn = raw.(driver.Conn); return nil }); err != nil {
				t.Fatal(err)
			}
			counted, err := postgres.NewRepository(ctx, sql.OpenDB(probe), smallPool())
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
			if err := conn.Close(); err != nil {
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
		for index := 1; index <= 512; index++ {
			if err := repository.WriteTenant(ctx, org, corestore.Background, func(w tenantstore.Writer) error {
				row, err := w.BoardItem(ctx, "prj_example", tracker.NativeWorkItemID(fmt.Sprintf("wi_%04d", index)))
				if err != nil {
					return err
				}
				return w.ReplaceBoardItem(ctx, viewstore.BoardUpdate{ProjectID: row.ProjectID, WorkItemID: row.WorkItemID, ExpectedRevision: row.SourceRevision, Current: &row})
			}); err != nil {
				t.Fatal(err)
			}
			if index == 1 {
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
			if err != nil && !(change.action == "rollback" && errors.Is(err, rollback)) {
				t.Fatal(err)
			}
			checkTotals(change.want)
		}
	}
}

type boardLanePlan struct {
	Relation string  `json:"Relation Name"`
	Rows     float64 `json:"Actual Rows"`
	Removed  float64 `json:"Rows Removed by Filter"`
	Loops    float64 `json:"Actual Loops"`
	Plans    []boardLanePlan
}
