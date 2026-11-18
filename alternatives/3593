package postgres_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestScheduledSourcesSelectCurrentMarkersBeyondOrdinaryRecords(t *testing.T) {
	t.Parallel()
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedRepository(t, repository, org)
		objects := pgstoretest.NewHistoryObjects()
		content := eventstore.NewContent(objects)
		for _, suffix := range []string{"first", "second"} {
			project := tracker.ProjectID("prj_" + suffix)
			for number := 1; number <= 2; number++ {
				id := tracker.NativeWorkItemID(fmt.Sprintf("wi_%s_%d", suffix, number))
				var current corestore.WorkItem
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					current, err = r.WorkItem(t.Context(), project, id)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				for sequence := int64(1); sequence <= 2; sequence++ {
					commentID := fmt.Sprintf("comment_%s_%d_%d", suffix, number, sequence)
					var comment corestore.Comment
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						comment, err = r.Comment(t.Context(), project, id, commentID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					value := tracker.NativeComment{OrganizationID: org, ProjectID: project, WorkItemID: id, ID: comment.ID, Revision: tracker.Revision(comment.Revision), Sequence: comment.Sequence, Actor: pgstoretest.HistoryActor(), Body: "ordinary", CreatedAt: comment.CreatedAt, UpdatedAt: comment.UpdatedAt}
					row, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: comment.ID, Revision: comment.Revision}, pgstoretest.HistoryJSON(t, value))
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.CreateCollaborationVersion(t.Context(), row); return err }); err != nil {
						t.Fatal(err)
					}
				}
				value := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: id, Number: number, Revision: tracker.Revision(current.Revision), Profile: "native"}, Title: current.Title, Body: "ordinary", Actor: pgstoretest.HistoryActor(), CreatedAt: current.NativeCreatedAt, UpdatedAt: current.NativeUpdatedAt}
				row, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: id, RecordID: string(id), Revision: current.Revision}, pgstoretest.HistoryJSON(t, value))
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.CreateCollaborationVersion(t.Context(), row); return err }); err != nil {
					t.Fatal(err)
				}
			}
		}
		var historical eventstore.CollaborationVersion
		for i := range 93 {
			id := fmt.Sprintf("scheduled_%03d", i)
			body := "detent-checks"
			at := pgstoretest.HistoryAt().Add(time.Duration(i) * time.Second)
			legacy := i >= 91
			if i >= 41 && i < 91 {
				body = "ordinary"
			}
			if legacy {
				id = fmt.Sprintf("legacy_%03d", i)
				at = pgstoretest.HistoryAt().Add(-time.Second)
				if i == 92 {
					body = "ordinary"
				}
			}
			value := tracker.NativeComment{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", ID: id, Revision: 1, Sequence: int64(i + 3), Actor: pgstoretest.HistoryActor(), Body: body, CreatedAt: at, UpdatedAt: at}
			row, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: value.ProjectID, WorkItemID: value.WorkItemID, RecordID: id, Revision: 1}, pgstoretest.HistoryJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			if legacy {
				row.SourceKnown = false
				row.SourceScheduled = false
				row.SourceObservedAt = time.Unix(0, 0).UTC()
			}
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: org, ProjectID: value.ProjectID, WorkItemID: value.WorkItemID, ID: id, Revision: 1, Sequence: value.Sequence, BodyObjectKey: row.PayloadObjectKey, ActorKind: value.Actor.Kind, ActorPrincipalID: value.Actor.PrincipalID, CreatedAt: at, UpdatedAt: at})
				if err != nil {
					return err
				}
				_, err = w.CreateCollaborationVersion(t.Context(), row)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if i == 91 {
				historical = row
			}
		}
		read := func() ([]eventstore.CollaborationVersion, bool, error) {
			var rows []eventstore.CollaborationVersion
			var partial bool
			err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				rows, partial, err = r.(eventstore.ScheduledSourceReader).ScheduledSources(t.Context(), "prj_first", "")
				return err
			})
			return rows, partial, err
		}
		if rows, partial, err := read(); err != nil || !partial || len(rows) != 40 {
			t.Fatal("unclassified current sources were reported complete or hid known evidence")
		}
		original := bytes.Clone(objects.Objects[historical.PayloadObjectKey])
		objects.Objects[historical.PayloadObjectKey] = []byte("corrupt")
		n, after, err := repository.BackfillScheduledSourcesAfter(t.Context(), org, content, 1, postgres.ScheduledSourceCursor{})
		if n != 0 || !errors.Is(err, artifact.ErrIntegrity) || after.RecordID != historical.RecordID {
			t.Fatalf("corrupt historical body backfill=%d %+v: %v", n, after, err)
		}
		if n, _, err := repository.BackfillScheduledSourcesAfter(t.Context(), org, content, 1, after); n != 1 || err != nil {
			t.Fatalf("poison row blocked following valid body: %d %v", n, err)
		}
		objects.Objects[historical.PayloadObjectKey] = original
		if n, err := repository.BackfillScheduledSources(t.Context(), org, content, 1); n != 1 || err != nil {
			t.Fatalf("bounded verified backfill=%d: %v", n, err)
		}
		rows, partial, err := read()
		if err != nil || !partial || len(rows) != 40 || rows[0].RecordID != "scheduled_040" || rows[39].RecordID != "scheduled_001" {
			t.Fatalf("latest scheduled sources=%d partial=%v: %v", len(rows), partial, err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			stored, err := r.CollaborationVersion(t.Context(), historical.RecordID, historical.Revision)
			if err != nil {
				return err
			}
			if stored != historical {
				t.Fatal("canonical source backfill rewrote immutable version metadata")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(objects.Objects[historical.PayloadObjectKey], original) {
			t.Fatal("backfill changed historical object")
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.DeleteComment(t.Context(), "prj_first", "wi_first_1", "scheduled_040")
		}); err != nil {
			t.Fatal(err)
		}
		rows, partial, err = read()
		if err != nil || !partial || len(rows) != 40 || rows[0].RecordID != "scheduled_039" || rows[39].RecordID != "scheduled_000" {
			t.Fatal("deleted canonical source retained a current projection")
		}

		var current corestore.Comment
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			current, err = r.Comment(t.Context(), "prj_first", "wi_first_1", "scheduled_039")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		next := tracker.NativeComment{OrganizationID: org, ProjectID: current.ProjectID, WorkItemID: current.WorkItemID, ID: current.ID, Revision: tracker.Revision(current.Revision + 1), Sequence: current.Sequence, Actor: pgstoretest.HistoryActor(), Body: "ordinary", CreatedAt: current.CreatedAt, UpdatedAt: pgstoretest.HistoryAt().Add(1000 * time.Second)}
		nextVersion, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: next.ProjectID, WorkItemID: next.WorkItemID, RecordID: next.ID, Revision: int64(next.Revision)}, pgstoretest.HistoryJSON(t, next))
		if err != nil {
			t.Fatal(err)
		}
		nextVersion.SourceKnown, nextVersion.SourceScheduled, nextVersion.SourceObservedAt = false, false, time.Unix(0, 0).UTC()
		current.BodyObjectKey = nextVersion.PayloadObjectKey
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.UpdateComment(t.Context(), current); err != nil {
				return err
			}
			_, err := w.CreateCollaborationVersion(t.Context(), nextVersion)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if rows, partial, err := read(); err != nil || !partial || len(rows) != 40 {
			t.Fatal("older writer update reused stale facts or hid verified sources")
		}
		if n, err := repository.BackfillScheduledSources(t.Context(), org, content, 1); n != 1 || err != nil {
			t.Fatalf("current revision backfill=%d: %v", n, err)
		}
		rows, partial, err = read()
		if err != nil || partial || len(rows) != 40 || rows[0].RecordID != "scheduled_038" || rows[39].RecordID != "legacy_091" {
			t.Fatal("obsolete scheduled revision remained in current sources")
		}
		rollback := errors.New("expected rollback")
		rolled := tracker.NativeComment{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", ID: "rolled_back", Revision: 1, Sequence: 1000, Actor: pgstoretest.HistoryActor(), Body: "detent-checks", CreatedAt: pgstoretest.HistoryAt(), UpdatedAt: pgstoretest.HistoryAt().Add(2000 * time.Second)}
		rolledVersion, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: rolled.ProjectID, WorkItemID: rolled.WorkItemID, RecordID: rolled.ID, Revision: 1}, pgstoretest.HistoryJSON(t, rolled))
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: org, ProjectID: rolled.ProjectID, WorkItemID: rolled.WorkItemID, ID: rolled.ID, Revision: 1, Sequence: rolled.Sequence, BodyObjectKey: rolledVersion.PayloadObjectKey, ActorKind: rolled.Actor.Kind, ActorPrincipalID: rolled.Actor.PrincipalID, CreatedAt: rolled.CreatedAt, UpdatedAt: rolled.UpdatedAt}); err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), rolledVersion); err != nil {
				return err
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatal("source publication transaction did not roll back")
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			if _, err := r.Comment(t.Context(), rolled.ProjectID, rolled.WorkItemID, rolled.ID); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatal("rolled-back canonical source survived")
			}
			if _, err := r.CollaborationVersion(t.Context(), rolled.ID, 1); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatal("rolled-back source version survived")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if n, err := repository.BackfillScheduledSources(t.Context(), org, content, 1); n != 0 || err != nil {
			t.Fatalf("backfill replay=%d: %v", n, err)
		}
		for _, versionFirst := range []bool{true, false} {
			id := tracker.NativeWorkItemID("wi_first_1")
			if !versionFirst {
				id = "wi_first_2"
			}
			var item corestore.WorkItem
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				item, err = r.WorkItem(t.Context(), "prj_first", id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			value := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: item.ProjectID, WorkItemID: id, Number: int(item.Number), Revision: tracker.Revision(item.Revision + 1)}, Title: item.Title, Body: "detent-checks", Actor: pgstoretest.HistoryActor(), CreatedAt: item.NativeCreatedAt, UpdatedAt: pgstoretest.HistoryAt().Add(3000 * time.Second)}
			version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: item.ProjectID, WorkItemID: id, RecordID: string(id), Revision: int64(value.Revision)}, pgstoretest.HistoryJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			item.BodyObjectKey = new(version.PayloadObjectKey)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if versionFirst {
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
				}
				changed, err := w.UpdateWorkItem(t.Context(), item)
				if err != nil {
					return err
				}
				if !versionFirst {
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
					changed, err = w.WorkItem(t.Context(), item.ProjectID, id)
					if err != nil {
						return err
					}
				}
				if changed.SourceRevision != changed.Revision || !changed.SourceScheduled || changed.SourceObservedAt == nil || !changed.SourceObservedAt.Equal(value.UpdatedAt) {
					t.Fatal("publication order left canonical source facts stale")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		for i, id := range []tracker.NativeWorkItemID{"wi_missing_body", "wi_poison", "wi_success"} {
			at := pgstoretest.HistoryAt().Add(4000 * time.Second)
			value := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: "prj_first", WorkItemID: id, Number: 3 + i, Revision: 1}, Title: "Example source", Body: "ordinary", Actor: pgstoretest.HistoryActor(), CreatedAt: at, UpdatedAt: at}
			version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: value.ProjectID, WorkItemID: id, RecordID: string(id), Revision: 1}, pgstoretest.HistoryJSON(t, value))
			if err != nil {
				t.Fatal(err)
			}
			version.SourceKnown, version.SourceScheduled, version.SourceObservedAt = false, false, time.Unix(0, 0).UTC()
			var body *string
			if i > 0 {
				body = new(version.PayloadObjectKey)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: value.ProjectID, NativeID: id, Number: int64(value.Number), Title: value.Title, BodyObjectKey: body, ActorKind: new(value.Actor.Kind), ActorPrincipalID: new(value.Actor.PrincipalID), NativeCreatedAt: at, NativeUpdatedAt: at}); err != nil {
					return err
				}
				if id == "wi_poison" {
					return nil
				}
				_, err := w.CreateCollaborationVersion(t.Context(), version)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		n, after, err = repository.BackfillScheduledSourcesAfter(t.Context(), org, content, 3, postgres.ScheduledSourceCursor{})
		if n != 2 || !errors.Is(err, artifact.ErrIntegrity) || after.RecordID != "wi_success" {
			t.Fatalf("missing version blocked healthy source or nil-body canonical record: %d %+v %v", n, after, err)
		}
		if _, partial, err := read(); err != nil || !partial {
			t.Fatal("permanently unknown source was hidden or made the whole project unavailable")
		}
		if n, next, err := repository.BackfillScheduledSourcesAfter(t.Context(), org, content, 3, after); n != 0 || next != (postgres.ScheduledSourceCursor{}) || err != nil {
			t.Fatal("backfill cursor did not wrap after later sources")
		}

	}
}
