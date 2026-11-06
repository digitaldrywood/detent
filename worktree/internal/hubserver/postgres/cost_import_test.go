package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

type costImportObjects struct {
	*historyObjects
	fail bool
}

func schemaFromCatalogForTest(catalog storagecatalog.Catalog) storagecatalog.Schema {
	schema := storagecatalog.Schema{}
	for name, table := range catalog.Tables {
		name, ok := strings.CutPrefix(name, "hub/")
		if !ok {
			continue
		}
		schema[name] = make(map[string]string, len(table.Columns))
		for column, definition := range table.Columns {
			schema[name][column] = definition.Source
		}
	}
	return schema
}

func (s *costImportObjects) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if s.fail {
		return attachment.ArchiveReference{}, errors.New("archive unavailable")
	}
	return s.historyObjects.StoreDetail(ctx, organization, raw)
}

func TestCostImportResumesAndPreservesCurrentAndHistoricalCosts(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	admin.SetMaxOpenConns(6)
	from := time.Date(2026, 1, 1, 0, 0, 0, 1, time.UTC)
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	sourceCopy := newBillingImportCopy(t, catalog, "usage_cost_observations")
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, sourceID := range []string{"active", "voided"} {
			for _, revision := range []int64{9, 10} {
				amount := revision * 10
				o := usagecost.Observation{Provider: "example", ProviderAccount: "account", ResourceID: "same_resource", Bucket: "runner_api", Metric: "tokens", SourceID: sourceID, Revision: revision, From: from, To: from.Add(time.Nanosecond), AmountMicros: &amount, Currency: "USD", Unit: "token", Voided: sourceID == "voided" && revision == 10}
				raw, err := json.Marshal(o)
				if err != nil {
					t.Fatal(err)
				}
				statement := `INSERT INTO usage_cost_observations (organization_id,project_id,provider,provider_account,resource_id,bucket,metric,source_id,period_start,period_end,revision,observation_json,received_at,voided) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
				voided := 0
				if o.Voided {
					voided = 1
				}
				if _, err := sourceCopy.db.ExecContext(t.Context(), statement, organization, "project_example", o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric, sourceID, from.Format(time.RFC3339Nano), o.To.Format(time.RFC3339Nano), revision, string(raw), from.Add(time.Hour).Format(time.RFC3339Nano), voided); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	copyPath := sourceCopy.seal(t)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedTenantAccess(t, repository, organization)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: organization, ID: "project_example", Name: "Example", Profile: "native"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		objects := &costImportObjects{historyObjects: newHistoryObjects()}
		source := postgres.CostImportSource{CopyPath: copyPath, Catalog: catalog, Schema: schemaFromCatalogForTest(catalog), Objects: objects, Source: storagecatalog.Source{Schema: "hub_copy", Store: "hub", CatalogVersion: catalog.Version, OrganizationID: string(organization)}}
		objects.fail = true
		if _, err := repository.ImportCostCopy(t.Context(), source); err == nil {
			t.Fatal("missing object storage accepted")
		}
		objects.fail = false
		progress, err := repository.ImportCostCopy(t.Context(), source)
		if err != nil {
			t.Fatal(err)
		}
		want := postgres.CostImportProgress{SourceRows: 4, CurrentSources: 2, HistoryRows: 4, Done: true}
		if progress != want {
			t.Fatalf("progress=%+v want %+v", progress, want)
		}
		if replay, err := repository.ImportCostCopy(t.Context(), source); err != nil || replay != want {
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
		changedCopy := newBillingImportCopy(t, catalog, "usage_cost_observations")
		if _, err := changedCopy.db.ExecContext(t.Context(), `INSERT INTO usage_cost_observations (organization_id,project_id,provider,provider_account,resource_id,bucket,metric,source_id,period_start,period_end,revision,observation_json,received_at,voided) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, organization, "project_example", "example", "account", "same_resource", "runner_api", "tokens", "new", from.Format(time.RFC3339Nano), from.Add(time.Nanosecond).Format(time.RFC3339Nano), 1, `{}`, from.Add(time.Hour).Format(time.RFC3339Nano), 0); err != nil {
			t.Fatal(err)
		}
		changed := source
		changed.CopyPath = changedCopy.seal(t)
		if _, err := repository.ImportCostCopy(t.Context(), changed); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("changed source copy accepted: %v", err)
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
