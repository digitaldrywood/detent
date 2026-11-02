package platformstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func TestSQLiteCopySourceValidatesSealedCatalogCopies(t *testing.T) {
	t.Parallel()
	catalog := platformImportCatalog(t)
	registryPath := createSQLiteCopyFixture(t, catalog, "registry", RegistryCopyApplicationID, true)
	authPath := createSQLiteCopyFixture(t, catalog, "auth", AuthCopyApplicationID, false)
	source, err := OpenSQLiteCopySource(t.Context(), registryPath, authPath, catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	importSource := source.ImportSource()
	if importSource.DB != nil || importSource.RegistryDB == nil || importSource.AuthDB == nil || importSource.Registry.Schema == importSource.Auth.Schema || len(importSource.Registry.Organizations) != 1 || importSource.Registry.Organizations[0] != "org_example" {
		t.Fatalf("unexpected SQLite source binding: registry=%+v auth=%+v", importSource.Registry, importSource.Auth)
	}
	if len(importSource.SnapshotID) != len("sqlite-copy:")+64 {
		t.Fatalf("snapshot identity length = %d", len(importSource.SnapshotID))
	}
	if err := importSource.copyVerifier.Verify(t.Context(), true); err != nil {
		t.Fatalf("sealed source verification failed: %v", err)
	}
	i := Importer{source: importSource, organizations: map[string]bool{"org_example": true}}
	page, err := i.readPage(t.Context(), importCheckpoint{}, 2)
	if err != nil || len(page) != 1 || page[0].keys[0] != "org_example" {
		t.Fatalf("SQLite keyset first page=%+v error=%v", page, err)
	}
	page, err = i.readPage(t.Context(), importCheckpoint{started: true, after: [3]string{"org_example"}}, 2)
	if err != nil || len(page) != 0 {
		t.Fatalf("SQLite keyset continuation=%+v error=%v", page, err)
	}
}

func TestSQLiteCopySourceAcceptsColumnReorderingAndOptionalLitestreamTables(t *testing.T) {
	t.Parallel()
	catalog := platformImportCatalog(t)
	registryPath := createSQLiteCopyFixtureWithLayout(t, catalog, "registry", RegistryCopyApplicationID, true, true)
	authPath := createSQLiteCopyFixture(t, catalog, "auth", AuthCopyApplicationID, false)
	dropSQLiteTables(t, registryPath, "_litestream_lock", "_litestream_seq")
	source, err := OpenSQLiteCopySource(t.Context(), registryPath, authPath, catalog)
	if err != nil {
		t.Fatalf("open source with production column order and absent optional Litestream tables: %v", err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	importer := Importer{source: source.ImportSource(), organizations: map[string]bool{"org_example": true}}
	page, err := importer.readPage(t.Context(), importCheckpoint{}, 2)
	if err != nil || len(page) != 1 {
		t.Fatalf("read reordered organization: page=%+v error=%v", page, err)
	}
	if page[0].values["slug"] != "example" || page[0].values["checkout_price"] != "checkout_price_example" {
		t.Fatalf("organization values were assigned by physical order: slug=%v checkout_price=%v", page[0].values["slug"], page[0].values["checkout_price"])
	}
}

func TestSQLiteCopySourceKeepsColumnSignaturesStrictWhenOrderDiffers(t *testing.T) {
	t.Parallel()
	catalog := platformImportCatalog(t)
	registryPath := createSQLiteCopyFixtureWithLayout(t, catalog, "registry", RegistryCopyApplicationID, true, true)
	authPath := createSQLiteCopyFixture(t, catalog, "auth", AuthCopyApplicationID, false)
	column := catalog.Tables["registry/organizations"].Columns["slug"]
	column.Source = `[16,"slug","BLOB",1,"''",0,0]`
	catalog.Tables["registry/organizations"].Columns["slug"] = column
	if source, err := OpenSQLiteCopySource(t.Context(), registryPath, authPath, catalog); err == nil {
		if closeErr := source.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		t.Fatal("accepted a reordered source with a changed column type")
	}
}

func TestSQLiteCopySourceRejectsChangedCopyBeforeCompletion(t *testing.T) {
	t.Parallel()
	catalog := platformImportCatalog(t)
	registryPath := createSQLiteCopyFixture(t, catalog, "registry", RegistryCopyApplicationID, true)
	authPath := createSQLiteCopyFixture(t, catalog, "auth", AuthCopyApplicationID, false)
	source, err := OpenSQLiteCopySource(t.Context(), registryPath, authPath, catalog)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.Chmod(registryPath, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(registryPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if _, err := file.WriteAt([]byte{0}, info.Size()-1); err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.source.copyVerifier.Verify(t.Context(), true); !errors.Is(err, ErrImport) {
		t.Fatalf("changed source verification error = %v, want ErrImport", err)
	}
}

func TestSQLiteCopySourceRejectsWritableFilesAndSidecars(t *testing.T) {
	t.Parallel()
	catalog := platformImportCatalog(t)
	for _, test := range []struct {
		name                string
		writable            bool
		sidecar             bool
		malformedLitestream bool
	}{
		{name: "writable copy", writable: true},
		{name: "wal sidecar", sidecar: true},
		{name: "malformed Litestream table", malformedLitestream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			registryPath := createSQLiteCopyFixture(t, catalog, "registry", RegistryCopyApplicationID, true)
			authPath := createSQLiteCopyFixture(t, catalog, "auth", AuthCopyApplicationID, false)
			if test.writable {
				if err := os.Chmod(registryPath, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.sidecar {
				if err := os.WriteFile(registryPath+"-wal", []byte("pending"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.malformedLitestream {
				if err := os.Chmod(registryPath, 0o600); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", registryPath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `DROP TABLE "_litestream_seq"`); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `CREATE TABLE "_litestream_seq" (id TEXT PRIMARY KEY, seq INTEGER)`); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(registryPath, 0o400); err != nil {
					t.Fatal(err)
				}
			}
			if source, err := OpenSQLiteCopySource(t.Context(), registryPath, authPath, catalog); err == nil {
				if closeErr := source.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("unsafe SQLite source was accepted")
			}
		})
	}
}

func platformImportCatalog(t *testing.T) storagecatalog.Catalog {
	t.Helper()
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func createSQLiteCopyFixture(t *testing.T, catalog storagecatalog.Catalog, store string, applicationID int64, includeOrganization bool) string {
	return createSQLiteCopyFixtureWithLayout(t, catalog, store, applicationID, includeOrganization, false)
}

func createSQLiteCopyFixtureWithLayout(t *testing.T, catalog storagecatalog.Catalog, store string, applicationID int64, includeOrganization, productionOrganizationOrder bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), store+".db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id = %d", applicationID)); err != nil {
		t.Fatal(err)
	}
	tableNames := make([]string, 0)
	for name := range catalog.Tables {
		if table, ok := strings.CutPrefix(name, store+"/"); ok && table != "sqlite_schema" && table != "sqlite_sequence" {
			tableNames = append(tableNames, table)
		}
	}
	slices.Sort(tableNames)
	for _, tableName := range tableNames {
		table := catalog.Tables[store+"/"+tableName]
		type columnDDL struct {
			ordinal int
			query   string
		}
		columns := make([]columnDDL, 0, len(table.Columns))
		keys := make([]string, 0, 3)
		for name, column := range table.Columns {
			var definition []json.RawMessage
			if err := json.Unmarshal([]byte(column.Source), &definition); err != nil || len(definition) != 7 {
				t.Fatalf("invalid source definition for %s/%s", tableName, name)
			}
			var ordinal, notNull, primary int
			var sourceName, kind string
			if json.Unmarshal(definition[0], &ordinal) != nil || json.Unmarshal(definition[1], &sourceName) != nil || json.Unmarshal(definition[2], &kind) != nil || json.Unmarshal(definition[3], &notNull) != nil || json.Unmarshal(definition[5], &primary) != nil || sourceName != name {
				t.Fatalf("invalid source column definition for %s/%s", tableName, name)
			}
			if productionOrganizationOrder && store == "registry" && tableName == "organizations" {
				switch name {
				case "checkout_price":
					ordinal--
				case "slug":
					ordinal++
				}
			}
			item := `"` + name + `" ` + kind
			if notNull != 0 {
				item += " NOT NULL"
			}
			if string(definition[4]) != "null" {
				var defaultExpression string
				if json.Unmarshal(definition[4], &defaultExpression) != nil {
					t.Fatalf("invalid default source definition for %s/%s", tableName, name)
				}
				item += " DEFAULT (" + defaultExpression + ")"
			}
			columns = append(columns, columnDDL{ordinal: ordinal, query: item})
			if primary > 0 {
				keys = append(keys, fmt.Sprintf("%d:%s", primary, `"`+name+`"`))
			}
		}
		slices.SortFunc(columns, func(left, right columnDDL) int { return left.ordinal - right.ordinal })
		slices.Sort(keys)
		for n := range keys {
			_, key, _ := strings.Cut(keys[n], ":")
			keys[n] = key
		}
		columnQueries := make([]string, len(columns), len(columns)+1)
		for n, column := range columns {
			columnQueries[n] = column.query
		}
		if len(keys) != 0 {
			columnQueries = append(columnQueries, "PRIMARY KEY ("+strings.Join(keys, ",")+")")
		}
		query := "CREATE TABLE \"" + tableName + "\" (" + strings.Join(columnQueries, ",") + ")"
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatalf("create source table %s: %v", tableName, err)
		}
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE "_litestream_lock" (id INTEGER); CREATE TABLE "_litestream_seq" (id INTEGER PRIMARY KEY, seq INTEGER); INSERT INTO "_litestream_seq" (id,seq) VALUES (1,1)`); err != nil {
		t.Fatal(err)
	}
	if includeOrganization {
		source := ImportSource{Catalog: catalog}
		values := platformSourceValues(t, source, testConfig(t), "registry/organizations", "example", 1)
		columns := slices.Sorted(maps.Keys(values))
		placeholders, args := make([]string, len(columns)), make([]any, len(columns))
		for n, name := range columns {
			columns[n] = `"` + name + `"`
			placeholders[n] = "?"
			args[n] = values[name]
		}
		if _, err := db.ExecContext(t.Context(), "INSERT INTO organizations ("+strings.Join(columns, ",")+") VALUES ("+strings.Join(placeholders, ",")+")", args...); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), "CREATE INDEX registry_organization_slug_idx ON organizations(slug)"); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

func dropSQLiteTables(t *testing.T, path string, names ...string) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if _, err := db.ExecContext(t.Context(), `DROP TABLE "`+name+`"`); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
}
