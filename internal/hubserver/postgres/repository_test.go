package postgres_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func repositoryFor(t *testing.T, admin *sql.DB, config postgres.PoolConfig) (*postgres.Repository, *sql.DB) {
	t.Helper()
	var database string
	if err := admin.QueryRowContext(t.Context(), `SELECT current_database()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), "ALTER DATABASE "+pq.QuoteIdentifier(database)+fmt.Sprintf(" CONNECTION LIMIT %d", config.MaxConnections+1)); err != nil {
		t.Fatal(err)
	}
	pool := rolePool(t, admin, "detent_runtime", "")
	repository, err := postgres.NewRepository(t.Context(), pool, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository, pool
}

func smallPool() postgres.PoolConfig {
	return postgres.PoolConfig{MaxConnections: 2, ServingConnections: 1, BackgroundConnections: 1}
}

func seedRepository(t *testing.T, repository corestore.Repository, organization tracker.OrganizationID) {
	t.Helper()
	err := repository.Write(t.Context(), organization, corestore.Serving, func(w corestore.Writer) error {
		for index, suffix := range []string{"first", "second"} {
			want := corestore.Project{OrganizationID: organization, ID: tracker.ProjectID("prj_" + suffix), Name: suffix, Slug: suffix, Profile: "native", RequireDependencies: true, SchedulingRank: int64(index), WorkflowSource: "example", WorkflowSourceRevision: "source-revision", WorkflowObjectKey: new("example/workflow"), StatesObjectKey: new("example/states"), ModelSelectionObjectKey: new("example/models")}
			got, err := w.CreateProject(t.Context(), want)
			if err != nil {
				return err
			}
			if got.CreatedAt.IsZero() {
				t.Error("project creation time is missing")
			}
			want.CreatedAt = got.CreatedAt
			if !reflect.DeepEqual(got, want) {
				t.Errorf("created project = %+v, want %+v", got, want)
			}
			for number := int64(1); number <= 2; number++ {
				item := corestore.WorkItem{OrganizationID: organization, NativeID: tracker.NativeWorkItemID(fmt.Sprintf("wi_%s_%d", suffix, number)), ProjectID: got.ID, Number: number, NativeSourceKey: new(fmt.Sprintf("source_%d", number)), Title: "Example title", BodyObjectKey: new("example/body"), IssueContractObjectKey: new("example/contract"), ReportContextObjectKey: new("example/report"), ActorKind: new("human"), ActorPrincipalID: new("example"), ProvenanceProvider: new("example"), ProvenanceExternalID: new("external")}
				created, err := w.CreateWorkItem(t.Context(), item)
				if err != nil {
					return err
				}
				if created.ID <= 0 || created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() || created.NativeCreatedAt.IsZero() || created.NativeUpdatedAt.IsZero() {
					t.Error("work item generated metadata is missing")
				}
				item.LastActivityAt = created.LastActivityAt
				item.ID, item.Revision = created.ID, 1
				item.CreatedAt, item.UpdatedAt, item.NativeCreatedAt, item.NativeUpdatedAt = created.CreatedAt, created.UpdatedAt, created.NativeCreatedAt, created.NativeUpdatedAt
				if !reflect.DeepEqual(created, item) {
					t.Errorf("created work item = %+v, want %+v", created, item)
				}
				for sequence := int64(1); sequence <= 2; sequence++ {
					comment := corestore.Comment{OrganizationID: organization, ID: fmt.Sprintf("comment_%s_%d_%d", suffix, number, sequence), ProjectID: got.ID, WorkItemID: created.NativeID, Sequence: sequence, BodyObjectKey: "example/comment", ActorKind: "human", ActorPrincipalID: "example", EditedByKind: new("human"), EditedByPrincipalID: new("editor"), ProvenanceProvider: new("example"), ProvenanceExternalID: new("external"), SourceKey: new(fmt.Sprintf("source_%d", sequence))}
					stored, err := w.CreateComment(t.Context(), comment)
					if err != nil {
						return err
					}
					if stored.CreatedAt.IsZero() || stored.UpdatedAt.IsZero() {
						t.Error("comment generated metadata is missing")
					}
					comment.Revision, comment.CreatedAt, comment.UpdatedAt, comment.RecordID = 1, stored.CreatedAt, stored.UpdatedAt, stored.RecordID
					if !reflect.DeepEqual(stored, comment) {
						t.Errorf("created comment = %+v, want %+v", stored, comment)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestInsertsNeverWriteConstantUniqueKeys(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		seedRepository(t, repository, organization)
		t.Run(string(organization), func(t *testing.T) {
			err := repository.Read(t.Context(), organization, corestore.Serving, func(r corestore.Reader) error {
				for _, suffix := range []string{"first", "second"} {
					project, err := r.Project(t.Context(), tracker.ProjectID("prj_"+suffix))
					if err != nil {
						return err
					}
					if project.OrganizationID != organization || project.Name != suffix || project.WorkflowObjectKey == nil || *project.WorkflowObjectKey != "example/workflow" {
						t.Fatalf("project fields = %+v", project)
					}
					page, err := r.WorkItemPage(t.Context(), project.ID, corestore.Cursor{}, 2)
					if err != nil {
						return err
					}
					if len(page.Items) != 2 {
						t.Fatalf("work item page has %d rows, want two", len(page.Items))
					}
					for _, row := range page.Items {
						if row.OrganizationID != organization || row.ProjectID != project.ID {
							t.Fatalf("work item page escaped scope: %+v", row)
						}
						comments, err := r.CommentPage(t.Context(), project.ID, row.NativeID, corestore.Cursor{}, 2)
						if err != nil {
							return err
						}
						if len(comments.Items) != 2 {
							t.Fatalf("comment page has %d rows, want two", len(comments.Items))
						}
						for _, comment := range comments.Items {
							if comment.OrganizationID != organization || comment.ProjectID != project.ID || comment.WorkItemID != row.NativeID {
								t.Fatalf("comment page escaped scope: %+v", comment)
							}
						}
					}
					for number := int64(1); number <= 2; number++ {
						itemID := tracker.NativeWorkItemID(fmt.Sprintf("wi_%s_%d", suffix, number))
						item, err := r.WorkItem(t.Context(), project.ID, itemID)
						if err != nil {
							return err
						}
						if item.OrganizationID != organization || item.Number != number || item.Title != "Example title" || item.BodyObjectKey == nil || *item.BodyObjectKey != "example/body" {
							t.Fatalf("work item fields = %+v", item)
						}
						for sequence := int64(1); sequence <= 2; sequence++ {
							comment, err := r.Comment(t.Context(), project.ID, itemID, fmt.Sprintf("comment_%s_%d_%d", suffix, number, sequence))
							if err != nil {
								return err
							}
							if comment.OrganizationID != organization || comment.Sequence != sequence || comment.BodyObjectKey != "example/comment" {
								t.Fatalf("comment fields = %+v", comment)
							}
						}
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

func TestRepositoryUpdatesAndDeletes(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	seedRepository(t, repository, "org_first")
	for _, tc := range []struct {
		name   string
		change func(corestore.Writer) error
		check  func(corestore.Reader) error
	}{
		{"project", func(w corestore.Writer) error {
			p, err := w.Project(t.Context(), "prj_second")
			if err != nil {
				return err
			}
			p.Name, p.DeletedAt, p.WorkflowObjectKey = "Changed project", new(p.CreatedAt), nil
			got, err := w.UpdateProject(t.Context(), p)
			if err == nil && (got.Name != "Changed project" || got.ContentSettingsRevision != 1 || got.DeletedAt == nil || got.WorkflowObjectKey != nil) {
				t.Errorf("updated project = %+v", got)
			}
			return err
		}, func(r corestore.Reader) error {
			p, err := r.Project(t.Context(), "prj_second")
			if err == nil && (p.Name != "Changed project" || p.ContentSettingsRevision != 1 || p.DeletedAt == nil || p.WorkflowObjectKey != nil) {
				t.Errorf("persisted project = %+v", p)
			}
			return err
		}},
		{"work item", func(w corestore.Writer) error {
			p, err := w.WorkItem(t.Context(), "prj_first", "wi_first_2")
			if err != nil {
				return err
			}
			p.Title, p.Archived, p.BodyObjectKey = "Changed item", true, nil
			got, err := w.UpdateWorkItem(t.Context(), p)
			if err == nil && (got.Title != "Changed item" || got.Revision != 2 || !got.Archived || got.BodyObjectKey != nil) {
				t.Errorf("updated work item = %+v", got)
			}
			return err
		}, func(r corestore.Reader) error {
			p, err := r.WorkItem(t.Context(), "prj_first", "wi_first_2")
			if err == nil && (p.Title != "Changed item" || p.Revision != 2 || !p.Archived || p.BodyObjectKey != nil) {
				t.Errorf("persisted work item = %+v", p)
			}
			return err
		}},
		{"comment", func(w corestore.Writer) error {
			p, err := w.Comment(t.Context(), "prj_first", "wi_first_1", "comment_first_1_2")
			if err != nil {
				return err
			}
			p.BodyObjectKey, p.Deleted, p.EditedByKind = "example/changed", true, nil
			got, err := w.UpdateComment(t.Context(), p)
			if err == nil && (got.BodyObjectKey != "example/changed" || got.Revision != 2 || !got.Deleted || got.EditedByKind != nil) {
				t.Errorf("updated comment = %+v", got)
			}
			return err
		}, func(r corestore.Reader) error {
			p, err := r.Comment(t.Context(), "prj_first", "wi_first_1", "comment_first_1_2")
			if err == nil && (p.BodyObjectKey != "example/changed" || p.Revision != 2 || !p.Deleted || p.EditedByKind != nil) {
				t.Errorf("persisted comment = %+v", p)
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := repository.Write(t.Context(), "org_first", corestore.Serving, tc.change); err != nil {
				t.Fatal(err)
			}
			if err := repository.Read(t.Context(), "org_first", corestore.Background, tc.check); err != nil {
				t.Fatal(err)
			}
		})
	}
	err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			suffix := strings.TrimPrefix(string(project), "prj_")
			for number := 1; number <= 2; number++ {
				item := tracker.NativeWorkItemID(fmt.Sprintf("wi_%s_%d", suffix, number))
				for sequence := 1; sequence <= 2; sequence++ {
					if err := w.DeleteComment(t.Context(), project, item, fmt.Sprintf("comment_%s_%d_%d", suffix, number, sequence)); err != nil {
						return err
					}
				}
				if err := w.DeleteWorkItem(t.Context(), project, item); err != nil {
					return err
				}
			}
			if err := w.DeleteProject(t.Context(), project); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
		rows, err := r.Projects(t.Context(), "", 2)
		if len(rows) != 0 {
			t.Errorf("deleted projects still readable: %+v", rows)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRepositoryPages(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	seedRepository(t, repository, "org_first")
	seedRepository(t, repository, "org_second")
	for _, tc := range []struct {
		name string
		page func(corestore.Reader, int) ([]string, error)
	}{
		{"projects", func(r corestore.Reader, limit int) ([]string, error) {
			rows, err := r.Projects(t.Context(), "prj_first", limit)
			ids := make([]string, len(rows))
			for i, row := range rows {
				ids[i] = string(row.ID)
			}
			return ids, err
		}},
		{"work items", func(r corestore.Reader, limit int) ([]string, error) {
			rows, err := r.WorkItems(t.Context(), "prj_first", 1, limit)
			ids := make([]string, len(rows))
			for i, row := range rows {
				ids[i] = string(row.NativeID)
			}
			return ids, err
		}},
		{"comments", func(r corestore.Reader, limit int) ([]string, error) {
			rows, err := r.Comments(t.Context(), "prj_first", "wi_first_1", 1, limit)
			ids := make([]string, len(rows))
			for i, row := range rows {
				ids[i] = row.ID
			}
			return ids, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, limit := range []int{-1, 0, 1, corestore.MaxPageSize, corestore.MaxPageSize + 1} {
				err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
					ids, err := tc.page(r, limit)
					if err == nil && len(ids) != 1 {
						t.Errorf("keyset page = %v, want one remaining entity", ids)
					}
					return err
				})
				if limit < 1 || limit > corestore.MaxPageSize {
					if !errors.Is(err, corestore.ErrInvalidPage) {
						t.Errorf("limit %d error = %v", limit, err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second", "org_unknown"} {
		if err := repository.Read(t.Context(), organization, corestore.Serving, func(r corestore.Reader) error {
			rows, err := r.Projects(t.Context(), "", 1)
			want := 1
			if organization == "org_unknown" {
				want = 0
			}
			if len(rows) != want {
				t.Errorf("first page length = %d, want %d", len(rows), want)
			}
			for _, p := range rows {
				if p.OrganizationID != organization || p.ID != "prj_first" {
					t.Errorf("page escaped scope or order: %+v", p)
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepositoryDomainErrors(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	seedRepository(t, repository, "org_first")
	for _, tc := range []struct {
		name  string
		run   func(corestore.Writer) error
		want  error
		state string
	}{
		{"project uniqueness", func(w corestore.Writer) error {
			_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: "org_first", ID: "prj_duplicate", Name: "first", Profile: "native"})
			return err
		}, corestore.ErrUniqueViolation, "23505"},
		{"work item uniqueness", func(w corestore.Writer) error {
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_first", NativeID: "wi_duplicate", ProjectID: "prj_first", Number: 1, Title: "duplicate"})
			return err
		}, corestore.ErrUniqueViolation, "23505"},
		{"comment uniqueness", func(w corestore.Writer) error {
			_, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: "org_first", ID: "comment_duplicate", ProjectID: "prj_first", WorkItemID: "wi_first_1", Sequence: 1, BodyObjectKey: "example/body", ActorKind: "human", ActorPrincipalID: "example"})
			return err
		}, corestore.ErrUniqueViolation, "23505"},
		{"missing project", func(w corestore.Writer) error { _, err := w.Project(t.Context(), "missing"); return err }, corestore.ErrNotFound, ""},
		{"missing work item", func(w corestore.Writer) error { _, err := w.WorkItem(t.Context(), "prj_first", "missing"); return err }, corestore.ErrNotFound, ""},
		{"missing comment", func(w corestore.Writer) error {
			_, err := w.Comment(t.Context(), "prj_first", "wi_first_1", "missing")
			return err
		}, corestore.ErrNotFound, ""},
		{"wrong project", func(w corestore.Writer) error {
			_, err := w.WorkItem(t.Context(), "prj_second", "wi_first_1")
			return err
		}, corestore.ErrNotFound, ""},
		{"wrong work item", func(w corestore.Writer) error {
			_, err := w.Comment(t.Context(), "prj_first", "wi_first_2", "comment_first_1_1")
			return err
		}, corestore.ErrNotFound, ""},
		{"stale project", func(w corestore.Writer) error {
			p, err := w.Project(t.Context(), "prj_first")
			if err != nil {
				return err
			}
			p.ContentSettingsRevision++
			_, err = w.UpdateProject(t.Context(), p)
			return err
		}, corestore.ErrRevisionConflict, ""},
		{"stale work item", func(w corestore.Writer) error {
			p, err := w.WorkItem(t.Context(), "prj_first", "wi_first_1")
			if err != nil {
				return err
			}
			p.Revision++
			_, err = w.UpdateWorkItem(t.Context(), p)
			return err
		}, corestore.ErrRevisionConflict, ""},
		{"stale comment", func(w corestore.Writer) error {
			p, err := w.Comment(t.Context(), "prj_first", "wi_first_1", "comment_first_1_1")
			if err != nil {
				return err
			}
			p.Revision++
			_, err = w.UpdateComment(t.Context(), p)
			return err
		}, corestore.ErrRevisionConflict, ""},
		{"foreign project create", func(w corestore.Writer) error {
			_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: "org_second", ID: "prj_foreign", Name: "foreign", Profile: "native"})
			return err
		}, corestore.ErrTenantMismatch, ""},
		{"foreign work item create", func(w corestore.Writer) error {
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_second"})
			return err
		}, corestore.ErrTenantMismatch, ""},
		{"foreign comment create", func(w corestore.Writer) error {
			_, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: "org_second"})
			return err
		}, corestore.ErrTenantMismatch, ""},
		{"foreign project update", func(w corestore.Writer) error {
			_, err := w.UpdateProject(t.Context(), corestore.Project{OrganizationID: "org_second", ID: "prj_first"})
			return err
		}, corestore.ErrTenantMismatch, ""},
		{"foreign work item update", func(w corestore.Writer) error {
			_, err := w.UpdateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_second"})
			return err
		}, corestore.ErrTenantMismatch, ""},
		{"foreign comment update", func(w corestore.Writer) error {
			_, err := w.UpdateComment(t.Context(), corestore.Comment{OrganizationID: "org_second"})
			return err
		}, corestore.ErrTenantMismatch, ""},
		{"missing project delete", func(w corestore.Writer) error { return w.DeleteProject(t.Context(), "missing") }, corestore.ErrNotFound, ""},
		{"missing work item delete", func(w corestore.Writer) error { return w.DeleteWorkItem(t.Context(), "prj_first", "missing") }, corestore.ErrNotFound, ""},
		{"missing comment delete", func(w corestore.Writer) error {
			return w.DeleteComment(t.Context(), "prj_first", "wi_first_1", "missing")
		}, corestore.ErrNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := repository.Write(t.Context(), "org_first", corestore.Serving, tc.run)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if tc.state != "" {
				requireSQLState(t, err, tc.state)
				if !errors.Is(err, mutation.ErrConflict) {
					t.Errorf("error lost existing conflict classification: %v", err)
				}
				if strings.Contains(err.Error(), "first") {
					t.Errorf("error includes customer content: %v", err)
				}
			}
		})
	}
}

func TestRepositoryRollsBackFailedCallbacks(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	failure := errors.New("example callback failure")
	for _, mode := range []string{"error", "panic", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var got error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil && (mode != "panic" || recovered != failure) {
						t.Errorf("unexpected panic: %v", recovered)
					}
				}()
				got = repository.Write(ctx, "org_first", corestore.Serving, func(w corestore.Writer) error {
					if _, err := w.CreateProject(ctx, corestore.Project{OrganizationID: "org_first", ID: "prj_rollback", Name: "rollback", Profile: "native"}); err != nil {
						return err
					}
					if mode == "panic" {
						panic(failure)
					}
					if mode == "cancellation" {
						cancel()
						return nil
					}
					return failure
				})
				if mode == "panic" {
					t.Error("callback panic was swallowed")
				}
			}()
			if mode == "error" && !errors.Is(got, failure) {
				t.Errorf("callback error = %v", got)
			}
			if mode == "cancellation" && !errors.Is(got, context.Canceled) {
				t.Errorf("cancellation error = %v", got)
			}
			err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error { _, err := r.Project(t.Context(), "prj_rollback"); return err })
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("rolled-back row survived: %v", err)
			}
		})
	}
}
