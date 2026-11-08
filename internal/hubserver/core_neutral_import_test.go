package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func coreNeutralFixtureTable(t *testing.T, name string, keys []postgres.NeutralImportKey) postgres.NeutralImportTable {
	t.Helper()
	catalog, err := storagecatalog.Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
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
	for _, name := range names {
		table := coreNeutralFixtureTable(t, name, nil)
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
	f.records[table] = append(f.records[table], coreNeutralFixtureRow(f.t, definition, values))
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
