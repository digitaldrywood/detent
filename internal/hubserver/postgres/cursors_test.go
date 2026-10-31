package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type recordIdentity struct {
	ID        int64
	Name      string
	CreatedAt time.Time
	Revision  int64
	Sequence  int64
}

func TestRecordPagesAndScansRevisitLateCommits(t *testing.T) {
	for _, family := range recordFamilies() {
		t.Run(family.name, func(t *testing.T) {
			repository, _ := repositoryFor(t, template.Database(t), postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
			prepareRecords(t, repository, "org_first", family.name == "comments")
			created := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
			create := func(name string, sequence int64) recordIdentity {
				t.Helper()
				var record recordIdentity
				if err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
					var err error
					record, err = family.create(t.Context(), w, "org_first", recordIdentity{Name: name, Sequence: sequence, CreatedAt: created})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return record
			}
			first := create("first", 1)
			release := make(chan struct{})
			ready := make(chan recordIdentity, 1)
			done := make(chan struct{})
			var writeErr error
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(func() { cancel(); <-done })
			go func() {
				defer close(done)
				writeErr = repository.Write(ctx, "org_first", corestore.Serving, func(w corestore.Writer) error {
					record, err := family.create(ctx, w, "org_first", recordIdentity{Name: "late", Sequence: 2, CreatedAt: created})
					if err != nil {
						ready <- recordIdentity{}
						return err
					}
					ready <- record
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			var late recordIdentity
			select {
			case late = <-ready:
			case <-done:
				t.Fatalf("late transaction failed: %v", writeErr)
			}
			if late.ID == 0 {
				t.Fatal("late insert failed")
			}
			high := create("committed", 3)
			if late.ID <= first.ID || high.ID <= late.ID {
				t.Fatalf("allocation order = %d, %d, %d", first.ID, late.ID, high.ID)
			}
			scan := func(cursor corestore.ScanCursor) ([]recordIdentity, corestore.ScanCursor) {
				t.Helper()
				var rows []recordIdentity
				var next corestore.ScanCursor
				if err := repository.Read(t.Context(), "org_first", corestore.Background, func(r corestore.Reader) error {
					var err error
					rows, next, err = family.scan(t.Context(), r, cursor, 1)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return rows, next
			}
			rows, cursor := scan(corestore.ScanCursor{})
			if len(rows) != 1 || rows[0].Name != "first" || cursor.Through != high.ID {
				t.Fatalf("first scan = %+v, next = %+v", rows, cursor)
			}
			var userCursor corestore.Cursor
			if err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
				var err error
				rows, userCursor, err = family.page(t.Context(), r, corestore.Cursor{}, 1)
				if err == nil && (len(rows) != 1 || rows[0].Name != "first") {
					t.Fatalf("first user page = %+v", rows)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
				if family.name == "comments" {
					return w.DeleteComment(t.Context(), "prj_records", "wi_parent", "first")
				}
				return w.DeleteWorkItem(t.Context(), "prj_records", "first")
			}); err != nil {
				t.Fatal(err)
			}
			create("appended", 4)
			rows, cursor = scan(cursor)
			if len(rows) != 1 || rows[0].Name != "committed" || cursor != (corestore.ScanCursor{}) {
				t.Fatalf("bounded sweep = %+v, next = %+v", rows, cursor)
			}
			close(release)
			<-done
			if writeErr != nil {
				t.Fatal(writeErr)
			}
			var names []string
			for range 5 {
				rows, cursor = scan(cursor)
				for _, row := range rows {
					names = append(names, row.Name)
				}
				if cursor == (corestore.ScanCursor{}) {
					break
				}
			}
			if !reflect.DeepEqual(names, []string{"late", "committed", "appended"}) {
				t.Fatalf("next sweep skipped a commit: %v", names)
			}
			if err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
				rows, _, err := family.page(t.Context(), r, userCursor, 4)
				var names []string
				for _, row := range rows {
					names = append(names, row.Name)
				}
				if err == nil && !reflect.DeepEqual(names, []string{"late", "committed", "appended"}) {
					t.Fatalf("user continuation skipped concurrent inserts after deleted anchor: %v", names)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type recordFamily struct {
	name   string
	create func(context.Context, corestore.Writer, tracker.OrganizationID, recordIdentity) (recordIdentity, error)
	page   func(context.Context, corestore.Reader, corestore.Cursor, int) ([]recordIdentity, corestore.Cursor, error)
	scan   func(context.Context, corestore.Reader, corestore.ScanCursor, int) ([]recordIdentity, corestore.ScanCursor, error)
}

func recordFamilies() []recordFamily {
	return []recordFamily{
		{"work items", func(ctx context.Context, w corestore.Writer, org tracker.OrganizationID, record recordIdentity) (recordIdentity, error) {
			row, err := w.CreateWorkItem(ctx, corestore.WorkItem{OrganizationID: org, ProjectID: "prj_records", NativeID: tracker.NativeWorkItemID(record.Name), ID: record.ID, Number: record.Sequence, Revision: record.Revision, EventSequence: record.Sequence, Title: record.Name, CreatedAt: record.CreatedAt})
			return recordIdentity{row.ID, string(row.NativeID), row.CreatedAt, row.Revision, row.EventSequence}, err
		}, func(ctx context.Context, r corestore.Reader, cursor corestore.Cursor, limit int) ([]recordIdentity, corestore.Cursor, error) {
			page, err := r.WorkItemPage(ctx, "prj_records", cursor, limit)
			rows := make([]recordIdentity, len(page.Items))
			for i, row := range page.Items {
				rows[i] = recordIdentity{row.ID, string(row.NativeID), row.CreatedAt, row.Revision, row.EventSequence}
			}
			return rows, page.Next, err
		}, func(ctx context.Context, r corestore.Reader, cursor corestore.ScanCursor, limit int) ([]recordIdentity, corestore.ScanCursor, error) {
			page, err := r.ScanWorkItems(ctx, "prj_records", cursor, limit)
			rows := make([]recordIdentity, len(page.Items))
			for i, row := range page.Items {
				rows[i] = recordIdentity{row.ID, string(row.NativeID), row.CreatedAt, row.Revision, row.EventSequence}
			}
			return rows, page.Next, err
		}},
		{"comments", func(ctx context.Context, w corestore.Writer, org tracker.OrganizationID, record recordIdentity) (recordIdentity, error) {
			row, err := w.CreateComment(ctx, corestore.Comment{OrganizationID: org, ProjectID: "prj_records", WorkItemID: "wi_parent", ID: record.Name, RecordID: record.ID, Sequence: record.Sequence, Revision: record.Revision, BodyObjectKey: "example/body", ActorKind: "human", ActorPrincipalID: "example", CreatedAt: record.CreatedAt})
			return recordIdentity{row.RecordID, row.ID, row.CreatedAt, row.Revision, row.Sequence}, err
		}, func(ctx context.Context, r corestore.Reader, cursor corestore.Cursor, limit int) ([]recordIdentity, corestore.Cursor, error) {
			page, err := r.CommentPage(ctx, "prj_records", "wi_parent", cursor, limit)
			rows := make([]recordIdentity, len(page.Items))
			for i, row := range page.Items {
				rows[i] = recordIdentity{row.RecordID, row.ID, row.CreatedAt, row.Revision, row.Sequence}
			}
			return rows, page.Next, err
		}, func(ctx context.Context, r corestore.Reader, cursor corestore.ScanCursor, limit int) ([]recordIdentity, corestore.ScanCursor, error) {
			page, err := r.ScanComments(ctx, "prj_records", "wi_parent", cursor, limit)
			rows := make([]recordIdentity, len(page.Items))
			for i, row := range page.Items {
				rows[i] = recordIdentity{row.RecordID, row.ID, row.CreatedAt, row.Revision, row.Sequence}
			}
			return rows, page.Next, err
		}},
	}
}

func prepareRecords(t *testing.T, repository corestore.Repository, org tracker.OrganizationID, comments bool) {
	t.Helper()
	if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
		if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "prj_records", Name: "Records", Profile: "native"}); err != nil {
			return err
		}
		if comments {
			_, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_records", NativeID: "wi_parent", Number: 1, Title: "Parent"})
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecordPagesPreserveImportsAndTranslateCursors(t *testing.T) {
	created := time.Date(2020, 1, 2, 3, 4, 5, 123456000, time.UTC)
	for _, family := range recordFamilies() {
		t.Run(family.name, func(t *testing.T) {
			repository, _ := repositoryFor(t, template.Database(t), smallPool())
			for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
				prepareRecords(t, repository, org, family.name == "comments")
				if err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
					for _, want := range []recordIdentity{{103, "third", created.Add(time.Second), 7, 1}, {102, "second", created, 8, 2}, {101, "first", created, 9, 3}} {
						got, err := family.create(t.Context(), w, org, want)
						if err != nil {
							return err
						}
						if got.ID != want.ID || got.Name != want.Name || !got.CreatedAt.Equal(want.CreatedAt) || got.Revision != want.Revision || got.Sequence != want.Sequence {
							t.Errorf("import = %+v, want %+v", got, want)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				err := repository.Write(t.Context(), org, corestore.Serving, func(w corestore.Writer) error {
					_, err := family.create(t.Context(), w, org, recordIdentity{ID: 101, Name: "duplicate", Sequence: 4})
					return err
				})
				if !errors.Is(err, corestore.ErrUniqueViolation) {
					t.Fatalf("duplicate numeric identity error = %v", err)
				}
			}
			var cursor corestore.Cursor
			if err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
				rows, next, err := family.page(t.Context(), r, corestore.Cursor{}, 1)
				if err == nil && (len(rows) != 1 || rows[0].Name != "first" || next.Version != 2) {
					t.Fatalf("first page = %+v, next = %+v", rows, next)
				}
				cursor = next
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, version := range []int{1, 2} {
				t.Run(fmt.Sprintf("version %d", version), func(t *testing.T) {
					after := cursor
					after.Version = version
					if version == 1 {
						after.CreatedAt = time.Time{}
					}
					if err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
						rows, next, err := family.page(t.Context(), r, after, 1)
						if err != nil {
							return err
						}
						if len(rows) != 1 || rows[0].Name != "second" || next.Version != 2 || next.ID != 102 {
							t.Fatalf("continued page = %+v, next = %+v", rows, next)
						}
						rows, next, err = family.page(t.Context(), r, next, 2)
						if err == nil && (len(rows) != 1 || rows[0].Name != "third" || next != (corestore.Cursor{})) {
							t.Fatalf("last page = %+v, next = %+v", rows, next)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
			for _, tc := range []struct {
				name   string
				change func(*corestore.Cursor)
			}{
				{"tenant", func(c *corestore.Cursor) { c.OrganizationID = "org_second" }},
				{"project", func(c *corestore.Cursor) { c.ProjectID = "prj_other" }},
				{"work item", func(c *corestore.Cursor) { c.WorkItemID = "wi_other" }},
				{"version", func(c *corestore.Cursor) { c.Version = 99 }},
				{"missing timestamp", func(c *corestore.Cursor) { c.CreatedAt = time.Time{} }},
				{"missing legacy anchor", func(c *corestore.Cursor) { c.Version = 1; c.CreatedAt = time.Time{}; c.ID = 999 }},
			} {
				t.Run(tc.name, func(t *testing.T) {
					after := cursor
					tc.change(&after)
					err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
						_, _, err := family.page(t.Context(), r, after, 1)
						return err
					})
					if !errors.Is(err, corestore.ErrInvalidCursor) {
						t.Fatalf("invalid cursor error = %v", err)
					}
				})
			}
			for _, limit := range []int{0, corestore.MaxPageSize + 1} {
				t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
					if err := repository.Read(t.Context(), "org_first", corestore.Serving, func(r corestore.Reader) error {
						_, _, pageErr := family.page(t.Context(), r, corestore.Cursor{}, limit)
						_, _, scanErr := family.scan(t.Context(), r, corestore.ScanCursor{}, limit)
						if !errors.Is(pageErr, corestore.ErrInvalidPage) || !errors.Is(scanErr, corestore.ErrInvalidPage) {
							t.Fatalf("invalid limit returned page=%v scan=%v", pageErr, scanErr)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestImportedIDsDoNotCollideWithAutomaticInserts(t *testing.T) {
	for _, family := range recordFamilies() {
		t.Run(family.name, func(t *testing.T) {
			for _, imports := range [][]int64{{1}, {100, 2}} {
				t.Run(fmt.Sprint(imports), func(t *testing.T) {
					repository, _ := repositoryFor(t, template.Database(t), smallPool())
					prepareRecords(t, repository, "org_first", family.name == "comments")
					var maximum int64
					for i, id := range imports {
						maximum = max(maximum, id)
						if err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
							got, err := family.create(t.Context(), w, "org_first", recordIdentity{ID: id, Name: fmt.Sprintf("import_%d", i), Sequence: int64(i + 1)})
							if err == nil && got.ID != id {
								t.Errorf("imported ID = %d, want %d", got.ID, id)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					for i := range 2 {
						if err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
							got, err := family.create(t.Context(), w, "org_first", recordIdentity{Name: fmt.Sprintf("automatic_%d", i), Sequence: int64(len(imports) + i + 1)})
							if err == nil && got.ID <= maximum {
								t.Errorf("automatic ID = %d, want greater than %d", got.ID, maximum)
							}
							maximum = got.ID
							return err
						}); err != nil {
							t.Fatalf("automatic insert after import: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestImportedIDsWaitForConcurrentAllocations(t *testing.T) {
	for _, family := range recordFamilies() {
		t.Run(family.name, func(t *testing.T) {
			admin := template.Database(t)
			repository, _ := repositoryFor(t, admin, postgres.PoolConfig{MaxConnections: 3, ServingConnections: 2, BackgroundConnections: 1})
			prepareRecords(t, repository, "org_first", family.name == "comments")
			ctx, cancel := context.WithCancel(t.Context())
			release := make(chan struct{})
			ready := make(chan recordIdentity, 1)
			allocated := make(chan struct{})
			imported := make(chan struct{})
			var allocationErr, importErr error
			go func() {
				defer close(allocated)
				allocationErr = repository.Write(ctx, "org_first", corestore.Serving, func(w corestore.Writer) error {
					row, err := family.create(ctx, w, "org_first", recordIdentity{Name: "automatic_first", Sequence: 1})
					if err != nil {
						return err
					}
					ready <- row
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			}()
			t.Cleanup(func() { cancel(); <-allocated })
			select {
			case row := <-ready:
				if row.ID <= 0 {
					t.Fatal("automatic allocation is missing")
				}
			case <-allocated:
				t.Fatalf("automatic allocation: %v", allocationErr)
			}
			go func() {
				defer close(imported)
				importErr = repository.Write(ctx, "org_first", corestore.Serving, func(w corestore.Writer) error {
					_, err := family.create(ctx, w, "org_first", recordIdentity{ID: 100, Name: "imported", Sequence: 2})
					return err
				})
			}()
			t.Cleanup(func() { cancel(); <-imported })
			testenv.WaitUntil(t, ctx, func() error {
				select {
				case <-imported:
					t.Fatalf("import completed before concurrent allocation committed: %v", importErr)
				default:
				}
				var waiting bool
				if err := admin.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted AND database=(SELECT oid FROM pg_database WHERE datname=current_database()))`).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return errors.New("import has not reached concurrent allocation")
				}
				return nil
			})
			close(release)
			<-allocated
			if allocationErr != nil {
				t.Fatal(allocationErr)
			}
			<-imported
			if importErr != nil {
				t.Fatal(importErr)
			}
			if err := repository.Write(t.Context(), "org_first", corestore.Serving, func(w corestore.Writer) error {
				row, err := family.create(t.Context(), w, "org_first", recordIdentity{Name: "automatic_next", Sequence: 3})
				if err == nil && row.ID <= 100 {
					t.Errorf("automatic ID = %d, want greater than imported 100", row.ID)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
