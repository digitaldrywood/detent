package postgres_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func driftNeutralCoreSchema(t *testing.T, path, tableName, column, change string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(path, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest postgres.NeutralImportManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	records := make(map[string][]postgres.NeutralImportRow)
	for index := range manifest.Tables {
		table := &manifest.Tables[index]
		for _, page := range table.Pages {
			raw, err := os.ReadFile(filepath.Join(path, page.File))
			if err != nil {
				t.Fatal(err)
			}
			var rows []postgres.NeutralImportRow
			if err := json.Unmarshal(raw, &rows); err != nil {
				t.Fatal(err)
			}
			records[table.Name] = append(records[table.Name], rows...)
		}
		if table.Name != tableName {
			continue
		}
		if change == "extra" {
			table.Columns["unaccounted"] = fmt.Sprintf(`[%d,"unaccounted","TEXT",1,null,0,0]`, len(table.Columns))
			for _, row := range records[table.Name] {
				row.Values["unaccounted"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("must survive migration")}
			}
			continue
		}
		var signature []any
		if err := json.Unmarshal([]byte(table.Columns[column]), &signature); err != nil {
			t.Fatal(err)
		}
		switch change {
		case "type":
			signature[2] = "BLOB"
		case "required":
			signature[3] = 1 - signature[3].(float64)
		case "primary":
			signature[5] = 0
		}
		raw, err := json.Marshal(signature)
		if err != nil {
			t.Fatal(err)
		}
		table.Columns[column] = string(raw)
	}
	directory, _ := writeNeutralFixture(t, manifest.Identity.OrganizationID, manifest.Tables, records)
	return directory
}

func TestNeutralCoreImportsRejectWellFormedSchemaDriftBeforePin(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, target := range []struct{ table, column, family string }{
		{"projects", "name", "project"},
		{"repositories", "github_owner", "project"},
		{"policy_revisions", "approved_by", "policy"},
		{"project_policies", "policy_id", "policy"},
		{"change_review_policies", "policy_json", "policy"},
		{"monthly_budget_policies", "revision", "policy"},
		{"issues", "title", "budget"},
		{"leases", "machine_id", "budget"},
		{"monthly_budget_admissions", "issue_id", "budget"},
		{"monthly_budget_leases", "lease_id", "budget"},
	} {
		for _, change := range []string{"extra", "type", "required", "primary"} {
			if change == "primary" && target.table != "leases" {
				continue
			}
			t.Run(target.table+"_"+change, func(t *testing.T) {
				org := tracker.OrganizationID("org_" + target.table + "_" + change)
				if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
					return seedCoreOrganization(t.Context(), w, org)
				}); err != nil {
					t.Fatal(err)
				}
				path := corePolicyCopy(t, org, time.Date(2026, 11, 1, 21, 0, 0, 0, time.UTC))
				if target.family == "project" {
					path = coreProjectCopy(t, org)
				}
				column := target.column
				if change == "primary" {
					column = "fencing_token"
				}
				path = driftNeutralCoreSchema(t, path, target.table, column, change)
				var err error
				switch target.family {
				case "project":
					_, err = repository.ImportCoreProjects(t.Context(), postgres.CoreProjectImportSource{BundlePath: path, OrganizationID: org, Objects: newHistoryObjects()})
				case "policy":
					_, err = repository.ImportCorePolicies(t.Context(), postgres.CorePolicyImportSource{BundlePath: path, OrganizationID: org, Objects: newHistoryObjects()})
				case "budget":
					_, err = repository.ImportCoreMonthlyBudgetFacts(t.Context(), postgres.CorePolicyImportSource{BundlePath: path, OrganizationID: org, Objects: newHistoryObjects()})
				}
				if !errors.Is(err, storagecatalog.ErrUnmapped) {
					t.Fatalf("well-formed %s drift was not rejected: %v", change, err)
				}
				for _, table := range []string{"neutral_import_sources", "policy_revisions", "monthly_budget_admissions"} {
					var count int
					if err := admin.QueryRowContext(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s WHERE organization_id=$1", table), org).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Fatalf("unsupported source wrote %s rows=%d", table, count)
					}
				}
			})
		}
	}
}
