package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const costImportPageLimit = 100

var costImportColumns = []string{"organization_id", "provider", "provider_account", "source_id", "period_start", "period_end", "revision", "project_id", "resource_id", "bucket", "metric", "received_at", "voided", "observation_json"}

const costImportPageAfter = ` AND ("organization_id" COLLATE BINARY,"provider" COLLATE BINARY,"provider_account" COLLATE BINARY,"source_id" COLLATE BINARY,"period_start" COLLATE BINARY,"period_end" COLLATE BINARY,"revision" COLLATE BINARY) > (?,?,?,?,?,?,?)`
const costImportPageOrder = ` ORDER BY "organization_id" COLLATE BINARY,"provider" COLLATE BINARY,"provider_account" COLLATE BINARY,"source_id" COLLATE BINARY,"period_start" COLLATE BINARY,"period_end" COLLATE BINARY,"revision" COLLATE BINARY LIMIT ?`

type costImportPageRow struct {
	key    [7]string
	record costImportRecord
}

func readCostImportPage(ctx context.Context, db *sql.Tx, organization string, catalog storagecatalog.Catalog, binding *storagecatalog.Binding, after [7]string, hasAfter bool, limit int) ([]costImportPageRow, error) {
	if db == nil || organization == "" || binding == nil || limit < 1 || limit > costImportPageLimit {
		return nil, ErrCostImport
	}
	definition, ok := catalog.Tables["hub/usage_cost_observations"]
	if !ok || len(definition.Columns) != len(costImportColumns) {
		return nil, storagecatalog.ErrUnmapped
	}
	for _, name := range costImportColumns {
		column, ok := definition.Columns[name]
		if !ok {
			return nil, storagecatalog.ErrUnmapped
		}
		if column.Disposition == "pending" {
			return nil, storagecatalog.ErrPending
		}
		if column.Disposition != "column" && column.Disposition != "object" && column.Disposition != "transform" {
			return nil, ErrCostImport
		}
	}
	statement := `SELECT "organization_id","provider","provider_account","source_id","period_start","period_end","revision","project_id","resource_id","bucket","metric","received_at","voided","observation_json" FROM "usage_cost_observations"`
	args := []any{organization}
	statement += " WHERE organization_id = ?"
	if hasAfter {
		statement += costImportPageAfter
		for n, value := range after {
			if n == len(after)-1 {
				revision, err := strconv.ParseInt(value, 10, 64)
				if err != nil {
					return nil, ErrCostImport
				}
				args = append(args, revision)
			} else {
				args = append(args, value)
			}
		}
	}
	statement += costImportPageOrder
	args = append(args, limit)
	rows, err := db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("read cost import page: %w", err)
	}
	defer rows.Close()
	var page []costImportPageRow
	for rows.Next() {
		var cells [14]sql.NullString
		values := make([]any, len(cells))
		for n := range cells {
			values[n] = &cells[n]
		}
		if err := rows.Scan(values...); err != nil {
			return nil, err
		}
		raw := make(map[string]storagecatalog.Cell, len(cells))
		for n, value := range cells {
			if !value.Valid && n != 12 {
				return nil, ErrCostImport
			}
			if !value.Valid {
				raw[costImportColumns[n]] = storagecatalog.Cell{Null: true}
			} else {
				raw[costImportColumns[n]] = storagecatalog.Cell{Encoding: "utf8", Data: []byte(value.String)}
			}
		}
		organization, err := binding.ValidateMappedRow("usage_cost_observations", raw)
		if err != nil {
			return nil, err
		}
		var row costImportPageRow
		for n := range row.key {
			row.key[n] = cells[n].String
		}
		r := &row.record
		r.OrganizationID, r.ProjectID = tracker.OrganizationID(organization), tracker.ProjectID(cells[7].String)
		r.Provider, r.ProviderAccount, r.SourceID = cells[1].String, cells[2].String, cells[3].String
		r.ResourceID, r.Bucket, r.Metric = cells[8].String, cells[9].String, cells[10].String
		r.Revision, err = strconv.ParseInt(cells[6].String, 10, 64)
		if err != nil {
			return nil, ErrCostImport
		}
		for n, target := range map[int]*time.Time{4: &r.From, 5: &r.To, 11: &r.ReceivedAt} {
			*target, err = time.Parse(time.RFC3339Nano, cells[n].String)
			if err != nil {
				return nil, ErrCostImport
			}
		}
		if cells[12].Valid {
			switch cells[12].String {
			case "0":
				r.Voided = new(false)
			case "1":
				r.Voided = new(true)
			default:
				return nil, ErrCostImport
			}
		}
		r.Payload = []byte(cells[13].String)
		if _, err := decodeCostImportRecord(tracker.OrganizationID(organization), *r); err != nil {
			return nil, err
		}
		page = append(page, row)
	}
	return page, rows.Err()
}
