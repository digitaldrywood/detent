package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

func TestCostImportPagesPreserveCompositeTextCursor(t *testing.T) {
	fixture := pgtest.NewTemplate(func(context.Context, *sql.DB) error { return nil })
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		if err := fixture.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	db := fixture.Database(t)
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	schema := storagecatalog.Schema{}
	for name, table := range catalog.Tables {
		if name, ok := strings.CutPrefix(name, "hub/"); ok {
			schema[name] = map[string]string{}
			for column, definition := range table.Columns {
				schema[name][column] = definition.Source
			}
		}
	}
	binding, err := catalog.Bind(storagecatalog.Source{Schema: "cost_fixture", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: "org_example"}, schema)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA cost_fixture"); err != nil {
		t.Fatal(err)
	}
	var columns, keys, parameters []string
	for n, name := range costImportColumns {
		columns = append(columns, pq.QuoteIdentifier(name)+` text COLLATE "C"`)
		parameters = append(parameters, fmt.Sprintf("$%d", n+1))
		if n < 7 {
			keys = append(keys, pq.QuoteIdentifier(name))
		}
	}
	if _, err := db.ExecContext(t.Context(), "CREATE TABLE cost_fixture.usage_cost_observations ("+strings.Join(columns, ",")+", PRIMARY KEY ("+strings.Join(keys, ",")+"))"); err != nil {
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
			values := []any{"org_example", "example", "account", source, from.Format(costPeriodLayout), observation.To.Format(costPeriodLayout), fmt.Sprint(revision), "project_example", source, "runner_api", "tokens", from.Add(time.Hour).Format(costPeriodLayout), nil, string(payload)}
			if _, err := db.ExecContext(t.Context(), "INSERT INTO cost_fixture.usage_cost_observations VALUES ("+strings.Join(parameters, ",")+")", values...); err != nil {
				t.Fatal(err)
			}
		}
	}
	source := CostImportSource{DB: db, Catalog: catalog, Source: storagecatalog.Source{Schema: "cost_fixture", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: "org_example"}, Schema: schema, SnapshotID: "frozen_example", Objects: costImportNoObjects{}}
	keeper, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keeper.Rollback() })
	if err := keeper.QueryRowContext(t.Context(), "SELECT pg_export_snapshot()").Scan(&source.SnapshotID); err != nil {
		t.Fatal(err)
	}
	importer, err := (&Repository{}).NewCostImporter(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	mapping := catalog.Tables["hub/usage_cost_observations"].Columns["source_id"]
	changed := mapping
	changed.Disposition = "pending"
	catalog.Tables["hub/usage_cost_observations"].Columns["source_id"] = changed
	if importer.source.Catalog.Tables["hub/usage_cost_observations"].Columns["source_id"].Disposition == "pending" {
		t.Fatal("caller mutation changed frozen import catalog")
	}
	catalog.Tables["hub/usage_cost_observations"].Columns["source_id"] = mapping
	var after [7]string
	for n, want := range []string{"first:10", "first:9", "second:10", "second:9"} {
		page, err := readCostImportPage(t.Context(), db, "cost_fixture", catalog, binding, after, n > 0, 1)
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
	page, err := readCostImportPage(t.Context(), db, "cost_fixture", catalog, binding, after, true, 1)
	if err != nil || len(page) != 0 {
		t.Fatalf("final page=%d error=%v", len(page), err)
	}
	if err := keeper.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Repository{}).NewCostImporter(t.Context(), source); !errors.Is(err, ErrCostImport) {
		t.Fatalf("expired snapshot accepted: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), "ALTER TABLE cost_fixture.usage_cost_observations DROP CONSTRAINT usage_cost_observations_pkey"); err != nil {
		t.Fatal(err)
	}
	newKeeper, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = newKeeper.Rollback() })
	if err := newKeeper.QueryRowContext(t.Context(), "SELECT pg_export_snapshot()").Scan(&source.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := (&Repository{}).NewCostImporter(t.Context(), source); !errors.Is(err, ErrCostImport) {
		t.Fatalf("unindexed source accepted: %v", err)
	}
}

type costImportNoObjects struct{}

func (costImportNoObjects) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	return attachment.ArchiveReference{}, errors.New("unexpected archive write")
}

func (costImportNoObjects) ReadDetail(context.Context, string, attachment.ArchiveReference) ([]byte, error) {
	return nil, errors.New("unexpected archive read")
}
