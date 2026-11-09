package hubserver

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func TestCoreChangeImportsRefuseLegacyCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "historical-source.db")
	original := []byte("preserved verified historical source")
	if err := os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"changes", "landings", "escapes", "usage", "tenant"} {
		for _, bundle := range []string{"", "unused-neutral-bundle"} {
			var err error
			switch family {
			case "changes":
				_, err = ImportCoreChanges(t.Context(), nil, CoreChangeImportSource{CopyPath: path, BundlePath: bundle})
			case "landings":
				_, err = ImportCoreReportLandings(t.Context(), nil, CoreChangeImportSource{CopyPath: path, BundlePath: bundle})
			case "escapes":
				_, err = ImportCoreReportEscapes(t.Context(), nil, CoreChangeImportSource{CopyPath: path, BundlePath: bundle})
			case "usage":
				_, err = ImportCoreReportUsage(t.Context(), nil, ReportUsageImportSource{CopyPath: path, BundlePath: bundle})
			case "tenant":
				_, err = ImportTenantCoreCopy(t.Context(), postgres.Config{}, TenantCoreImportSource{CopyPath: path, BundlePath: bundle})
			}
			if !errors.Is(err, postgres.ErrNeutralImport) {
				t.Fatalf("%s legacy selector error=%v", family, err)
			}
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("historical source changed: %v", err)
	}
}

func TestCoreChangeImportsRejectUnsupportedSchemaBeforePin(t *testing.T) {
	repository, admin := coreImportPostgresWithAdmin(t, 3)
	for _, family := range []string{"changes", "landings", "escapes", "usage"} {
		t.Run(family, func(t *testing.T) {
			descriptor := hubTestPolicy()
			policyJSON, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			fixture := coreChangeNeutralFixture(t, "org_change_schema", "prj_example", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), descriptor, policyJSON)
			tableName := "change_versions"
			if family == "usage" {
				fixture = coreExecutionNeutralFixture(t, "org_change_schema")
				tableName = "native_attempts"
			}
			table := fixture.tables[tableName]
			table.Columns["unaccounted"] = fmt.Sprintf(`[%d,"unaccounted","TEXT",1,null,0,0]`, len(table.Columns))
			for _, row := range fixture.records[tableName] {
				row.Values["unaccounted"] = storagecatalog.Cell{Encoding: "utf8", Data: []byte("must survive migration")}
			}
			fixture.tables[tableName] = table
			objects := &memoryRunDetails{}
			path := fixture.write()
			switch family {
			case "changes":
				_, err = ImportCoreChanges(t.Context(), repository, CoreChangeImportSource{BundlePath: path, OrganizationID: fixture.organization, SourceObjects: objects, Objects: objects})
			case "landings":
				_, err = ImportCoreReportLandings(t.Context(), repository, CoreChangeImportSource{BundlePath: path, OrganizationID: fixture.organization, SourceObjects: objects, Objects: objects})
			case "escapes":
				_, err = ImportCoreReportEscapes(t.Context(), repository, CoreChangeImportSource{BundlePath: path, OrganizationID: fixture.organization, SourceObjects: objects, Objects: objects})
			case "usage":
				_, err = ImportCoreReportUsage(t.Context(), repository, ReportUsageImportSource{BundlePath: path, OrganizationID: fixture.organization, ExpectedSHA256: strings.Repeat("0", 64)})
			}
			if !errors.Is(err, storagecatalog.ErrUnmapped) {
				t.Fatalf("unsupported schema error=%v", err)
			}
			var count int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM neutral_import_sources").Scan(&count); err != nil || count != 0 {
				t.Fatalf("source pinned before schema validation: count=%d error=%v", count, err)
			}
		})
	}
}

func TestCoreNeutralObjectRecoversOnlyVerifiedPendingBytes(t *testing.T) {
	payload := []byte{0, 1, 0xff, 'x'}
	reference, _, err := attachment.PrepareArchive("org_pending", 1, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"binary", "base64", "hex", "changed", "missing", "invalid hex", "wrong reference", "wrong size"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newCoreNeutralFixture(t, "org_pending", "change_content_objects", "attachments")
			digest := reference.SHA256
			if scenario == "wrong reference" {
				digest = strings.Repeat("0", 64)
			}
			fixture.add("change_content_objects", map[string]any{"object_key": reference.Key, "object_sha256": digest, "object_bytes": reference.CompressedSize, "content_bytes": reference.UncompressedSize, "pending_content": payload})
			if scenario == "wrong size" {
				fixture.set("change_content_objects", nil, map[string]any{"object_bytes": reference.CompressedSize + 1})
			}
			row := fixture.records["change_content_objects"][0]
			switch scenario {
			case "base64":
				row.Values["pending_content"] = storagecatalog.Cell{Encoding: "base64", Data: []byte(base64.StdEncoding.EncodeToString(payload))}
			case "hex":
				row.Values["pending_content"] = storagecatalog.Cell{Encoding: "hex", Data: []byte(hex.EncodeToString(payload))}
			case "changed":
				row.Values["pending_content"] = storagecatalog.Cell{Encoding: "binary", Data: []byte("changed source")}
			case "missing":
				row.Values["pending_content"] = storagecatalog.Cell{Null: true}
			case "invalid hex":
				row.Values["pending_content"] = storagecatalog.Cell{Encoding: "hex", Data: []byte("zz")}
			}
			err := postgres.ReadNeutralImportBundle(t.Context(), fixture.write(), fixture.organization, func(bundle *postgres.VerifiedNeutralImport) error {
				actual, err := readCoreNeutralObject(t.Context(), newCoreNeutralImport(bundle), fixture.organization, &memoryRunDetails{}, reference.Key)
				switch scenario {
				case "binary", "base64", "hex":
					if err != nil || !bytes.Equal(actual, payload) {
						t.Fatalf("verified pending bytes=%v error=%v", actual, err)
					}
				case "missing":
					if !errors.Is(err, artifact.ErrMissing) {
						t.Fatalf("missing pending bytes error=%v", err)
					}
				case "invalid hex":
					if !errors.Is(err, postgres.ErrNeutralImport) {
						t.Fatalf("invalid encoding error=%v", err)
					}
				default:
					if err == nil {
						t.Fatal("invalid pending reference accepted")
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCoreNeutralUsageRefusesInvalidSourceBeforeWriting(t *testing.T) {
	repository, admin := coreImportPostgresWithAdmin(t, 3)
	for _, scenario := range []string{"digest", "attempt scope", "currency"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := coreExecutionNeutralFixture(t, "org_usage_refusal")
			switch scenario {
			case "attempt scope":
				fixture.set("native_attempts", map[string]string{"id": "attempt_execution_import_second"}, map[string]any{"project_id": "prj_other"})
			case "currency":
				fixture.set("attempt_usage", map[string]string{"attempt_id": "attempt_execution_import_second"}, map[string]any{"currency": "EUR"})
			}
			path := fixture.write()
			var identity postgres.NeutralImportIdentity
			if err := postgres.ReadNeutralImportBundle(t.Context(), path, fixture.organization, func(bundle *postgres.VerifiedNeutralImport) error { identity = bundle.Identity(); return nil }); err != nil {
				t.Fatal(err)
			}
			expected := identity.SourceSHA256
			phrase := ""
			switch scenario {
			case "digest":
				expected = strings.Repeat("0", 64)
				phrase = "source digest mismatch"
			case "attempt scope":
				phrase = "invalid attempt scope"
			case "currency":
				phrase = "unsupported currency"
			}
			_, err := ImportCoreReportUsage(t.Context(), repository, ReportUsageImportSource{BundlePath: path, OrganizationID: fixture.organization, ExpectedSHA256: expected})
			if err == nil || !strings.Contains(err.Error(), phrase) {
				t.Fatalf("source refusal error=%v", err)
			}
			var count int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM attempt_usage").Scan(&count); err != nil || count != 0 {
				t.Fatalf("usage written before full-source validation: count=%d error=%v", count, err)
			}
		})
	}
}
