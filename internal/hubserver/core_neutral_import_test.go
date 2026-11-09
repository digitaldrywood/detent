package hubserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func coreNeutralFixtureTable(t *testing.T, catalog storagecatalog.Catalog, name string, keys []postgres.NeutralImportKey) postgres.NeutralImportTable {
	t.Helper()
	table := postgres.NeutralImportTable{Name: name, Key: keys, Columns: map[string]string{}}
	for name, column := range catalog.Tables["hub/"+name].Columns {
		table.Columns[name] = column.Source
	}
	if len(table.Columns) == 0 {
		t.Fatal("missing source table")
	}
	return table
}
func coreNeutralFixtureRow(t *testing.T, table postgres.NeutralImportTable, values map[string]any) postgres.NeutralImportRow {
	t.Helper()
	row := postgres.NeutralImportRow{Values: map[string]storagecatalog.Cell{}}
	for name, signature := range table.Columns {
		var metadata []json.RawMessage
		if err := json.Unmarshal([]byte(signature), &metadata); err != nil || len(metadata) != 7 {
			t.Fatalf("source signature: %v", err)
		}
		var kind string
		var required int
		var defaultValue *string
		if err := json.Unmarshal(metadata[2], &kind); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(metadata[3], &required); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(metadata[4], &defaultValue); err != nil {
			t.Fatal(err)
		}
		cell := storagecatalog.Cell{Encoding: "utf8"}
		if defaultValue != nil {
			cell.Data = []byte(strings.Trim(*defaultValue, "'"))
		} else if required == 0 {
			cell = storagecatalog.Cell{Null: true}
		} else if strings.Contains(kind, "INT") || strings.Contains(kind, "REAL") {
			cell.Data = []byte("0")
		}
		row.Values[name] = cell
	}
	for name, value := range values {
		if _, ok := table.Columns[name]; !ok {
			t.Fatalf("unknown source column %s", name)
		}
		if value == nil {
			row.Values[name] = storagecatalog.Cell{Null: true}
			continue
		}
		if data, ok := value.([]byte); ok {
			row.Values[name] = storagecatalog.Cell{Encoding: "binary", Data: bytes.Clone(data)}
			continue
		}
		text := fmt.Sprint(value)
		if flag, ok := value.(bool); ok {
			text = "0"
			if flag {
				text = "1"
			}
		}
		row.Values[name] = storagecatalog.Cell{Encoding: "utf8", Data: []byte(text)}
	}
	for _, key := range table.Key {
		row.Key = append(row.Key, string(row.Values[key.Column].Data))
	}
	if len(table.Key) == 1 && table.Key[0].Column == "id" && table.Key[0].Kind == "integer" {
		id, err := strconv.ParseInt(row.Key[0], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		row.SourceRowID = id
	}
	return row
}
func writeCoreNeutralFixture(t *testing.T, organization tracker.OrganizationID, tables []postgres.NeutralImportTable, records map[string][]postgres.NeutralImportRow, pageSizes ...int) (string, postgres.NeutralImportManifest) {
	t.Helper()
	pageSize := 100
	if len(pageSizes) > 0 {
		pageSize = pageSizes[0]
	}
	if pageSize < 1 || pageSize > 100 {
		t.Fatal("invalid neutral fixture page size")
	}
	sourceBytes, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	manifest := postgres.NeutralImportManifest{Version: 1, Identity: postgres.NeutralImportIdentity{SourceID: "example-source", SourceSHA256: artifact.Digest(sourceBytes), OrganizationID: organization, SchemaVersion: 1}}
	for _, table := range tables {
		rows := slices.Clone(records[table.Name])
		slices.SortFunc(rows, func(left, right postgres.NeutralImportRow) int {
			for i, key := range table.Key {
				if key.Kind == "integer" {
					a, err := strconv.ParseInt(left.Key[i], 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					b, err := strconv.ParseInt(right.Key[i], 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					if a < b {
						return -1
					}
					if a > b {
						return 1
					}
				} else {
					if left.Key[i] < right.Key[i] {
						return -1
					}
					if left.Key[i] > right.Key[i] {
						return 1
					}
				}
			}
			return 0
		})
		table.Pages = nil
		for offset := 0; offset < len(rows); offset += pageSize {
			page := rows[offset:min(offset+pageSize, len(rows))]
			raw, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			file := fmt.Sprintf("%s_%d.json", table.Name, offset/pageSize)
			if err := os.WriteFile(filepath.Join(directory, file), raw, 0400); err != nil {
				t.Fatal(err)
			}
			table.Pages = append(table.Pages, postgres.NeutralImportPage{File: file, SHA256: artifact.Digest(raw), Rows: len(page), First: page[0].Key, Last: page[len(page)-1].Key})
		}
		manifest.Tables = append(manifest.Tables, table)
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), raw, 0400); err != nil {
		t.Fatal(err)
	}
	return directory, manifest
}

type coreNeutralFixture struct {
	t            *testing.T
	organization tracker.OrganizationID
	tables       map[string]postgres.NeutralImportTable
	records      map[string][]postgres.NeutralImportRow
}

func newCoreNeutralFixture(t *testing.T, organization tracker.OrganizationID, names ...string) *coreNeutralFixture {
	t.Helper()
	fixture := &coreNeutralFixture{t: t, organization: organization, tables: make(map[string]postgres.NeutralImportTable), records: make(map[string][]postgres.NeutralImportRow)}
	catalog, err := storagecatalog.Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		table := coreNeutralFixtureTable(t, catalog, name, nil)
		type sourceKey struct {
			position int
			key      postgres.NeutralImportKey
		}
		var keys []sourceKey
		for column, signature := range table.Columns {
			var fields []json.RawMessage
			if err := json.Unmarshal([]byte(signature), &fields); err != nil {
				t.Fatal(err)
			}
			var position int
			if err := json.Unmarshal(fields[5], &position); err != nil {
				t.Fatal(err)
			}
			if position == 0 {
				continue
			}
			var kind string
			if err := json.Unmarshal(fields[2], &kind); err != nil {
				t.Fatal(err)
			}
			keyKind := "text"
			if strings.Contains(strings.ToUpper(kind), "INT") {
				keyKind = "integer"
			}
			keys = append(keys, sourceKey{position, postgres.NeutralImportKey{Column: column, Kind: keyKind}})
		}
		slices.SortFunc(keys, func(a, b sourceKey) int { return a.position - b.position })
		for _, key := range keys {
			table.Key = append(table.Key, key.key)
		}
		if name == "leases" {
			table.Key = []postgres.NeutralImportKey{{Column: "lease_id", Kind: "text"}}
		}
		if name == "change_evidence" {
			table.Key = []postgres.NeutralImportKey{{Column: "change_id", Kind: "text"}, {Column: "sequence", Kind: "integer"}}
		}
		if len(table.Key) == 0 {
			t.Fatalf("missing fixture key for %s", name)
		}
		fixture.tables[name] = table
	}
	return fixture
}
func (f *coreNeutralFixture) add(table string, values map[string]any) {
	f.t.Helper()
	definition, ok := f.tables[table]
	if !ok {
		f.t.Fatalf("missing fixture table %s", table)
	}
	if _, scoped := definition.Columns["organization_id"]; scoped {
		if _, set := values["organization_id"]; !set {
			values["organization_id"] = f.organization
		}
	}
	row := coreNeutralFixtureRow(f.t, definition, values)
	if row.SourceRowID == 0 {
		row.SourceRowID = int64(len(f.records[table]) + 1)
	}
	f.records[table] = append(f.records[table], row)
}
func (f *coreNeutralFixture) write() string {
	f.t.Helper()
	names := make([]string, 0, len(f.tables))
	for name := range f.tables {
		names = append(names, name)
	}
	slices.Sort(names)
	tables := make([]postgres.NeutralImportTable, 0, len(names))
	for _, name := range names {
		tables = append(tables, f.tables[name])
	}
	path, _ := writeCoreNeutralFixture(f.t, f.organization, tables, f.records, 1)
	return path
}

func (f *coreNeutralFixture) clone(t *testing.T) *coreNeutralFixture {
	t.Helper()
	copy := &coreNeutralFixture{t: t, organization: f.organization, tables: make(map[string]postgres.NeutralImportTable), records: make(map[string][]postgres.NeutralImportRow)}
	raw, err := json.Marshal(f.tables)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &copy.tables); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(f.records)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &copy.records); err != nil {
		t.Fatal(err)
	}
	return copy
}
func (f *coreNeutralFixture) set(table string, filters map[string]string, updates map[string]any) {
	f.t.Helper()
	matched := 0
	for index, row := range f.records[table] {
		matches := true
		for name, want := range filters {
			cell, ok := row.Values[name]
			if !ok || cell.Null || string(cell.Data) != want {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		matched++
		values := make(map[string]any, len(row.Values))
		for name, cell := range row.Values {
			if cell.Null {
				values[name] = nil
			} else if cell.Encoding == "binary" {
				values[name] = cell.Data
			} else {
				values[name] = string(cell.Data)
			}
		}
		for name, value := range updates {
			values[name] = value
		}
		updated := coreNeutralFixtureRow(f.t, f.tables[table], values)
		updated.SourceRowID = row.SourceRowID
		updated.SourcePosition = row.SourcePosition
		f.records[table][index] = updated
	}
	if matched == 0 {
		f.t.Fatalf("no source rows matched %s update", table)
	}
}

func coreImportPostgres(t *testing.T) *postgres.Repository {
	t.Helper()
	repository, _ := coreImportPostgresWithAdmin(t, 3)
	return repository
}
func coreImportPostgresWithAdmin(t *testing.T, maxConnections int, configure ...func(*pgxpool.Config)) (*postgres.Repository, *sql.DB) {
	t.Helper()
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pgx.Identifier{connection.Database}.Sanitize()+" CONNECTION LIMIT "+strconv.Itoa(maxConnections+1)); err != nil {
		t.Fatal(err)
	}
	native := postgresStartupTemplate.NativeConfig(t, admin, connection.User, connection.Password)
	native.MaxConns = int32(maxConnections)
	for _, option := range configure {
		option(native)
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, postgres.PoolConfig{MaxConnections: maxConnections, ServingConnections: maxConnections - 1, BackgroundConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository, admin
}
