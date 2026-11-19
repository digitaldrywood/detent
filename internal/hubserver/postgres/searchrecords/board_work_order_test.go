package searchrecords_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestBoardWorkOrderPreservesProjectionAndLaneTotals(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	objects := pgstoretest.NewHistoryObjects()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
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
					row := viewstore.BoardRecord{OrganizationID: org, ProjectID: item.ProjectID, WorkItemID: id, SourceRevision: item.Revision, Position: viewstore.BoardPosition{State: "Todo", Identifier: fmt.Sprintf("prj_example#%d", number)}, Dispatchable: true, CreatedAt: pgstoretest.HistoryAt(), UpdatedAt: pgstoretest.HistoryAt(), Payload: ref}
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
			err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				rows, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_example", WorkOrder: true, Limit: 1})
				if err != nil {
					return err
				}
				if len(rows) != 1 || rows[0].WorkItemID != id || rows[0].OrganizationID != org {
					t.Errorf("work page=%+v, want %s/%s", rows, org, id)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			err = repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				lanes, err := r.BoardLanes(ctx, viewstore.BoardSelection{ProjectID: "prj_example", Archived: new(false), ExcludeWorkspace: true}, pgstoretest.HistoryAt())
				if err != nil {
					return err
				}
				if len(lanes) != 1 || lanes[0].Total != int64((index+1)/2) || lanes[0].Running != 0 {
					t.Fatalf("workspace-excluded totals: %+v", lanes)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if index == 1 {
				if _, err := admin.ExecContext(ctx, "UPDATE native_board_items SET issue_number=NULL WHERE organization_id=$1", org); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					rows, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_example", WorkOrder: true, Limit: 1})
					if err == nil && (len(rows) != 1 || rows[0].WorkItemID != id || rows[0].OrganizationID != org) {
						t.Errorf("warmup lost tenant card: %+v", rows)
					}
					return err
				}); err != nil {
					t.Fatal(err)
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
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.BoardPage(ctx, viewstore.BoardSelection{ProjectID: "prj_example", WorkOrder: true, Limit: 1})
					return err
				}); err != nil {
					t.Fatal(err)
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
			if err = repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				lanes, err := r.BoardLanes(ctx, viewstore.BoardSelection{ProjectID: "prj_example", ExcludeWorkspace: true}, pgstoretest.HistoryAt())
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
	if table != "issues" && table != "native_board_items" && table != "native_work_item_pages" {
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
			`INSERT INTO native_board_deltas(organization_id,project_id,sequence,work_item_id,current_object_key,current_sha256,current_bytes,current_content_bytes,topic,entity_id,entity_revision,kind,status,related_id)
 SELECT seed.organization_id,seed.project_id,CASE seed.topic WHEN 'work_item' THEN 3*n-2 WHEN 'human_action' THEN 3*n-1 ELSE 3*n END,CASE WHEN seed.topic='human_action' THEN '' ELSE ` + id + ` END,
 seed.current_object_key,seed.current_sha256,seed.current_bytes,seed.current_content_bytes,
 seed.topic,` + id + `,seed.entity_revision,seed.kind,seed.status,CASE WHEN seed.topic IN ('work_item','human_action') THEN ` + id + ` ELSE seed.related_id END
 FROM native_board_deltas seed CROSS JOIN generate_series($2::int,$3::int)n
 WHERE seed.organization_id=$1 AND seed.project_id='prj_example' AND seed.entity_id='wi_0002' AND seed.topic IN ('work_item','human_action','card')`,
			`UPDATE native_board_sequences SET sequence=sequence+3*($3::int-$2::int+1) WHERE organization_id=$1 AND project_id='prj_example'`,
			`UPDATE projects SET duplicate_source_sequence=duplicate_source_sequence+($3::int-$2::int+1) WHERE organization_id=$1 AND id='prj_example'`,
			`DELETE FROM native_board_dirty WHERE organization_id=$1 AND project_id='prj_example' AND work_item_id IN (SELECT 'wi_'||lpad(n::text,4,'0') FROM generate_series($2::int,$3::int)n)`,
		}
		for _, statement := range statements {
			if _, err := tx.Exec(ctx, statement, organization, first, last); err != nil {
				return err
			}
		}
		var issues, boards, filters, deltas, cards, dirty, identities, total, workspace, sequence, floor, invalid int64
		err = tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM issues WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM native_board_items WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM native_board_filter_values WHERE organization_id=$1 AND project_id='prj_example' AND kind='label' AND value='detent:workspace'),
 (SELECT count(*) FROM native_board_deltas WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM native_board_deltas WHERE organization_id=$1 AND project_id='prj_example' AND topic='card'),
 (SELECT count(*) FROM native_board_dirty WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM work_item_identities WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT total FROM native_board_counts WHERE organization_id=$1 AND project_id='prj_example' AND state='Todo' AND NOT archived),
 (SELECT workspace_total FROM native_board_counts WHERE organization_id=$1 AND project_id='prj_example' AND state='Todo' AND NOT archived),
 (SELECT sequence FROM native_board_sequences WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT replay_floor FROM native_board_sequences WHERE organization_id=$1 AND project_id='prj_example'),
 (SELECT count(*) FROM native_board_deltas d LEFT JOIN native_board_items b ON b.organization_id=d.organization_id AND b.project_id=d.project_id AND b.work_item_id=d.entity_id
  WHERE d.organization_id=$1 AND d.project_id='prj_example' AND (
   b.work_item_id IS NULL OR d.entity_id<>'wi_'||lpad(((d.sequence+2)/3)::text,4,'0')
   OR d.work_item_id<>CASE WHEN d.topic='human_action' THEN '' ELSE d.entity_id END OR d.entity_revision IS DISTINCT FROM b.source_revision OR d.kind<>'upsert'
   OR d.previous_object_key IS NOT NULL
   OR CASE WHEN d.sequence%3=0 THEN
    d.topic<>'card' OR d.related_id<>'' OR (d.current_object_key,d.current_sha256,d.current_bytes,d.current_content_bytes)
      IS DISTINCT FROM (b.payload_object_key,b.payload_sha256,b.payload_bytes,b.payload_content_bytes)
   WHEN d.sequence%3=2 THEN d.topic<>'human_action' OR d.related_id<>d.entity_id OR d.status<>'' OR d.current_object_key IS NOT NULL
   ELSE d.topic<>'work_item' OR d.related_id<>d.work_item_id OR d.current_object_key IS NOT NULL END))`, organization).Scan(&issues, &boards, &filters, &deltas, &cards, &dirty, &identities, &total, &workspace, &sequence, &floor, &invalid)
		if err != nil {
			return err
		}
		if issues != int64(last) || boards != int64(last) || filters != int64(last/2) || deltas != 3*int64(last) || cards != int64(last) || dirty != 0 || identities != int64(last) || total != int64(last) || workspace != int64(last/2) || sequence != 3*int64(last) || floor != 0 || invalid != 0 {
			return fmt.Errorf("bulk board effects differ: issues=%d boards=%d filters=%d deltas=%d cards=%d dirty=%d identities=%d total=%d workspace=%d sequence=%d floor=%d invalid=%d", issues, boards, filters, deltas, cards, dirty, identities, total, workspace, sequence, floor, invalid)
		}
		return nil
	})
}
