package storagecatalog

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func TestTargetCatalogMatchesBillingTargetClassification(t *testing.T) {
	catalog, err := ReadTargetCatalog(os.DirFS("."), "targets/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../hubserver/postgres/testdata/target_classification.json")
	if err != nil {
		t.Fatal(err)
	}
	var classification map[string]TargetClassification
	if err := json.Unmarshal(data, &classification); err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateClassification(classification); err != nil {
		t.Fatal(err)
	}
}

func TestNewSchemaQualifiedMigrationNeedsTargetContract(t *testing.T) {
	const migration = "internal/hubserver/postgres/migrations/20990101000000_future.sql"
	files := fstest.MapFS{
		"internal/hubserver/postgres/migrations": {Mode: fs.ModeDir},
		migration:                                {Data: []byte(`CREATE TABLE "audit" . "events" (organization_id text NOT NULL, event_id text NOT NULL);`)},
	}
	catalog := TargetCatalog{Version: "1", LegacyMigrations: map[string]string{}, Tables: map[string]TargetTable{}}
	if err := catalog.ValidateMigrations(files, nil); !errors.Is(err, ErrCatalog) {
		t.Fatalf("uncovered new migration error = %v", err)
	}
	catalog.Migrations = []string{migration}
	catalog.Tables = map[string]TargetTable{"audit.events": {
		Family: "audit", Contract: "Retain tenant-owned event records.", Organization: "organization-owned",
		Columns: map[string]string{"organization_id": "text", "event_id": "text"},
	}}
	if err := catalog.ValidateMigrations(files, nil); err != nil {
		t.Fatalf("covered schema-qualified migration: %v", err)
	}
	if err := catalog.ValidateClassification(map[string]TargetClassification{
		"audit/events": {Ownership: "organization-owned", Scope: "audit.events.organization_id", InlinePayloads: []string{}},
	}); err != nil {
		t.Fatalf("schema-qualified classification: %v", err)
	}
	files[migration].Data = []byte(`CREATE TABLE "bad-name" (id text);`)
	if err := catalog.ValidateMigrations(files, nil); !errors.Is(err, ErrCatalog) {
		t.Fatalf("unrecognized table declaration error = %v", err)
	}
}

func TestTargetCatalogAcceptsBinaryColumns(t *testing.T) {
	catalog := TargetCatalog{
		Version:          "1",
		LegacyMigrations: map[string]string{},
		Migrations:       []string{"internal/hubserver/postgres/migrations/20990101000000_future.sql"},
		Tables: map[string]TargetTable{
			"public.hub_server_identities": {
				Family: "access", Contract: "Preserve binary tenant identity keys.", Organization: "organization-owned",
				Columns: map[string]string{"organization_id": "text", "cursor_key": "bytea"},
			},
		},
	}
	if err := catalog.Validate(); err != nil {
		t.Fatalf("binary target schema: %v", err)
	}
}

func TestMigrationCatalogPreservesPinnedHistory(t *testing.T) {
	const root = "internal/hubserver/postgres/migrations/"
	const legacy = root + "00001_legacy.sql"
	const catalogued = root + "20261101000000_catalogued.sql"
	const later = root + "20261102000000_later.sql"
	original := []byte("CREATE TABLE old (id text);")
	catalog := TargetCatalog{LegacyMigrations: map[string]string{legacy: fmt.Sprintf("%x", sha256.Sum256(original))}, Migrations: []string{catalogued}}
	for _, mode := range []string{"later migration", "changed legacy", "missing legacy", "missing catalogued", "uncatalogued history"} {
		t.Run(mode, func(t *testing.T) {
			files := fstest.MapFS{legacy: {Data: original}, catalogued: {Data: []byte("SELECT 1;")}, later: {Data: []byte("CREATE TABLE future (id text);")}}
			switch mode {
			case "changed legacy":
				files[legacy] = &fstest.MapFile{Data: []byte("SELECT 2;")}
			case "missing legacy":
				delete(files, legacy)
			case "missing catalogued":
				delete(files, catalogued)
			case "uncatalogued history":
				files[root+"20261001000000_missing.sql"] = &fstest.MapFile{Data: []byte("SELECT 3;")}
			}
			err := catalog.ValidateMigrations(files, nil)
			if mode == "later migration" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrCatalog) {
				t.Fatalf("expected invalid history: %v", err)
			}
			if mode == "changed legacy" && !strings.Contains(err.Error(), "digest changed") {
				t.Fatalf("digest check missed: %v", err)
			}
		})
	}
}
