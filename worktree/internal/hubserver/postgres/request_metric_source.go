package postgres

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

type metricImportRow struct {
	at, route, kind, caller, status string
	metric                          metricsstore.Metric
}

func metricSourceKeys(stage int64) []string {
	switch stage {
	case 0:
		return []string{"day", "route", "caller_kind", "status_class"}
	case 1:
		return []string{"minute", "route", "caller_kind", "caller_id", "status_class"}
	default:
		return []string{"hour", "route", "status_class"}
	}
}

func (i *MetricsImporter) sourceTable(name string) string {
	return pgx.Identifier{i.source.Source.Schema}.Sanitize() + "." + pgx.Identifier{name}.Sanitize()
}

func (i *MetricsImporter) validateSource(ctx context.Context) error {
	for _, table := range []string{"tenant", "request_days", "request_minutes", "request_hours"} {
		if err := i.validateSourceTable(ctx, table); err != nil {
			return err
		}
	}
	statement := strings.Join([]string{"SELECT organization FROM", i.sourceTable("tenant"), "LIMIT 2"}, " ")
	rows, err := i.source.DB.QueryContext(ctx, statement)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return storagecatalog.ErrOrganization
	}
	var organization string
	if err := rows.Scan(&organization); err != nil {
		return storagecatalog.ErrOrganization
	}
	if organization != string(i.organization) || rows.Next() {
		return storagecatalog.ErrOrganization
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for stage, grain := range metricImportGrains {
		var indexed bool
		err := i.source.DB.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am am ON am.oid=c.relam
WHERE i.indrelid=$1::regclass AND i.indisvalid AND i.indisprimary AND i.indpred IS NULL AND i.indexprs IS NULL
AND am.amname='btree' AND i.indnkeyatts=cardinality($2::text[])
AND NOT EXISTS (SELECT 1 FROM unnest(i.indoption) option WHERE option<>0)
AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(attnum,n)
JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum WHERE k.n<=i.indnkeyatts ORDER BY k.n)=$2::text[]
AND NOT EXISTS (SELECT 1 FROM unnest(i.indcollation) WITH ORDINALITY k(coll,n)
WHERE k.n<=i.indnkeyatts AND k.coll<>'"C"'::regcollation))`, i.sourceTable("request_"+string(grain)+"s"), metricSourceKeys(int64(stage))).Scan(&indexed)
		if err != nil {
			return err
		}
		if !indexed {
			return ErrMetricImport
		}
	}
	return nil
}

func (i *MetricsImporter) validateSourceTable(ctx context.Context, table string) error {
	statement := strings.Join([]string{"SELECT * FROM", i.sourceTable(table), "LIMIT 0"}, " ")
	rows, err := i.source.DB.QueryContext(ctx, statement)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	expected := i.source.Schema[table]
	if len(columns) != len(expected) {
		return storagecatalog.ErrUnmapped
	}
	for _, column := range columns {
		if _, ok := expected[column.Name()]; !ok || column.DatabaseTypeName() != "TEXT" && column.DatabaseTypeName() != "BYTEA" {
			return storagecatalog.ErrUnmapped
		}
	}
	return rows.Close()
}

func (i *MetricsImporter) readPage(ctx context.Context, checkpoint pgquery.RequestMetricImport, limit int) ([]metricImportRow, error) {
	table := "request_" + string(metricImportGrains[checkpoint.Stage]) + "s"
	keys := metricSourceKeys(checkpoint.Stage)
	after := map[string]string{keys[0]: checkpoint.AfterTime, "route": checkpoint.AfterRoute, "caller_kind": checkpoint.AfterKind, "caller_id": checkpoint.AfterCaller, "status_class": checkpoint.AfterStatus}
	columns := make([]string, 0, len(i.source.Schema[table]))
	for column := range i.source.Schema[table] {
		columns = append(columns, column)
	}
	slices.Sort(columns)
	quoted := make([]string, len(columns))
	for n, column := range columns {
		quoted[n] = pgx.Identifier{column}.Sanitize()
	}
	order, placeholders := make([]string, len(keys)), make([]string, len(keys))
	args := make([]any, 0, len(keys)+1)
	for n, key := range keys {
		order[n] = pgx.Identifier{key}.Sanitize() + ` COLLATE "C"`
		placeholders[n] = fmt.Sprintf("$%d::text", n+1)
		args = append(args, after[key])
	}
	predicate := ""
	if checkpoint.AfterTime != "" {
		predicate = " WHERE (" + strings.Join(order, ",") + ") > (" + strings.Join(placeholders, ",") + ")"
	} else {
		args = nil
	}
	args = append(args, limit)
	query := strings.Join([]string{"SELECT", strings.Join(quoted, ","), "FROM", i.sourceTable(table), predicate, "ORDER BY", strings.Join(order, ","), "LIMIT", fmt.Sprintf("$%d", len(args))}, " ")
	rows, err := i.source.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []metricImportRow
	for rows.Next() {
		data := make([][]byte, len(columns))
		destinations := make([]any, len(columns))
		for n := range columns {
			destinations[n] = &data[n]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, err
		}
		raw := make(map[string]storagecatalog.Cell, len(columns))
		for n, name := range columns {
			cell := storagecatalog.Cell{Encoding: "utf8", Data: data[n]}
			if data[n] == nil {
				cell = storagecatalog.Cell{Null: true}
			}
			raw[name] = cell
		}
		values := make(map[string]storagecatalog.Value, len(columns))
		for _, name := range columns {
			if i.source.Catalog.Tables["metrics/"+table].Columns[name].Disposition == "rebuild" {
				continue
			}
			value, err := i.source.Catalog.Decode("metrics/"+table, name, raw[name])
			if err != nil {
				return nil, fmt.Errorf("decode %s source column: %w", name, err)
			}
			values[name] = value
		}
		if _, err := i.binding.ValidateRow(table, raw); err != nil {
			return nil, fmt.Errorf("validate %s source row: %w", table, err)
		}
		m := metricsstore.Metric{At: values[keys[0]].Time, Route: values["route"].Text, CallerKind: values["caller_kind"].Text, CallerID: values["caller_id"].Text, StatusClass: values["status_class"].Integer,
			Latency: metricsstore.Latency{Count: values["count"].Integer, Sum: values["sum_ms"].Real, Max: values["max_ms"].Real}}
		grain := metricImportGrains[checkpoint.Stage]
		unit := map[metricsstore.Grain]time.Duration{metricsstore.Day: 24 * time.Hour, metricsstore.Hour: time.Hour, metricsstore.Minute: time.Minute}[grain]
		if m.At.IsZero() || !m.At.Equal(m.At.Truncate(unit)) || m.Route == "" {
			return nil, fmt.Errorf("validate metric key: %w", storagecatalog.ErrValue)
		}
		if grain != metricsstore.Day {
			copy(m.Histogram[:], values["histogram"].Histogram)
			if !m.Valid() {
				return nil, fmt.Errorf("validate metric histogram count: %w", storagecatalog.ErrValue)
			}
		}
		page = append(page, metricImportRow{at: string(raw[keys[0]].Data), route: string(raw["route"].Data), kind: string(raw["caller_kind"].Data), caller: string(raw["caller_id"].Data), status: string(raw["status_class"].Data), metric: m})
	}
	return page, rows.Err()
}
