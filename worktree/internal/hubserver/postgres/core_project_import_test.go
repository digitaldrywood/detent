package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/attachment"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func coreProjectCopy(t *testing.T, org tracker.OrganizationID, extra ...map[string]any) string {
	t.Helper()
	table := neutralFixtureTable(t, "projects", []neutralimport.Key{{Column: "id", Kind: "text"}})
	states := `[{"name":"Todo","dispatchable":true,"transitions":["Done"]},{"name":"Done","terminal":true,"transitions":["Todo"]}]`
	var records []neutralimport.Row
	for _, id := range []string{"prj_import_a", "prj_import_b"} {
		record := neutralimporttest.Row(t, table, map[string]any{"id": id, "organization_id": org, "name": "Example " + id, "slug": strings.ReplaceAll(id, "_", "-"), "profile": "native", "states_json": states, "created_at": "2026-11-01T00:00:00Z", "workflow_markdown": "example workflow", "content_settings_revision": 7, "integration_revision": 9, "model_selection_revision": 11})
		if id == "prj_import_b" {
			record.Values["archive_completed_after_days"] = storagecatalog.Cell{Null: true}
			record.Values["archive_cancelled_after_days"] = storagecatalog.Cell{Null: true}
		}
		records = append(records, record)
	}
	for _, values := range extra {
		record := map[string]any{"organization_id": org, "name": "Imported " + values["id"].(string), "created_at": "2026-11-01T00:00:00Z", "content_settings_revision": 7, "integration_revision": 9, "model_selection_revision": 11}
		for key, value := range values {
			record[key] = value
		}
		records = append(records, neutralimporttest.Row(t, table, record))
	}
	repositories := neutralFixtureTable(t, "repositories", []neutralimport.Key{{Column: "id", Kind: "integer"}})
	directory, _ := neutralimporttest.Write(t, []byte("historical source"), org, []neutralimport.Table{table, repositories}, map[string][]neutralimport.Row{"projects": records})
	return directory
}

func TestCoreProjectImportPreservesRevisionsAndRejectsChangedCopy(t *testing.T) {
	t.Parallel()
	db := pgstoretest.Template.Database(t)
	for _, org := range []tracker.OrganizationID{"org_import_first", "org_import_second"} {
		t.Run(string(org), func(t *testing.T) {
			repository, _ := pgstoretest.RepositoryFor(t, db, pgstoretest.SmallPool())
			if _, err := db.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,slug,local,created_at) VALUES($1,'Example',$1,false,now())`, org); err != nil {
				t.Fatal(err)
			}
			previous := []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Merging", "Done"}},
				{Name: "Blocked", Transitions: []string{"Todo", "In Progress", "Done"}},
				{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Blocked", "Merging"}},
				{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
			}
			upgraded := []tracker.NativeState{
				{Name: "Triage", Transitions: []string{"Todo", "Backlog", "Cancelled"}},
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done", "Cancelled"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Human Review", "Merging", "Done", "Cancelled"}},
				{Name: "Blocked", Transitions: []string{"Todo", "In Progress", "Done", "Cancelled"}},
				{Name: "Human Review", Transitions: []string{"Done", "In Progress", "Blocked", "Merging", "Cancelled"}},
				{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Blocked", "Human Review", "In Progress", "Cancelled"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
				{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}},
				{Name: "Cancelled", Terminal: true, Transitions: []string{"Todo"}},
			}
			cases := []struct {
				id, profile, workflow, workflowSource string
				cloudUpgrade                          bool
				rawStates                             string
				states, want                          []tracker.NativeState
			}{
				{id: "prj_custom_metadata", profile: "native", rawStates: `[{"name":"Todo","dispatchable":true,"transitions":["In Progress","Done"],"custom":true},{"name":"In Progress","dispatchable":true,"transitions":["Todo","Done"]},{"name":"Done","terminal":true,"transitions":["Todo"]}]`, want: []tracker.NativeState{{Name: "Triage", Transitions: []string{"Todo"}}, {Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}},
				{id: "prj_old_hosted_template", profile: "native", states: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}, want: upgraded},
				{id: "prj_cloud_legacy", profile: "native", workflow: "---\ntracker:\n  kind: hub_native\n  observed_states: [Backlog, Blocked]\n  active_states: [Todo]\n  terminal_states: [Done]\n---\nKeep the Cloud prompt.\n", cloudUpgrade: true, states: []tracker.NativeState{{Name: "Backlog"}, {Name: "Todo", Dispatchable: true}, {Name: "Blocked"}, {Name: "Done", Terminal: true}}, want: []tracker.NativeState{{Name: "Triage", Transitions: []string{"Backlog", "Todo"}}, {Name: "Backlog", Transitions: []string{}}, {Name: "Todo", Dispatchable: true, Transitions: []string{}}, {Name: "Blocked", Transitions: []string{}}, {Name: "Done", Terminal: true, Transitions: []string{}}}},
				{id: "prj_old_defaults", profile: "native", states: previous, want: upgraded},
				{id: "prj_workflow_defaults", profile: "native", workflow: "example workflow", states: previous, want: append([]tracker.NativeState{{Name: "Triage", Transitions: []string{"Todo"}}}, previous...)},
				{id: "prj_repository_defaults", profile: "native", workflowSource: "repository", states: previous, want: append([]tracker.NativeState{{Name: "Triage", Transitions: []string{"Todo"}}}, previous...)},
				{id: "prj_custom", profile: "native", states: []tracker.NativeState{{Name: "Ready", Dispatchable: true, Transitions: []string{"Finished"}}, {Name: "Finished", Terminal: true, Transitions: []string{"Ready"}}}, want: []tracker.NativeState{{Name: "Triage", Transitions: []string{"Ready"}}, {Name: "Ready", Dispatchable: true, Transitions: []string{"Finished"}}, {Name: "Finished", Terminal: true, Transitions: []string{"Ready"}}}},
				{id: "prj_custom_triage", profile: "native", states: []tracker.NativeState{{Name: "Ready", Dispatchable: true, Transitions: []string{"Triage"}}, {Name: "Triage", Terminal: true, Dispatchable: true, OperatorOnly: true, Transitions: []string{"Ready"}}}, want: []tracker.NativeState{{Name: "Ready", Dispatchable: true, Transitions: []string{"Triage"}}, {Name: "Triage", Transitions: []string{"Ready"}}}},
				{id: "prj_terminal_fallback", profile: "native", states: []tracker.NativeState{{Name: "Finished", Terminal: true}, {Name: "Closed", Terminal: true}}, want: []tracker.NativeState{{Name: "Triage", Transitions: []string{"Finished"}}, {Name: "Finished", Terminal: true, Transitions: []string{}}, {Name: "Closed", Terminal: true, Transitions: []string{}}}},
				{id: "prj_non_native", profile: "github_compatible", states: previous, want: previous},
			}
			var extra []map[string]any
			for _, test := range cases {
				raw, err := json.Marshal(test.states)
				if err != nil {
					t.Fatal(err)
				}
				if test.rawStates != "" {
					raw = []byte(test.rawStates)
				}
				extra = append(extra, map[string]any{"id": test.id, "profile": test.profile, "states_json": string(raw), "workflow_markdown": test.workflow, "workflow_source": test.workflowSource})
			}
			source := postgres.CoreProjectImportSource{BundlePath: coreProjectCopy(t, org, extra...), OrganizationID: org, Objects: pgstoretest.NewHistoryObjects()}
			if err := neutralimport.Read(t.Context(), source.BundlePath, org, nil); err != nil {
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
				if err != nil || count != int64(2+len(cases)) {
					t.Fatalf("import count=%d error=%v", count, err)
				}
			}
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			repository, _ = pgstoretest.RepositoryFor(t, db, pgstoretest.SmallPool())
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
				var saved neutralimport.Manifest
				if err := json.Unmarshal(raw, &saved); err != nil {
					return err
				}
				expected := fmt.Sprintf("%x", sha256.Sum256([]byte("historical source")))
				if saved.Identity.SourceSHA256 != expected || saved.Identity.OrganizationID != org || saved.Version != 1 || len(saved.Tables) != 2 || saved.Tables[0].Name != "projects" || saved.Tables[1].Name != "repositories" {
					t.Fatalf("source manifest provenance lost: %+v", saved.Identity)
				}

				archived, err := reader.Receipt(t.Context(), "production-import", "neutral-source-archive", "source")
				if err != nil || archived.RequestHash != receipt.RequestHash {
					return fmt.Errorf("source archive completion missing: %v", err)
				}
				for _, table := range saved.Tables {
					for _, page := range table.Pages {
						pageReceipt, err := reader.Receipt(t.Context(), "production-import", "neutral-source-page", page.SHA256)
						if err != nil {
							return err
						}
						data, err := source.Objects.ReadDetail(t.Context(), string(org), eventstore.Reference(pageReceipt.PayloadObjectKey, pageReceipt.PayloadSHA256, pageReceipt.PayloadBytes, pageReceipt.PayloadContentBytes))
						if err != nil {
							return err
						}
						original, err := os.ReadFile(filepath.Join(source.BundlePath, page.File))
						if err != nil {
							return err
						}
						if string(data) != string(original) || fmt.Sprintf("%x", sha256.Sum256(data)) != page.SHA256 {
							return fmt.Errorf("source page bytes lost for %s", page.File)
						}
					}
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
					wantSlug := map[tracker.ProjectID]string{"prj_import_a": "prj-import-a", "prj_import_b": "prj-import-b"}[id]
					if record.Core.Slug != wantSlug {
						return fmt.Errorf("imported project %s slug=%q want=%q", id, record.Core.Slug, wantSlug)
					}
					if id == "prj_import_b" {
						if record.Settings.ArchiveCompletedAfterDays != nil || record.Settings.ArchiveCancelledAfterDays != nil {
							return fmt.Errorf("imported project lost null archive policy: %+v", record.Settings)
						}
					} else if record.Settings.ArchiveCompletedAfterDays == nil || *record.Settings.ArchiveCompletedAfterDays != 30 || record.Settings.ArchiveCancelledAfterDays == nil || *record.Settings.ArchiveCancelledAfterDays != 7 {
						return fmt.Errorf("imported project lost nullable archive policy: %+v", record.Settings)
					}
					if record.Core.ContentSettingsRevision != 7 || record.Settings.Revision != 9 || record.Settings.ModelRevision != 11 || len(record.States) != 3 || record.Settings.WorkflowKey == "" {
						return fmt.Errorf("imported project lost settings: %+v", record)
					}
					want := []tracker.NativeState{{Name: "Triage", Transitions: []string{"Todo"}}, {Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
					if !reflect.DeepEqual(record.States, want) {
						return fmt.Errorf("custom workflow flags or order changed: %+v", record.States)
					}
					workflow, err := source.Objects.ReadDetail(t.Context(), string(org), eventstore.Reference(record.Settings.WorkflowKey, record.Settings.WorkflowSHA256, record.Settings.WorkflowBytes, record.Settings.WorkflowContentBytes))
					if err != nil || string(workflow) != `"example workflow"` {
						return fmt.Errorf("custom workflow changed: %q: %v", workflow, err)
					}
				}
				for _, test := range cases {
					record, err := reader.ProjectRecord(t.Context(), tracker.ProjectID(test.id))
					if err != nil {
						return err
					}
					if record.Core.Profile != test.profile || record.Core.WorkflowSource != test.workflowSource || !reflect.DeepEqual(record.States, test.want) {
						return fmt.Errorf("historical project %s workflow normalization=%+v", test.id, record)
					}
					workflow, err := source.Objects.ReadDetail(t.Context(), string(org), eventstore.Reference(record.Settings.WorkflowKey, record.Settings.WorkflowSHA256, record.Settings.WorkflowBytes, record.Settings.WorkflowContentBytes))
					var saved string
					if err != nil || json.Unmarshal(workflow, &saved) != nil {
						return fmt.Errorf("historical workflow %s unreadable: %v", test.id, err)
					}
					if test.cloudUpgrade {
						parsed, err := workflowconfig.ParseProjectDefinition(workflowconfig.ProjectDefinitionSources{WorkflowPath: "Cloud WORKFLOW.md", Workflow: []byte(saved)})
						if err != nil || saved == test.workflow || parsed.Prompt != "Keep the Cloud prompt.\n" || !reflect.DeepEqual(parsed.Config.KanbanStateNames(), []string{"Backlog", "Todo", "Blocked", "Done"}) || record.Settings.Revision != 10 {
							return fmt.Errorf("historical Cloud workflow did not migrate exactly once: saved=%q revision=%d: %v", saved, record.Settings.Revision, err)
						}
					} else if saved != test.workflow || record.Settings.Revision != 9 {
						return fmt.Errorf("historical workflow %s changed: %q revision=%d", test.id, saved, record.Settings.Revision)
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
			var manifest neutralimport.Manifest
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
	*pgstoretest.HistoryObjects
	change func()
}

func (s *changingCoreImportObjects) StoreDetail(ctx context.Context, org string, raw []byte) (attachment.ArchiveReference, error) {
	ref, err := s.HistoryObjects.StoreDetail(ctx, org, raw)
	if err == nil && s.change != nil {
		change := s.change
		s.change = nil
		change()
	}
	return ref, err
}

func TestCoreProjectImportRejectsCopyChangedDuringImport(t *testing.T) {
	t.Parallel()
	db := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, db, pgstoretest.SmallPool())
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
	var manifest neutralimport.Manifest
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
	objects := &changingCoreImportObjects{HistoryObjects: pgstoretest.NewHistoryObjects(), change: func() {
		if err := os.Rename(manifestPath, filepath.Join(path, "manifest.json")); err != nil {
			t.Fatal(err)
		}
	}}
	source := postgres.CoreProjectImportSource{BundlePath: path, OrganizationID: org, Objects: objects}
	if _, err := repository.ImportCoreProjects(t.Context(), source); !errors.Is(err, mutation.ErrConflict) {
		t.Fatalf("changed copy completed: %v", err)
	}
}
