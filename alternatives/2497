package postgres_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func coreProjectCopy(t *testing.T, org tracker.OrganizationID) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	catalog, err := storagecatalog.Read(os.DirFS("../../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for name, column := range catalog.Tables["hub/projects"].Columns {
		var metadata []json.RawMessage
		if err := json.Unmarshal([]byte(column.Source), &metadata); err != nil {
			t.Fatal(err)
		}
		var kind string
		var defaultSQL *string
		if err := json.Unmarshal(metadata[2], &kind); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(metadata[4], &defaultSQL); err != nil {
			t.Fatal(err)
		}
		definition := `"` + name + `" ` + kind
		if defaultSQL != nil {
			definition += " DEFAULT (" + *defaultSQL + ")"
		}
		columns = append(columns, definition)
	}
	for _, query := range []string{"CREATE TABLE projects (" + strings.Join(columns, ",") + ")", `CREATE TABLE repositories(id INTEGER PRIMARY KEY,github_owner TEXT,github_name TEXT)`} {
		if _, err := db.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	states := `[{"name":"Todo","dispatchable":true,"transitions":["Done"]},{"name":"Done","terminal":true,"transitions":["Todo"]}]`
	for _, id := range []string{"prj_import_a", "prj_import_b"} {
		_, err := db.ExecContext(t.Context(), `INSERT INTO projects(id,organization_id,name,slug,profile,states_json,created_at,workflow_markdown,content_settings_revision,integration_revision,model_selection_revision) VALUES(?,?,?,?,'native',?,'2026-11-01T00:00:00Z','example workflow',7,9,11)`, id, org, "Example "+id, strings.ReplaceAll(id, "_", "-"), states)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCoreProjectImportPreservesRevisionsAndRejectsChangedCopy(t *testing.T) {
	db := template.Database(t)
	repository, _ := repositoryFor(t, db, smallPool())
	for _, org := range []tracker.OrganizationID{"org_import_first", "org_import_second"} {
		t.Run(string(org), func(t *testing.T) {
			if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,slug,local,created_at) VALUES($1,'Example',$1,false,now())`, org); err != nil {
				t.Fatal(err)
			}
			source := postgres.CoreProjectImportSource{CopyPath: coreProjectCopy(t, org), OrganizationID: org, Objects: newHistoryObjects()}
			for range 2 {
				count, err := repository.ImportCoreProjects(t.Context(), source)
				if err != nil || count != 2 {
					t.Fatalf("import count=%d error=%v", count, err)
				}
			}
			err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
				for _, id := range []tracker.ProjectID{"prj_import_a", "prj_import_b"} {
					record, err := reader.ProjectRecord(t.Context(), id)
					if err != nil {
						return err
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
			bytes, err := os.ReadFile(source.CopyPath)
			if err != nil {
				t.Fatal(err)
			}
			changedPath := filepath.Join(t.TempDir(), "changed.db")
			if err := os.WriteFile(changedPath, bytes, 0600); err != nil {
				t.Fatal(err)
			}
			changed, err := sql.Open("sqlite", changedPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := changed.ExecContext(t.Context(), `DELETE FROM projects WHERE id='prj_import_b'`); err != nil {
				t.Fatal(err)
			}
			if err := changed.Close(); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(changedPath, 0400); err != nil {
				t.Fatal(err)
			}
			source.CopyPath = changedPath
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
	if err := os.Chmod(replacement, 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := sql.Open("sqlite", replacement)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := changed.ExecContext(t.Context(), `CREATE TABLE copied_metadata(value TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := changed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(replacement, 0400); err != nil {
		t.Fatal(err)
	}
	objects := &changingCoreImportObjects{historyObjects: newHistoryObjects(), change: func() {
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
	}}
	source := postgres.CoreProjectImportSource{CopyPath: path, OrganizationID: org, Objects: objects}
	if _, err := repository.ImportCoreProjects(t.Context(), source); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed copy completed: %v", err)
	}
}
