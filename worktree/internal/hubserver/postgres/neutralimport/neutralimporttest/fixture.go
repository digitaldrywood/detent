package neutralimporttest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func Table(t *testing.T, catalog storagecatalog.Catalog, name string, keys []neutralimport.Key) neutralimport.Table {
	t.Helper()
	table := neutralimport.Table{Name: name, Key: keys, Columns: map[string]string{}}
	for name, column := range catalog.Tables["hub/"+name].Columns {
		table.Columns[name] = column.Source
	}
	if len(table.Columns) == 0 {
		t.Fatal("missing source table")
	}
	return table
}
func Row(t *testing.T, table neutralimport.Table, values map[string]any) neutralimport.Row {
	t.Helper()
	row := neutralimport.Row{Values: map[string]storagecatalog.Cell{}}
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
func Write(t *testing.T, source []byte, organization tracker.OrganizationID, tables []neutralimport.Table, records map[string][]neutralimport.Row, pageSizes ...int) (string, neutralimport.Manifest) {
	t.Helper()
	pageSize := 100
	if len(pageSizes) > 0 {
		pageSize = pageSizes[0]
	}
	if pageSize < 1 || pageSize > 100 {
		t.Fatal("invalid neutral fixture page size")
	}
	directory := t.TempDir()
	manifest := neutralimport.Manifest{Version: 1, Identity: neutralimport.Identity{SourceID: "example-source", SourceSHA256: artifact.Digest(source), OrganizationID: organization, SchemaVersion: 1}}
	for _, table := range tables {
		rows := slices.Clone(records[table.Name])
		slices.SortFunc(rows, func(left, right neutralimport.Row) int {
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
			table.Pages = append(table.Pages, neutralimport.Page{File: file, SHA256: artifact.Digest(raw), Rows: len(page), First: page[0].Key, Last: page[len(page)-1].Key})
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

type Fixture struct {
	t            *testing.T
	Organization tracker.OrganizationID
	Tables       map[string]neutralimport.Table
	Records      map[string][]neutralimport.Row
}

func Empty(t *testing.T, organization tracker.OrganizationID) *Fixture {
	return &Fixture{t: t, Organization: organization, Tables: make(map[string]neutralimport.Table), Records: make(map[string][]neutralimport.Row)}
}

func New(t *testing.T, repository fs.FS, organization tracker.OrganizationID, names ...string) *Fixture {
	t.Helper()
	fixture := Empty(t, organization)
	catalog, err := storagecatalog.Read(repository, "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		table := Table(t, catalog, name, nil)
		type sourceKey struct {
			position int
			key      neutralimport.Key
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
			keys = append(keys, sourceKey{position, neutralimport.Key{Column: column, Kind: keyKind}})
		}
		slices.SortFunc(keys, func(a, b sourceKey) int { return a.position - b.position })
		for _, key := range keys {
			table.Key = append(table.Key, key.key)
		}
		if name == "leases" {
			table.Key = []neutralimport.Key{{Column: "lease_id", Kind: "text"}}
		}
		if name == "change_evidence" {
			table.Key = []neutralimport.Key{{Column: "change_id", Kind: "text"}, {Column: "sequence", Kind: "integer"}}
		}
		if len(table.Key) == 0 {
			t.Fatalf("missing fixture key for %s", name)
		}
		fixture.Tables[name] = table
	}
	return fixture
}
func (f *Fixture) Add(table string, values map[string]any) {
	f.t.Helper()
	definition, ok := f.Tables[table]
	if !ok {
		f.t.Fatalf("missing fixture table %s", table)
	}
	if _, scoped := definition.Columns["organization_id"]; scoped {
		if _, set := values["organization_id"]; !set {
			values["organization_id"] = f.Organization
		}
	}
	row := Row(f.t, definition, values)
	if row.SourceRowID == 0 {
		row.SourceRowID = int64(len(f.Records[table]) + 1)
	}
	f.Records[table] = append(f.Records[table], row)
}
func (f *Fixture) Write() string {
	f.t.Helper()
	names := make([]string, 0, len(f.Tables))
	for name := range f.Tables {
		names = append(names, name)
	}
	slices.Sort(names)
	sourceBytes, err := json.Marshal(f.Records)
	if err != nil {
		f.t.Fatal(err)
	}
	tables := make([]neutralimport.Table, 0, len(names))
	for _, name := range names {
		tables = append(tables, f.Tables[name])
	}
	path, _ := Write(f.t, sourceBytes, f.Organization, tables, f.Records, 1)
	return path
}

func (f *Fixture) Clone(t *testing.T) *Fixture {
	t.Helper()
	copy := Empty(t, f.Organization)
	raw, err := json.Marshal(f.Tables)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &copy.Tables); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(f.Records)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &copy.Records); err != nil {
		t.Fatal(err)
	}
	return copy
}
func (f *Fixture) Set(table string, filters map[string]string, updates map[string]any) {
	f.t.Helper()
	matched := 0
	for index, row := range f.Records[table] {
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
		updated := Row(f.t, f.Tables[table], values)
		updated.SourceRowID = row.SourceRowID
		updated.SourcePosition = row.SourcePosition
		f.Records[table][index] = updated
	}
	if matched == 0 {
		f.t.Fatalf("no source rows matched %s update", table)
	}
}
