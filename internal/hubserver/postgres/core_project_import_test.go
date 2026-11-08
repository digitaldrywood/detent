package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func coreProjectCopy(t *testing.T, org tracker.OrganizationID) string {
	t.Helper()
	table := neutralFixtureTable(t, "projects", []postgres.NeutralImportKey{{Column: "id", Kind: "text"}})
	states := `[{"name":"Todo","dispatchable":true,"transitions":["Done"]},{"name":"Done","terminal":true,"transitions":["Todo"]}]`
	var records []postgres.NeutralImportRow
	for _, id := range []string{"prj_import_a", "prj_import_b"} {
		record := neutralFixtureRow(t, table, map[string]any{"id": id, "organization_id": org, "name": "Example " + id, "slug": strings.ReplaceAll(id, "_", "-"), "profile": "native", "states_json": states, "created_at": "2026-11-01T00:00:00Z", "workflow_markdown": "example workflow", "content_settings_revision": 7, "integration_revision": 9, "model_selection_revision": 11})
		if id == "prj_import_b" {
			record.Values["archive_completed_after_days"] = storagecatalog.Cell{Null: true}
			record.Values["archive_cancelled_after_days"] = storagecatalog.Cell{Null: true}
		}
		records = append(records, record)
	}
	repositories := neutralFixtureTable(t, "repositories", []postgres.NeutralImportKey{{Column: "id", Kind: "integer"}})
	directory, _ := writeNeutralFixture(t, org, []postgres.NeutralImportTable{table, repositories}, map[string][]postgres.NeutralImportRow{"projects": records})
	return directory
}

func TestCoreProjectImportPreservesRevisionsAndRejectsChangedCopy(t *testing.T) {
	db := template.Database(t)
	for _, org := range []tracker.OrganizationID{"org_import_first", "org_import_second"} {
		t.Run(string(org), func(t *testing.T) {
			repository, _ := repositoryFor(t, db, smallPool())
			if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,slug,local,created_at) VALUES($1,'Example',$1,false,now())`, org); err != nil {
				t.Fatal(err)
			}
			source := postgres.CoreProjectImportSource{BundlePath: coreProjectCopy(t, org), OrganizationID: org, Objects: newHistoryObjects()}
			if err := postgres.ReadNeutralImportBundle(t.Context(), source.BundlePath, org, nil); err != nil {
				t.Fatal(err)
			}
			var dryRunProjects int
			if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM projects WHERE organization_id=$1", org).Scan(&dryRunProjects); err != nil || dryRunProjects != 0 {
				t.Fatalf("dry run wrote projects=%d: %v", dryRunProjects, err)
			}
			historicalChecksum := fmt.Sprintf("%x", sha256.Sum256([]byte("historical source")))
			if err := repository.PinCoreImportSource(t.Context(), org, historicalChecksum, source.Objects); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				count, err := repository.ImportCoreProjects(t.Context(), source)
				if err != nil || count != 2 {
					t.Fatalf("import count=%d error=%v", count, err)
				}
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			repository, _ = repositoryFor(t, db, smallPool())
			manifestRaw, err := os.ReadFile(filepath.Join(source.BundlePath, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadCutover(t.Context(), org, corestore.Serving, func(reader postgres.CutoverReader) error {
				pin, err := reader.NeutralImportSource(t.Context(), postgres.NeutralImportSourceInput{OrganizationID: org, SourceID: "example-source"})
				if err != nil {
					return err
				}
				expected := fmt.Sprintf("%x", sha256.Sum256(manifestRaw))
				if pin.Checksum != expected || pin.SchemaVersion != 1 || pin.SchemaDigest == "" {
					t.Fatalf("neutral source provenance=%+v", pin)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadRecords(t.Context(), org, corestore.Serving, func(reader eventstore.Reader) error {
				receipt, err := reader.Receipt(t.Context(), "production-import", "neutral-source", "source")
				if err != nil {
					return err
				}
				raw, err := eventstore.NewContent(source.Objects).Receipt(t.Context(), receipt)
				if err != nil {
					return err
				}
				var saved postgres.NeutralImportManifest
				if err := json.Unmarshal(raw, &saved); err != nil {
					return err
				}
				expected := fmt.Sprintf("%x", sha256.Sum256([]byte("historical source")))
				if saved.Identity.SourceSHA256 != expected || saved.Identity.OrganizationID != org || saved.Version != 1 || len(saved.Tables) != 2 || saved.Tables[0].Name != "projects" || saved.Tables[1].Name != "repositories" {
					t.Fatalf("source manifest provenance lost: %+v", saved.Identity)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			err = repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				for _, id := range []tracker.ProjectID{"prj_import_a", "prj_import_b"} {
					record, err := reader.ProjectRecord(t.Context(), id)
					if err != nil {
						return err
					}
					if id == "prj_import_b" {
						if record.Settings.ArchiveCompletedAfterDays != nil || record.Settings.ArchiveCancelledAfterDays != nil {
							return fmt.Errorf("imported project lost null archive policy: %+v", record.Settings)
						}
					} else if record.Settings.ArchiveCompletedAfterDays == nil || *record.Settings.ArchiveCompletedAfterDays != 30 || record.Settings.ArchiveCancelledAfterDays == nil || *record.Settings.ArchiveCancelledAfterDays != 7 {
						return fmt.Errorf("imported project lost nullable archive policy: %+v", record.Settings)
					}
					if record.Core.ContentSettingsRevision != 7 || record.Settings.Revision != 9 || record.Settings.ModelRevision != 11 || len(record.States) != 2 || record.Settings.WorkflowKey == "" {
						return fmt.Errorf("imported project lost settings: %+v", record)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			changedPath := coreProjectCopy(t, org)
			manifestPath := filepath.Join(changedPath, "manifest.json")
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			var manifest postgres.NeutralImportManifest
			if err := json.Unmarshal(raw, &manifest); err != nil {
				t.Fatal(err)
			}
			manifest.Identity.SourceID = "changed-source"
			raw, err = json.Marshal(manifest)
			if err != nil {
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
			source.BundlePath = changedPath
			if _, err := repository.ImportCoreProjects(t.Context(), source); !errors.Is(err, mutation.ErrConflict) {
				t.Fatalf("different actual copy error=%v", err)
			}

		})
	}
}

type changingCoreImportObjects struct {
	*historyObjects
	change func()
}

func (s *changingCoreImportObjects) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	ref, err := s.historyObjects.StoreDetail(ctx, org, raw)
	if err == nil && s.change != nil {
		change := s.change
		s.change = nil
		change()
	}
	return ref, err
}

func TestCoreProjectImportRejectsCopyChangedDuringImport(t *testing.T) {
	db := template.Database(t)
	repository, _ := repositoryFor(t, db, smallPool())
	org := tracker.OrganizationID("org_import_mutated")
	if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,slug,local,created_at) VALUES($1,'Example',$1,false,now())`, org); err != nil {
		t.Fatal(err)
	}
	path := coreProjectCopy(t, org)
	replacement := coreProjectCopy(t, org)
	manifestPath := filepath.Join(replacement, "manifest.json")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest postgres.NeutralImportManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Identity.SourceID = "changed-during-import"
	raw, err = json.Marshal(manifest)
	if err != nil {
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
	objects := &changingCoreImportObjects{historyObjects: newHistoryObjects(), change: func() {
		if err := os.Rename(manifestPath, filepath.Join(path, "manifest.json")); err != nil {
			t.Fatal(err)
		}
	}}
	source := postgres.CoreProjectImportSource{BundlePath: path, OrganizationID: org, Objects: objects}
	if _, err := repository.ImportCoreProjects(t.Context(), source); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed copy completed: %v", err)
	}
}
