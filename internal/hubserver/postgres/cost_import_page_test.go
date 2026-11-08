package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

func costNeutralFixture(t *testing.T, columns map[string]string, keys []NeutralImportKey, records []NeutralImportRow) string {
	t.Helper()
	directory := t.TempDir()
	table := NeutralImportTable{Name: "usage_cost_observations", Columns: columns, Key: keys}
	if len(records) > 0 {
		raw, err := json.Marshal(records)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "costs.json"), raw, 0400); err != nil {
			t.Fatal(err)
		}
		table.Pages = []NeutralImportPage{{File: "costs.json", SHA256: artifact.Digest(raw), Rows: len(records), First: records[0].Key, Last: records[len(records)-1].Key}}
	}
	manifest := NeutralImportManifest{Version: 1, Identity: NeutralImportIdentity{SourceID: "example-source", SourceSHA256: artifact.Digest([]byte("source")), OrganizationID: "org_example", SchemaVersion: 1}, Tables: []NeutralImportTable{table}}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "manifest.json"), raw, 0400); err != nil {
		t.Fatal(err)
	}
	return directory
}
func costNeutralColumns(catalog storagecatalog.Catalog) (map[string]string, []NeutralImportKey) {
	columns := map[string]string{}
	for name, column := range catalog.Tables["hub/usage_cost_observations"].Columns {
		columns[name] = column.Source
	}
	var keys []NeutralImportKey
	for index, name := range costImportColumns[:7] {
		kind := "text"
		if index == 6 {
			kind = "integer"
		}
		keys = append(keys, NeutralImportKey{Column: name, Kind: kind})
	}
	return columns, keys
}
func TestCostImportPagesPreserveCompositeTextCursor(t *testing.T) {
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	schema := schemaFromCatalog(catalog)
	binding, err := catalog.Bind(storagecatalog.Source{Schema: "hub_neutral", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: "org_example"}, schema)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC)
	var records []NeutralImportRow
	for _, source := range []string{"first", "second"} {
		for _, revision := range []int64{9, 10} {
			observation := usagecost.Observation{Provider: "example", ProviderAccount: "account", ResourceID: source, Bucket: "runner_api", Metric: "tokens", SourceID: source, From: from, To: from.Add(time.Nanosecond), Revision: revision}
			payload, err := json.Marshal(observation)
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]any{"organization_id": "org_example", "project_id": "project_example", "provider": "example", "provider_account": "account", "source_id": source, "period_start": from.Format(costPeriodLayout), "period_end": observation.To.Format(costPeriodLayout), "revision": revision, "resource_id": source, "bucket": "runner_api", "metric": "tokens", "received_at": from.Add(time.Hour).Format(costPeriodLayout), "voided": nil, "observation_json": string(payload)}
			row := NeutralImportRow{Values: map[string]storagecatalog.Cell{}}
			for index, name := range costImportColumns {
				cell := storagecatalog.Cell{Null: true}
				if values[name] != nil {
					cell = storagecatalog.Cell{Encoding: "utf8", Data: []byte(fmt.Sprint(values[name]))}
				}
				row.Values[name] = cell
				if index < 7 {
					row.Key = append(row.Key, string(cell.Data))
				}
			}
			records = append(records, row)
		}
	}
	columns, keys := costNeutralColumns(catalog)
	directory := costNeutralFixture(t, columns, keys, records)
	if err := ReadNeutralImportBundle(t.Context(), directory, "org_example", func(source *VerifiedNeutralImport) error {
		var after [7]string
		for n, want := range []string{"first:9", "first:10", "second:9", "second:10"} {
			page, err := readNeutralCostImportPage(t.Context(), source, "org_example", catalog, binding, after, n > 0, 1)
			if err != nil {
				return err
			}
			if len(page) != 1 {
				t.Fatalf("page %d rows=%d", n, len(page))
			}
			row := page[0]
			if got := fmt.Sprintf("%s:%d", row.record.SourceID, row.record.Revision); got != want {
				t.Fatalf("page %d=%s want %s", n, got, want)
			}
			if !row.record.From.Equal(from) || !row.record.To.Equal(from.Add(time.Nanosecond)) {
				t.Fatal("source timestamps lost precision")
			}
			after = row.key
		}
		page, err := readNeutralCostImportPage(context.Background(), source, "org_example", catalog, binding, after, true, 1)
		if err != nil || len(page) != 0 {
			t.Fatalf("final page=%d error=%v", len(page), err)
		}
		changed := catalog.Tables["hub/usage_cost_observations"].Columns["source_id"]
		changed.Disposition = "pending"
		catalog.Tables["hub/usage_cost_observations"].Columns["source_id"] = changed
		if _, err := readNeutralCostImportPage(t.Context(), source, "org_example", catalog, binding, after, true, 1); !errors.Is(err, storagecatalog.ErrPending) {
			t.Fatalf("pending source column error=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestCostImportSchemaAcceptsPhysicalColumnOrderAndRejectsTypeOrKeyDrift(t *testing.T) {
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, provider, primaryKey string
		wantErr                    bool
	}{
		{name: "column order", provider: "TEXT", primaryKey: "organization_id,provider,provider_account,source_id,period_start,period_end,revision"},
		{name: "column type", provider: "BLOB", primaryKey: "organization_id,provider,provider_account,source_id,period_start,period_end,revision", wantErr: true},
		{name: "primary key order", provider: "TEXT", primaryKey: "organization_id,provider_account,provider,source_id,period_start,period_end,revision", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			columns, _ := costNeutralColumns(catalog)
			primary := strings.Split(test.primaryKey, ",")
			var keys []NeutralImportKey
			for _, name := range primary {
				kind := "text"
				if name == "revision" {
					kind = "integer"
				}
				keys = append(keys, NeutralImportKey{Column: name, Kind: kind})
			}
			for name, signature := range columns {
				var fields []any
				if err := json.Unmarshal([]byte(signature), &fields); err != nil {
					t.Fatal(err)
				}
				fields[0] = float64(len(columns)-1) - fields[0].(float64)
				fields[5] = float64(0)
				for index, key := range primary {
					if name == key {
						fields[5] = float64(index + 1)
					}
				}
				if name == "provider" {
					fields[2] = test.provider
				}
				raw, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				columns[name] = string(raw)
			}
			directory := costNeutralFixture(t, columns, keys, nil)
			if err := ReadNeutralImportBundle(t.Context(), directory, "org_example", func(source *VerifiedNeutralImport) error {
				importer := costImporter{source: CostImportSource{Catalog: catalog, Schema: schemaFromCatalog(catalog), neutral: source}}
				err := importer.validateSource(t.Context())
				if test.wantErr {
					if !errors.Is(err, storagecatalog.ErrUnmapped) {
						t.Fatalf("schema error=%v want %v", err, storagecatalog.ErrUnmapped)
					}
				} else if err != nil {
					t.Fatalf("column-order-only source rejected: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
