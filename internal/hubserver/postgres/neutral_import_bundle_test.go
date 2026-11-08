package postgres_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func neutralFixtureTable(t *testing.T, name string, keys []postgres.NeutralImportKey) postgres.NeutralImportTable {
	t.Helper()
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
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
func neutralFixtureRow(t *testing.T, table postgres.NeutralImportTable, values map[string]any) postgres.NeutralImportRow {
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
func writeNeutralFixture(t *testing.T, organization tracker.OrganizationID, tables []postgres.NeutralImportTable, records map[string][]postgres.NeutralImportRow) (string, postgres.NeutralImportManifest) {
	t.Helper()
	directory := t.TempDir()
	manifest := postgres.NeutralImportManifest{Version: 1, Identity: postgres.NeutralImportIdentity{SourceID: "example-source", SourceSHA256: artifact.Digest([]byte("historical source")), OrganizationID: organization, SchemaVersion: 1}}
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
		for offset := 0; offset < len(rows); offset += 100 {
			page := rows[offset:min(offset+100, len(rows))]
			raw, err := json.Marshal(page)
			if err != nil {
				t.Fatal(err)
			}
			file := fmt.Sprintf("%s_%d.json", table.Name, offset/100)
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
func TestNeutralImportBundleVerifiesBeforeReadAndSeeksCompositeKeys(t *testing.T) {
	table := neutralFixtureTable(t, "usage_cost_observations", []postgres.NeutralImportKey{{Column: "organization_id", Kind: "text"}, {Column: "provider", Kind: "text"}, {Column: "provider_account", Kind: "text"}, {Column: "source_id", Kind: "text"}, {Column: "period_start", Kind: "text"}, {Column: "period_end", Kind: "text"}, {Column: "revision", Kind: "integer"}})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		var records []postgres.NeutralImportRow
		for revision := int64(1); revision <= 205; revision++ {
			records = append(records, neutralFixtureRow(t, table, map[string]any{"organization_id": org, "provider": "example", "provider_account": "account", "source_id": "source", "period_start": "2026-11-01T00:00:00Z", "period_end": "2026-12-01T00:00:00Z", "revision": revision}))
		}
		directory, _ := writeNeutralFixture(t, org, []postgres.NeutralImportTable{table}, map[string][]postgres.NeutralImportRow{table.Name: records})
		if err := postgres.ReadNeutralImportBundle(t.Context(), directory, org, nil); err != nil {
			t.Fatal(err)
		}
		err := postgres.ReadNeutralImportBundle(t.Context(), directory, org, func(source *postgres.VerifiedNeutralImport) error {
			var after []string
			var got int64
			for {
				rows, err := source.Page(t.Context(), table.Name, after, 13)
				if err != nil {
					return err
				}
				if len(rows) == 0 {
					break
				}
				for _, row := range rows {
					got++
					if row.Key[6] != strconv.FormatInt(got, 10) {
						t.Fatalf("cursor lost numeric ordering: got=%s want=%d", row.Key[6], got)
					}
				}
				after = rows[len(rows)-1].Key
			}
			if got != 205 {
				t.Fatalf("read %d rows", got)
			}
			ordinalPage, err := source.OrdinalPage(t.Context(), table.Name, 99, 3)
			if err != nil || len(ordinalPage) != 1 || ordinalPage[0].Ordinal != 100 || ordinalPage[0].Key[6] != "100" {
				t.Fatalf("ordinal boundary page=%+v: %v", ordinalPage, err)
			}
			ordinalPage, err = source.OrdinalPage(t.Context(), table.Name, 100, 3)
			if err != nil || len(ordinalPage) != 3 || ordinalPage[0].Ordinal != 101 || ordinalPage[2].Ordinal != 103 || ordinalPage[0].Key[6] != "101" {
				t.Fatalf("ordinal continuation=%+v: %v", ordinalPage, err)
			}
			key := slices.Clone(records[101].Key)
			row, err := source.Lookup(t.Context(), table.Name, key)
			if err != nil || !slices.Equal(row.Key, key) {
				t.Fatalf("lookup=%+v: %v", row, err)
			}
			key[6] = "206"
			if _, err := source.Lookup(t.Context(), table.Name, key); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("missing key: %v", err)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
func TestNeutralImportBundleRejectsMalformedSourcesBeforeCallback(t *testing.T) {
	for _, scenario := range []string{"version", "schema version", "tenant", "checksum", "key order", "path escape", "writable page", "schema drift", "duplicate source row ID", "negative source position"} {
		t.Run(scenario, func(t *testing.T) {
			table := neutralFixtureTable(t, "projects", []postgres.NeutralImportKey{{Column: "id", Kind: "text"}})
			records := []postgres.NeutralImportRow{neutralFixtureRow(t, table, map[string]any{"organization_id": "org_first", "id": "project_a"}), neutralFixtureRow(t, table, map[string]any{"organization_id": "org_first", "id": "project_b"})}
			if scenario == "duplicate source row ID" {
				records[0].SourceRowID = 7
				records[1].SourceRowID = 7
			}
			if scenario == "negative source position" {
				records[0].SourcePosition = new(int64(-1))
			}
			directory, manifest := writeNeutralFixture(t, "org_first", []postgres.NeutralImportTable{table}, map[string][]postgres.NeutralImportRow{"projects": records})
			switch scenario {
			case "schema drift":
				manifest.Tables[0].Columns["id"] = "invalid signature"
			case "version":
				manifest.Version = 2
			case "schema version":
				manifest.Identity.SchemaVersion = 2
			case "tenant":
				manifest.Identity.OrganizationID = "org_second"
			case "checksum":
				manifest.Tables[0].Pages[0].SHA256 = artifact.Digest([]byte("other"))
			case "key order":
				manifest.Tables[0].Pages[0].First = []string{"project_b"}
			case "path escape":
				manifest.Tables[0].Pages[0].File = "../outside.json"
			case "writable page":
				if err := os.Chmod(filepath.Join(directory, manifest.Tables[0].Pages[0].File), 0600); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(directory, "manifest.json")
			if _, err := os.ReadFile(manifestPath); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(manifestPath, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, raw, 0400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(manifestPath, 0400); err != nil {
				t.Fatal(err)
			}
			called := false
			err = postgres.ReadNeutralImportBundle(t.Context(), directory, "org_first", func(*postgres.VerifiedNeutralImport) error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("malformed source reached callback=%v: %v", called, err)
			}
			if scenario == "checksum" && !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("checksum error=%v", err)
			}
		})
	}
}
