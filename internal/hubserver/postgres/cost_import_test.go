package postgres_test

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
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

type costImportObjects struct {
	*historyObjects
	fail bool
}

func (s *costImportObjects) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if s.fail {
		return attachment.ArchiveReference{}, errors.New("archive unavailable")
	}
	return s.historyObjects.StoreDetail(ctx, organization, raw)
}

func costImportSourceFor(t *testing.T, db *sql.DB, organization tracker.OrganizationID, objects *costImportObjects) postgres.CostImportSource {
	t.Helper()
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
	source := postgres.CostImportSource{DB: db, Catalog: catalog, Schema: schema, SnapshotID: "frozen_example", Objects: objects, Source: storagecatalog.Source{Schema: string(organization), Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(organization)}}
	if _, err := db.ExecContext(t.Context(), "CREATE SCHEMA "+pq.QuoteIdentifier(source.Source.Schema)); err != nil {
		t.Fatal(err)
	}
	var columns []string
	keys := make([]string, 7)
	for name, definition := range catalog.Tables["hub/usage_cost_observations"].Columns {
		columns = append(columns, pq.QuoteIdentifier(name)+` text COLLATE "C"`)
		var metadata []json.RawMessage
		if err := json.Unmarshal([]byte(definition.Source), &metadata); err != nil {
			t.Fatal(err)
		}
		var position int
		if err := json.Unmarshal(metadata[5], &position); err != nil {
			t.Fatal(err)
		}
		if position > 0 {
			keys[position-1] = pq.QuoteIdentifier(name)
		}
	}
	statement := "CREATE TABLE " + pq.QuoteIdentifier(source.Source.Schema) + `.usage_cost_observations (` + strings.Join(columns, ",") + ", PRIMARY KEY (" + strings.Join(keys, ",") + "))"
	if _, err := db.ExecContext(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
	return source
}

func TestCostImportResumesAndPreservesCurrentAndHistoricalCosts(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	admin.SetMaxOpenConns(6)
	from := time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, organization)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: "project_example", Name: "Example", Profile: "native"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		objects := &costImportObjects{historyObjects: newHistoryObjects()}
		source := costImportSourceFor(t, admin, organization, objects)
		for _, sourceID := range []string{"active", "voided"} {
			for _, revision := range []int64{9, 10} {
				amount := revision * 10
				o := usagecost.Observation{Provider: "example", ProviderAccount: "account", ResourceID: "same_resource", Bucket: "runner_api", Metric: "tokens", SourceID: sourceID, Revision: revision, From: from, To: from.Add(time.Nanosecond), AmountMicros: &amount, Currency: "USD", Unit: "token", Voided: sourceID == "voided" && revision == 10}
				raw, err := json.Marshal(o)
				if err != nil {
					t.Fatal(err)
				}
				statement := "INSERT INTO " + pq.QuoteIdentifier(source.Source.Schema) + `.usage_cost_observations (organization_id,project_id,provider,provider_account,resource_id,bucket,metric,source_id,period_start,period_end,revision,observation_json,received_at,voided) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`
				voided := "0"
				if o.Voided {
					voided = "1"
				}
				if _, err := admin.ExecContext(t.Context(), statement, organization, "project_example", o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric, sourceID, from.Format(time.RFC3339Nano), o.To.Format(time.RFC3339Nano), fmt.Sprint(revision), string(raw), from.Add(time.Hour).Format(time.RFC3339Nano), voided); err != nil {
					t.Fatal(err)
				}
			}
		}
		source.SnapshotID = exportCostTestSnapshot(t, admin)
		var progress postgres.CostImportProgress
		for step := 0; step < 20 && !progress.Done; step++ {
			importer, err := repository.NewCostImporter(t.Context(), source)
			if err != nil {
				t.Fatal(err)
			}
			if step == 1 {
				if _, err := admin.ExecContext(t.Context(), "UPDATE "+pq.QuoteIdentifier(source.Source.Schema)+`.usage_cost_observations SET observation_json='{}'`); err != nil {
					t.Fatal(err)
				}
			}
			if step == 2 {
				objects.fail = true
				failed, err := importer.Step(t.Context(), 1)
				objects.fail = false
				if err == nil || failed != progress {
					t.Fatalf("failed archive advanced import: %+v %v", failed, err)
				}
			}
			progress, err = importer.Step(t.Context(), 1)
			if err != nil {
				t.Fatalf("step %d: %v", step, err)
			}
		}
		want := postgres.CostImportProgress{SourceRows: 4, CurrentSources: 2, HistoryRows: 4, Done: true}
		if progress != want {
			t.Fatalf("progress=%+v want %+v", progress, want)
		}
		importer, err := repository.NewCostImporter(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		if replay, err := importer.Step(t.Context(), 1); err != nil || replay != want {
			t.Fatalf("completed replay=%+v %v", replay, err)
		}
		func() {
			tx, err := admin.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback() }()
			if _, err := tx.ExecContext(t.Context(), `SELECT set_config('detent.organization_id',$1,true)`, organization); err != nil {
				t.Fatal(err)
			}
			var revisions, older int
			var received string
			if err := tx.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER (WHERE revision=9),min(received_at) FROM cost_observation_revisions WHERE organization_id=$1`, organization).Scan(&revisions, &older, &received); err != nil {
				t.Fatal(err)
			}
			if revisions != 4 || older != 2 || received != from.Add(time.Hour).Format("2006-01-02T15:04:05.000000000Z") {
				t.Fatalf("retained history=%d older=%d received=%s", revisions, older, received)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}()
		source.SnapshotID = exportCostTestSnapshot(t, admin)
		other, err := repository.NewCostImporter(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.Step(t.Context(), 1); !errors.Is(err, postgres.ErrCostImport) {
			t.Fatalf("changed snapshot accepted: %v", err)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, project := range []tracker.ProjectID{"", "project_example"} {
				spend, err := r.BillingMonthlySpend(t.Context(), project, from)
				if err != nil {
					return err
				}
				if spend.RunnerMicros != 100 {
					t.Errorf("project %s spend=%+v", project, spend)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func exportCostTestSnapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	var snapshot string
	if err := tx.QueryRowContext(t.Context(), "SELECT pg_export_snapshot()").Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
