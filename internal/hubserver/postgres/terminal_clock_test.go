package postgres_test

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestWorkItemTerminalStreakSurvivesEditsAndRestartsOnReopen(t *testing.T) {
	t.Run("populated restore provenance", func(t *testing.T) {
		db, fixture := migrationDatabaseAtVersion(t, 20261111225811)
		seedCore(t, db)
		if _, err := db.ExecContext(t.Context(), `INSERT INTO project_settings (organization_id,project_id) SELECT organization_id,id FROM projects ON CONFLICT DO NOTHING`); err != nil {
			t.Fatal(err)
		}
		provider, err := migrationProvider(db)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Up(t.Context()); err != nil {
			t.Fatal(err)
		}
		var enabled int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM project_settings WHERE archive_completed_after_days IS NOT NULL OR archive_cancelled_after_days IS NOT NULL`).Scan(&enabled); err != nil || enabled != 0 {
			t.Fatalf("upgrade enabled existing never periods=%d error=%v", enabled, err)
		}
		pool := rolePoolFromTemplate(t, fixture, db, "detent_runtime_v2", "")
		repository, err := postgres.NewRepository(t.Context(), pool, smallPool())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := repository.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			for _, record := range []struct {
				project tracker.ProjectID
				item    tracker.NativeWorkItemID
			}{{"prj_shared", "wi_shared"}, {"prj_other", "wi_other"}} {
				var before time.Time
				if err := db.QueryRowContext(t.Context(), "SELECT clock_timestamp()").Scan(&before); err != nil {
					t.Fatal(err)
				}
				var restored *time.Time
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), record.project, record.item)
					if err != nil {
						return err
					}
					if item.ArchiveRestoreKnown || item.ArchiveRestoredAt != nil {
						t.Fatal("upgrade invented known restore provenance")
					}
					item.ArchiveRestoreKnown = true
					item, err = w.UpdateWorkItem(t.Context(), item)
					if err != nil {
						return err
					}
					if item.ArchiveRestoreKnown || item.ArchiveRestoredAt != nil {
						t.Fatal("ordinary edit invented unknown restore provenance")
					}
					item.Archived = true
					item, err = w.UpdateWorkItem(t.Context(), item)
					if err != nil {
						return err
					}
					item.Archived = false
					item, err = w.UpdateWorkItem(t.Context(), item)
					if err != nil {
						return err
					}
					if !item.ArchiveRestoreKnown || item.ArchiveRestoredAt == nil {
						t.Fatal("genuine restore did not establish known provenance")
					}
					restored = item.ArchiveRestoredAt
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var after time.Time
				if err := db.QueryRowContext(t.Context(), "SELECT clock_timestamp()").Scan(&after); err != nil {
					t.Fatal(err)
				}
				if restored == nil || restored.Before(before) || restored.After(after) {
					t.Fatalf("default constructor restore clock=%v outside PostgreSQL [%v,%v]", restored, before, after)
				}
			}
		}
	})

	for _, constructor := range []string{"Open", "NewRepository"} {
		t.Run(constructor, func(t *testing.T) {
			admin := template.Database(t)
			pool := rolePool(t, admin, "detent_runtime_v2", "")
			now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
			var repository *postgres.Repository
			var err error
			if constructor == "Open" {
				connection := pool.Config().ConnConfig
				pool.Close()
				repository, err = postgres.Open(t.Context(), postgres.Config{
					Connection: template.Connection(t, admin, connection.User, connection.Password), Pool: smallPool(),
					LeaseClock: func() time.Time { return now },
				}, os.DirFS("migrations"))
			} else {
				repository, err = postgres.NewRepository(t.Context(), pool, smallPool(), func() time.Time { return now })
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := repository.Close(); err != nil {
					t.Error(err)
				}
			})
			started := now
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if err := seedCoreOrganization(t.Context(), w, org); err != nil {
						return err
					}
					_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{
						Core:   corestore.Project{OrganizationID: org, ID: "prj_shared", Name: "Example", Profile: "native"},
						States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}, {Name: "Done", Terminal: true}, {Name: "Cancelled", Terminal: true}},
					})
					if err != nil {
						return err
					}
					for i, state := range []string{"Todo", "Done"} {
						item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_shared", NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%d", i)), Number: int64(i + 1), Title: "Example", WorkflowStateName: new(state), TerminalEnteredAt: new(started.Add(-100 * time.Hour)), ArchiveRestoredAt: new(started.Add(-100 * time.Hour))})
						if err != nil {
							return err
						}
						if !item.ArchiveRestoreKnown {
							t.Fatal("current creation lost known no-restore provenance")
						}
						if item.ArchiveRestoredAt != nil {
							t.Fatal("current creation trusted a caller restore timestamp")
						}
						if i == 0 && item.TerminalEnteredAt != nil {
							t.Errorf("%s nonterminal creation received a terminal clock", org)
						}
						if i == 1 && (item.TerminalEnteredAt == nil || !item.TerminalEnteredAt.Equal(started)) {
							t.Errorf("%s terminal creation trusted caller clock: %v", org, item.TerminalEnteredAt)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, step := range []struct {
				state    string
				hours    int
				expected *time.Time
			}{
				{"Done", 1, new(started.Add(time.Hour))},
				{"Cancelled", 2, new(started.Add(time.Hour))},
				{"Cancelled", 100, new(started.Add(time.Hour))},
				{"Todo", 101, nil},
				{"Done", 102, new(started.Add(102 * time.Hour))},
			} {
				now = started.Add(time.Duration(step.hours) * time.Hour)
				for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						item, err := w.WorkItem(t.Context(), "prj_shared", "wi_0")
						if err != nil {
							return err
						}
						item.WorkflowStateName = new(step.state)
						item.TerminalEnteredAt = new(started.Add(-100 * time.Hour))
						item.Title = fmt.Sprintf("Example %d", step.hours)
						item, err = w.UpdateWorkItem(t.Context(), item)
						if err != nil {
							return err
						}
						if !item.UpdatedAt.Equal(now) || !item.NativeUpdatedAt.Equal(now) {
							t.Errorf("%s update clocks=%s/%s want=%s", org, item.UpdatedAt, item.NativeUpdatedAt, now)
						}
						if step.expected == nil {
							if item.TerminalEnteredAt != nil {
								t.Errorf("%s reopen retained clock %v", org, item.TerminalEnteredAt)
							}
						} else if item.TerminalEnteredAt == nil || !item.TerminalEnteredAt.Equal(*step.expected) {
							t.Errorf("%s %s clock=%v want=%v", org, step.state, item.TerminalEnteredAt, step.expected)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE issues SET terminal_entered_at=NULL WHERE native_id='wi_1'`); err != nil {
				t.Fatal(err)
			}
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					item, err := w.WorkItem(t.Context(), "prj_shared", "wi_1")
					if err != nil {
						return err
					}
					item.WorkflowStateName = new("Cancelled")
					item, err = w.UpdateWorkItem(t.Context(), item)
					if err == nil && item.TerminalEnteredAt != nil {
						t.Errorf("%s unknown historical terminal clock was invented: %v", org, item.TerminalEnteredAt)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				for _, id := range []tracker.NativeWorkItemID{"wi_0", "wi_1"} {
					var terminal *time.Time
					for _, step := range []struct {
						hours    int
						archived bool
						restored *time.Time
					}{{200, true, nil}, {201, false, new(started.Add(201 * time.Hour))}, {202, false, new(started.Add(201 * time.Hour))}} {
						now = started.Add(time.Duration(step.hours) * time.Hour)
						if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
							item, err := w.WorkItem(t.Context(), "prj_shared", id)
							if err != nil {
								return err
							}
							if step.hours == 200 {
								terminal = item.TerminalEnteredAt
							}
							item.Archived = step.archived
							item.ArchiveRestoreKnown = false
							item.ArchiveRestoredAt = new(started.Add(-200 * time.Hour))
							item, err = w.UpdateWorkItem(t.Context(), item)
							if err != nil {
								return err
							}
							if !item.ArchiveRestoreKnown {
								t.Fatal("current mutation trusted caller restore provenance")
							}
							if step.restored == nil {
								if item.ArchiveRestoredAt != nil {
									t.Fatalf("archive invented restore timestamp: %v", item.ArchiveRestoredAt)
								}
							} else if item.ArchiveRestoredAt == nil || !item.ArchiveRestoredAt.Equal(*step.restored) {
								t.Fatalf("restore timestamp=%v want=%v", item.ArchiveRestoredAt, step.restored)
							}
							if terminal == nil {
								if item.TerminalEnteredAt != nil {
									t.Fatal("restore invented a terminal streak")
								}
							} else if item.TerminalEnteredAt == nil || !item.TerminalEnteredAt.Equal(*terminal) {
								t.Fatal("restore changed the terminal streak")
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
		})
	}
}
