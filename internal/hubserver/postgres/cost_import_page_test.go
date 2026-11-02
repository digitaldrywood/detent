package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

func TestCostImportPagesPreserveCompositeTextCursor(t *testing.T) {
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE usage_cost_observations (
organization_id TEXT NOT NULL, project_id TEXT NOT NULL, provider TEXT NOT NULL,
provider_account TEXT NOT NULL, resource_id TEXT NOT NULL, bucket TEXT NOT NULL,
metric TEXT NOT NULL, source_id TEXT NOT NULL, period_start TEXT NOT NULL,
period_end TEXT NOT NULL, revision INTEGER NOT NULL, observation_json TEXT NOT NULL,
received_at TEXT NOT NULL, voided INTEGER,
PRIMARY KEY (organization_id,provider,provider_account,source_id,period_start,period_end,revision))`); err != nil {
		t.Fatal(err)
	}
	schema := schemaFromCatalog(catalog)
	binding, err := catalog.Bind(storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: "org_example"}, schema)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC)
	for _, source := range []string{"first", "second"} {
		for _, revision := range []int64{9, 10} {
			observation := usagecost.Observation{Provider: "example", ProviderAccount: "account", ResourceID: source, Bucket: "runner_api", Metric: "tokens", SourceID: source, From: from, To: from.Add(time.Nanosecond), Revision: revision}
			payload, err := json.Marshal(observation)
			if err != nil {
				t.Fatal(err)
			}
			values := []any{"org_example", "project_example", "example", "account", source, "runner_api", "tokens", source, from.Format(costPeriodLayout), observation.To.Format(costPeriodLayout), revision, string(payload), from.Add(time.Hour).Format(costPeriodLayout), nil}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO usage_cost_observations VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, values...); err != nil {
				t.Fatal(err)
			}
		}
	}
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	var after [7]string
	for n, want := range []string{"first:9", "first:10", "second:9", "second:10"} {
		page, err := readCostImportPage(t.Context(), tx, "org_example", catalog, binding, after, n > 0, 1)
		if err != nil {
			t.Fatal(err)
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
	page, err := readCostImportPage(context.Background(), tx, "org_example", catalog, binding, after, true, 1)
	if err != nil || len(page) != 0 {
		t.Fatalf("final page=%d error=%v", len(page), err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	changed := catalog.Tables["hub/usage_cost_observations"].Columns["source_id"]
	changed.Disposition = "pending"
	catalog.Tables["hub/usage_cost_observations"].Columns["source_id"] = changed
	if _, err := readCostImportPage(t.Context(), tx, "org_example", catalog, binding, after, true, 1); err == nil {
		t.Fatal("pending source column accepted")
	}
}
