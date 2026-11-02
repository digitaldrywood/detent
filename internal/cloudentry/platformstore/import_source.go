package platformstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

type importRow struct {
	name   string
	keys   [3]string
	values map[string]any
	text   map[string]string
	writes []importWrite
}

type importWrite struct {
	table  string
	values map[string]any
}

func importTarget(name string) string {
	if name == "auth/platform_notifications" {
		return "auth_notifications"
	}
	_, table, _ := strings.Cut(name, "/")
	return table
}

func importTargetTables() []string {
	names := make([]string, len(importTables))
	for n, name := range importTables {
		names[n] = importTarget(name)
	}
	return names
}

func (i *Importer) sourceTable(name string) string {
	store, table, _ := strings.Cut(name, "/")
	if i.source.RegistryDB != nil {
		return pq.QuoteIdentifier(table)
	}
	schema := i.source.Registry.Schema
	if store == "auth" {
		schema = i.source.Auth.Schema
	}
	return pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(table)
}

func (i *Importer) sourceDB(name string) *sql.DB {
	if i.source.RegistryDB != nil {
		store, _, _ := strings.Cut(name, "/")
		if store == "auth" {
			return i.source.AuthDB
		}
		return i.source.RegistryDB
	}
	return i.source.DB
}

func (i *Importer) sourceKeys(name string) ([]string, error) {
	var keys [3]string
	count := 0
	for column, definition := range i.source.Catalog.Tables[name].Columns {
		var fields []json.RawMessage
		var position int
		if json.Unmarshal([]byte(definition.Source), &fields) != nil || len(fields) != 7 || json.Unmarshal(fields[5], &position) != nil || position < 0 || position > len(keys) {
			return nil, storagecatalog.ErrUnmapped
		}
		if position != 0 {
			if keys[position-1] != "" {
				return nil, storagecatalog.ErrUnmapped
			}
			keys[position-1] = column
			count++
		}
	}
	if count == 0 || slices.Contains(keys[:count], "") {
		return nil, storagecatalog.ErrUnmapped
	}
	return keys[:count], nil
}

func (i *Importer) validateSource(ctx context.Context) error {
	for name, table := range i.source.Catalog.Tables {
		if !strings.HasPrefix(name, "registry/") && !strings.HasPrefix(name, "auth/") {
			continue
		}
		if table.Disposition == "source-only" && (strings.HasSuffix(name, "/sqlite_schema") || strings.HasSuffix(name, "/sqlite_sequence") || strings.HasSuffix(name, "/entry_schema_version")) {
			continue
		}
		if table.Disposition != "target" || table.Family != "platform" || !slices.Contains(importTables[:], name) {
			return storagecatalog.ErrUnmapped
		}
		if err := i.validateSourceTable(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

func (i *Importer) validateSourceTable(ctx context.Context, name string) error {
	if i.source.RegistryDB != nil {
		_, tableName, _ := strings.Cut(name, "/")
		return validateSQLiteTable(ctx, i.sourceDB(name), i.source.Catalog.Tables[name], tableName)
	}
	var query strings.Builder
	query.WriteString("SELECT * FROM ")
	query.WriteString(i.sourceTable(name))
	query.WriteString(" LIMIT 0")
	rows, err := i.source.DB.QueryContext(ctx, query.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	columns, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	expected := i.source.Catalog.Tables[name].Columns
	if len(columns) != len(expected) {
		return storagecatalog.ErrUnmapped
	}
	for _, column := range columns {
		if _, ok := expected[column.Name()]; !ok || column.DatabaseTypeName() != "TEXT" {
			return storagecatalog.ErrUnmapped
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	keys, err := i.sourceKeys(name)
	if err != nil {
		return err
	}
	var indexed bool
	err = i.source.DB.QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am am ON am.oid=c.relam
WHERE i.indrelid=$1::regclass AND i.indisvalid AND i.indisprimary AND i.indpred IS NULL AND i.indexprs IS NULL
AND am.amname='btree' AND i.indnkeyatts=cardinality($2::text[])
AND NOT EXISTS (SELECT 1 FROM unnest(i.indoption) option WHERE option<>0)
AND ARRAY(SELECT a.attname::text FROM unnest(i.indkey) WITH ORDINALITY k(attnum,n)
JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.attnum WHERE k.n<=i.indnkeyatts ORDER BY k.n)=$2::text[]
AND NOT EXISTS (SELECT 1 FROM unnest(i.indcollation) WITH ORDINALITY k(coll,n)
WHERE k.n<=i.indnkeyatts AND k.coll<>'"C"'::regcollation))`, i.sourceTable(name), pq.Array(keys)).Scan(&indexed)
	if err != nil {
		return err
	}
	if !indexed {
		return ErrImport
	}
	return nil
}

func (i *Importer) readPage(ctx context.Context, checkpoint importCheckpoint, limit int) ([]importRow, error) {
	name := importTables[checkpoint.stage]
	keys, err := i.sourceKeys(name)
	if err != nil {
		return nil, err
	}
	columns := make([]string, 0, len(i.source.Catalog.Tables[name].Columns))
	for name := range i.source.Catalog.Tables[name].Columns {
		columns = append(columns, name)
	}
	slices.Sort(columns)
	quoted := make([]string, len(columns))
	for n, column := range columns {
		quoted[n] = pq.QuoteIdentifier(column)
	}
	order, placeholders := make([]string, len(keys)), make([]string, len(keys))
	var args []any
	for n, key := range keys {
		collation := ` COLLATE "C"`
		if i.source.RegistryDB != nil {
			collation = ` COLLATE BINARY`
			placeholders[n] = "?"
		} else {
			placeholders[n] = fmt.Sprintf("$%d::text", n+1)
		}
		order[n] = pq.QuoteIdentifier(key) + collation
		if checkpoint.started {
			args = append(args, checkpoint.after[n])
		}
	}
	predicate := ""
	if checkpoint.started {
		predicate = " WHERE (" + strings.Join(order, ",") + ") > (" + strings.Join(placeholders, ",") + ")"
	}
	args = append(args, limit)
	var query strings.Builder
	query.WriteString("SELECT ")
	query.WriteString(strings.Join(quoted, ","))
	query.WriteString(" FROM ")
	query.WriteString(i.sourceTable(name))
	query.WriteString(predicate)
	query.WriteString(" ORDER BY ")
	query.WriteString(strings.Join(order, ","))
	if i.source.RegistryDB != nil {
		query.WriteString(" LIMIT ?")
	} else {
		fmt.Fprintf(&query, " LIMIT $%d", len(args))
	}
	rows, err := i.sourceDB(name).QueryContext(ctx, query.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var page []importRow
	for rows.Next() {
		raw := make([]sql.NullString, len(columns))
		dest := make([]any, len(columns))
		for n := range raw {
			dest[n] = &raw[n]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		cells := make(map[string]storagecatalog.Cell, len(columns))
		size := 0
		for n, column := range columns {
			size += len(raw[n].String)
			if size > 1<<20 {
				return nil, ErrImport
			}
			cell := storagecatalog.Cell{Null: true}
			if raw[n].Valid {
				cell = storagecatalog.Cell{Encoding: "utf8", Data: []byte(raw[n].String)}
			}
			cells[column] = cell
		}
		row, err := i.decodeRow(name, cells)
		if err != nil {
			return nil, fmt.Errorf("decode %s source row: %w", name, err)
		}
		for n, key := range keys {
			if cells[key].Null {
				return nil, ErrImport
			}
			row.keys[n] = string(cells[key].Data)
		}
		page = append(page, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for n := range page {
		if err := i.prepareRow(ctx, &page[n]); err != nil {
			return nil, fmt.Errorf("prepare %s source row: %w", name, err)
		}
	}
	return page, nil
}
