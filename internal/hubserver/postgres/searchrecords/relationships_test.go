package searchrecords_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
)

func TestRelationshipsAndLabelsAreTenantScopedAndAtomic(t *testing.T) {
	t.Parallel()
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), CreatedAt: pgstoretest.HistoryAt()})
		}); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedRepository(t, repository, organization)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	writeRelationship := func(organization tracker.OrganizationID, project, item, relatedProject, related string, kind string) error {
		return repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetRelationship(t.Context(), corestore.Relationship{
				OrganizationID: organization, ProjectID: tracker.ProjectID(project), WorkItemID: tracker.NativeWorkItemID(item),
				RelatedProjectID: tracker.ProjectID(relatedProject), RelatedWorkItemID: tracker.NativeWorkItemID(related), Type: kind,
			}, "add", now)
		})
	}
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		for index, project := range []tracker.ProjectID{"prj_third", "prj_fourth"} {
			if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: "org_first", ID: project, Name: "Example " + string(project), Profile: "native", SchedulingRank: int64(index + 2)}); err != nil {
				return err
			}
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: "org_first", ProjectID: project, NativeID: tracker.NativeWorkItemID("wi_" + string(project)), Number: 1, Title: "Example issue"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, edge := range [][5]string{
		{"org_first", "prj_second", "wi_second_2", "prj_third", "wi_prj_third"},
		{"org_first", "prj_fourth", "wi_prj_fourth", "prj_first", "wi_first_2"},
	} {
		if err := writeRelationship(tracker.OrganizationID(edge[0]), edge[1], edge[2], edge[3], edge[4], "dependency"); err != nil {
			t.Fatal(err)
		}
	}
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, edge := range [][4]string{
		{"prj_first", "wi_first_2", "prj_second", "wi_second_2"},
		{"prj_third", "wi_prj_third", "prj_fourth", "wi_prj_fourth"},
	} {
		edge := edge
		workers.Add(1)
		go func() {
			defer workers.Done()
			ready <- struct{}{}
			<-start
			results <- writeRelationship("org_first", edge[0], edge[1], edge[2], edge[3], "dependency")
		}()
	}
	<-ready
	<-ready
	close(start)
	firstErr := <-results
	secondErr := <-results
	workers.Wait()
	cycleErrors, successes := 0, 0
	for _, result := range []error{firstErr, secondErr} {
		switch {
		case result == nil:
			successes++
		case errors.Is(result, corestore.ErrDependencyCycle):
			cycleErrors++
		default:
			t.Fatalf("concurrent disjoint dependency error=%v", result)
		}
	}
	if successes != 1 || cycleErrors != 1 {
		t.Fatalf("concurrent disjoint dependency successes=%d cycle errors=%d", successes, cycleErrors)
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			edge := corestore.Relationship{OrganizationID: organization, ProjectID: "prj_first", WorkItemID: "wi_first_1", RelatedProjectID: "prj_second", RelatedWorkItemID: "wi_second_1", Type: "dependency"}
			if err := w.CheckDependency(t.Context(), edge); err != nil {
				return err
			}
			page, err := w.Relationships(t.Context(), edge.ProjectID, edge.WorkItemID, corestore.RelationshipCursor{}, 10)
			if err == nil && len(page.Items) != 0 {
				t.Fatalf("dependency preflight wrote edges: %+v", page.Items)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeRelationship("org_first", "prj_first", "wi_first_1", "prj_second", "wi_second_1", "dependency"); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		edge := corestore.Relationship{OrganizationID: "org_first", ProjectID: "prj_second", WorkItemID: "wi_second_1", RelatedProjectID: "prj_first", RelatedWorkItemID: "wi_first_1", Type: "dependency"}
		if err := w.CheckDependency(t.Context(), edge); !errors.Is(err, corestore.ErrDependencyCycle) {
			t.Fatalf("dependency preflight cycle error=%v", err)
		}
		page, err := w.Relationships(t.Context(), edge.ProjectID, edge.WorkItemID, corestore.RelationshipCursor{}, 10)
		if err == nil && (len(page.Items) != 1 || page.Items[0].Direction != "blocks") {
			t.Fatalf("refused dependency preflight changed edges: %+v", page.Items)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeRelationship("org_first", "prj_second", "wi_second_1", "prj_first", "wi_first_1", "dependency"); !errors.Is(err, corestore.ErrDependencyCycle) {
		t.Fatalf("cycle error = %v", err)
	}
	if err := writeRelationship("org_second", "prj_first", "wi_first_1", "prj_second", "wi_second_1", "related"); err != nil {
		t.Fatal(err)
	}
	if err := writeRelationship("org_first", "prj_first", "wi_first_1", "prj_second", "wi_second_2", "related"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		organization   tracker.OrganizationID
		project        tracker.ProjectID
		item           tracker.NativeWorkItemID
		relatedProject tracker.ProjectID
		related        tracker.NativeWorkItemID
		kind           string
		want           bool
	}{
		{"org_first", "prj_first", "wi_first_1", "prj_second", "wi_second_1", "dependency", true},
		{"org_first", "prj_second", "wi_second_1", "prj_first", "wi_first_1", "dependency", false},
		{"org_second", "prj_first", "wi_first_1", "prj_second", "wi_second_1", "dependency", false},
		{"org_second", "prj_first", "wi_first_1", "prj_second", "wi_second_1", "related", true},
		{"org_second", "prj_second", "wi_second_1", "prj_first", "wi_first_1", "related", true},
		{"org_first", "prj_first", "wi_first_1", "prj_second", "wi_second_1", "related", false},
		{"org_first", "prj_first", "wi_first_1", "prj_second", "wi_second_2", "related", true},
	} {
		if err := repository.ReadTenant(t.Context(), test.organization, corestore.Serving, func(r tenantstore.Reader) error {
			exists, err := r.RelationshipExists(t.Context(), corestore.Relationship{OrganizationID: test.organization, ProjectID: test.project, WorkItemID: test.item, RelatedProjectID: test.relatedProject, RelatedWorkItemID: test.related, Type: test.kind})
			if err == nil && exists != test.want {
				t.Errorf("relationship presence for %s %s %s to %s=%t, want %t", test.organization, test.kind, test.item, test.related, exists, test.want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		batch, ok := r.(tenantstore.WorkItemRelationshipBatchReader)
		if !ok {
			t.Fatal("PostgreSQL reader does not expose bounded relationship batches")
		}
		facts, err := batch.WorkItemRelationshipFacts(t.Context(), "prj_first", []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"})
		if err != nil {
			return err
		}
		byWorkItem := map[tracker.NativeWorkItemID][]tenantstore.WorkItemRelationshipFact{}
		for _, fact := range facts {
			byWorkItem[fact.WorkItemID] = append(byWorkItem[fact.WorkItemID], fact)
		}
		if len(byWorkItem["wi_first_1"]) != 2 || len(byWorkItem["wi_first_2"]) < 1 {
			t.Fatalf("batched relationship facts counts: first=%d second=%d", len(byWorkItem["wi_first_1"]), len(byWorkItem["wi_first_2"]))
		}
		want := map[string]bool{"dependency/blocked_by/wi_second_1": false, "related/related/wi_second_2": false}
		for _, fact := range byWorkItem["wi_first_1"] {
			if fact.Relationship.RelatedProjectID != "prj_second" || !fact.RelatedProjectVisible {
				t.Fatalf("cross-project visibility fact=%+v", fact)
			}
			key := fact.Relationship.Type + "/" + fact.Relationship.Direction + "/" + string(fact.Relationship.RelatedWorkItemID)
			if _, ok := want[key]; ok {
				want[key] = true
			}
		}
		for key, found := range want {
			if !found {
				t.Errorf("missing relationship fact %q", key)
			}
		}
		foundSecond := false
		for _, fact := range byWorkItem["wi_first_2"] {
			foundSecond = foundSecond || fact.Relationship.RelatedWorkItemID == "wi_prj_fourth"
		}
		if !foundSecond {
			t.Fatal("second source item relationship was not returned")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		batch, ok := r.(tenantstore.WorkItemRelationshipBatchReader)
		if !ok {
			t.Fatal("PostgreSQL reader does not expose bounded relationship batches")
		}
		facts, err := batch.WorkItemRelationshipFacts(t.Context(), "prj_first", []tracker.NativeWorkItemID{"wi_first_1", "wi_first_2"})
		if err != nil {
			return err
		}
		if len(facts) != 1 || facts[0].WorkItemID != "wi_first_1" || facts[0].Relationship.RelatedWorkItemID != "wi_second_1" {
			t.Fatalf("second-tenant batch facts=%+v", facts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("rollback relationship transaction")
	err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.SetRelationship(t.Context(), corestore.Relationship{
			OrganizationID: "org_first", ProjectID: "prj_first", WorkItemID: "wi_first_2",
			RelatedProjectID: "prj_second", RelatedWorkItemID: "wi_second_2", Type: "related",
		}, "add", now); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatalf("rollback error = %v", err)
	}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetWorkItemLabels(t.Context(), "prj_first", "wi_first_1", []string{"bug", "priority:high", "bug"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetWorkItemLabels(t.Context(), "prj_first", "wi_first_2", []string{"bug", "feature"})
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		organization tracker.OrganizationID
		project      tracker.ProjectID
		item         tracker.NativeWorkItemID
		want         int
	}{
		{"org_first", "prj_first", "wi_first_1", 2},
		{"org_first", "prj_second", "wi_second_1", 1},
		{"org_second", "prj_first", "wi_first_1", 1},
	} {
		if err := repository.ReadTenant(t.Context(), tc.organization, corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.Relationships(t.Context(), tc.project, tc.item, corestore.RelationshipCursor{}, 10)
			if err != nil {
				return err
			}
			if len(page.Items) != tc.want {
				t.Fatalf("%s/%s relationship count=%d want=%d rows=%+v", tc.organization, tc.item, len(page.Items), tc.want, page.Items)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		labelPage, err := r.LabelNames(t.Context(), "prj_first", corestore.LabelCursor{}, 1)
		if err != nil {
			return err
		}
		if len(labelPage.Items) != 1 || labelPage.Items[0] != "bug" || labelPage.Next.After != "bug" {
			t.Fatalf("first label page=%+v", labelPage)
		}
		labelPage, err = r.LabelNames(t.Context(), "prj_first", labelPage.Next, 10)
		if err != nil {
			return err
		}
		if len(labelPage.Items) != 2 || labelPage.Items[0] != "feature" || labelPage.Items[1] != "priority:high" {
			t.Fatalf("second label page=%+v", labelPage)
		}
		relationPage, err := r.Relationships(t.Context(), "prj_first", "wi_first_1", corestore.RelationshipCursor{}, 1)
		if err != nil {
			return err
		}
		if len(relationPage.Items) != 1 || relationPage.Next.RelatedWorkItemID == "" {
			t.Fatalf("relationship cursor page=%+v", relationPage)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.Relationships(t.Context(), "prj_first", "wi_first_2", corestore.RelationshipCursor{}, 10)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), "org_second", corestore.Serving, func(w tenantstore.Writer) error {
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: "org_first", ProjectID: "prj_first", WorkItemID: "wi_first_1", RelatedProjectID: "prj_second", RelatedWorkItemID: "wi_second_2", Type: "related"}, "add", now)
	}); !errors.Is(err, corestore.ErrTenantMismatch) {
		t.Fatalf("cross-tenant write error = %v", err)
	}
	if err := repository.ReadTenant(t.Context(), "org_first", corestore.Serving, func(r tenantstore.Reader) error {
		page, err := r.LabelNames(t.Context(), "prj_first", corestore.LabelCursor{}, 10)
		if err != nil {
			return err
		}
		if len(page.Items) != 3 {
			t.Fatalf("labels leaked or missing: %+v", page)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalIssueVersionLabelsRoundTripIntoTypedMemberships(t *testing.T) {
	t.Parallel()
	repository, _ := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	organization := tracker.OrganizationID("org_history")
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), Local: true, CreatedAt: pgstoretest.HistoryAt()})
	}); err != nil {
		t.Fatal(err)
	}
	pgstoretest.SeedRepository(t, repository, organization)

	var immutableIssueVersion tracker.NativeIssue
	if err := json.Unmarshal([]byte(`{"labels":["legacy:imported","priority:critical"]}`), &immutableIssueVersion); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.SetWorkItemLabels(t.Context(), "prj_first", "wi_first_1", immutableIssueVersion.Labels); err != nil {
			return err
		}
		return w.SetWorkItemLabels(t.Context(), "prj_first", "wi_first_2", []string{"unrelated:current"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		page, err := r.WorkItemLabels(t.Context(), "prj_first", "wi_first_1", corestore.LabelCursor{}, 1)
		if err != nil {
			return err
		}
		if len(page.Items) != 1 || page.Items[0] != "legacy:imported" || page.Next.After != "legacy:imported" {
			t.Fatalf("historical version labels did not round-trip: %+v", page.Items)
		}
		page, err = r.WorkItemLabels(t.Context(), "prj_first", "wi_first_1", page.Next, 1)
		if err != nil {
			return err
		}
		if len(page.Items) != 1 || page.Items[0] != "priority:critical" || page.Next.After != "" {
			t.Fatalf("historical version label continuation did not round-trip: %+v", page)
		}
		other, err := r.WorkItemLabels(t.Context(), "prj_first", "wi_first_2", corestore.LabelCursor{}, 10)
		if err != nil {
			return err
		}
		if len(other.Items) != 1 || other.Items[0] != "unrelated:current" {
			t.Fatalf("labels crossed work-item scope: %+v", other.Items)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedDependencyRemovalPreservesOtherSourcesAndTenants(t *testing.T) {
	t.Parallel()
	repository, runtimePool := pgstoretest.RepositoryFor(t, pgstoretest.Template.Database(t), pgstoretest.SmallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), CreatedAt: pgstoretest.HistoryAt()})
		}); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedRepository(t, repository, organization)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			row, err := w.WorkItem(t.Context(), "prj_second", "wi_second_1")
			if err != nil {
				return err
			}
			row.NativeSourceKey = new("progress-check:example")
			if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
				return err
			}
			for _, pair := range [][2]tracker.NativeWorkItemID{{"wi_first_1", "wi_second_1"}, {"wi_first_1", "wi_second_2"}, {"wi_first_2", "wi_second_1"}} {
				provenance := "native"
				if pair[0] == "wi_first_1" && pair[1] == "wi_second_1" {
					provenance = "progress_check"
				}
				if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: organization, ProjectID: "prj_first", WorkItemID: pair[0], RelatedProjectID: "prj_second", RelatedWorkItemID: pair[1], Type: "dependency", Provenance: provenance}, "add", pgstoretest.HistoryAt()); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	rollback := errors.New("rollback owned dependencies")
	refs := []corestore.WorkItemReference{{ProjectID: "prj_second", WorkItemID: "wi_second_1"}, {ProjectID: "prj_second", WorkItemID: "wi_second_2"}, {ProjectID: "prj_second", WorkItemID: "wi_second_1"}}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, tc := range []struct {
			name, prefix string
			refs         []corestore.WorkItemReference
			result       error
			rollback     bool
		}{
			{name: "empty", prefix: "progress-check:"},
			{name: "wrong owner", prefix: "another-owner:", refs: refs},
			{name: "missing owner", refs: refs, result: corestore.ErrInvalidRelationship},
			{name: "missing endpoint", prefix: "progress-check:", refs: append(append([]corestore.WorkItemReference{}, refs...), corestore.WorkItemReference{ProjectID: "prj_second", WorkItemID: "wi_missing"}), result: corestore.ErrNotFound},
			{name: "rollback", prefix: "progress-check:", refs: refs, result: rollback, rollback: true},
		} {
			t.Run(string(organization)+"/"+tc.name, func(t *testing.T) {
				err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.RemoveOwnedDependencies(t.Context(), "prj_first", "wi_first_1", tc.prefix, tc.refs); err != nil {
						return err
					}
					if tc.rollback {
						return rollback
					}
					return nil
				})
				if !errors.Is(err, tc.result) {
					t.Fatalf("removal error=%v want=%v", err, tc.result)
				}
				if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.Relationships(t.Context(), "prj_first", "wi_first_1", corestore.RelationshipCursor{}, 10)
					if err == nil && len(page.Items) != 2 {
						t.Fatalf("rejected/rolled-back removal changed dependencies: %+v", page.Items)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.RemoveOwnedDependencies(t.Context(), "prj_first", "wi_first_1", "progress-check:", refs)
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			for _, pair := range [][2]tracker.NativeWorkItemID{{"wi_first_1", "wi_second_2"}, {"wi_first_2", "wi_second_1"}} {
				page, err := r.Relationships(t.Context(), "prj_first", pair[0], corestore.RelationshipCursor{}, 10)
				if err != nil {
					return err
				}
				if len(page.Items) != 1 || page.Items[0].RelatedWorkItemID != pair[1] {
					t.Fatalf("removal changed an unrelated edge: %+v", page.Items)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("current provenance invalidates multiple edges and preserves manual links", func(t *testing.T) {
		content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
		for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
			var otherID int64
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", RelatedProjectID: "prj_second", RelatedWorkItemID: "wi_second_1", Type: "dependency"}, "add", pgstoretest.HistoryAt()); err != nil {
					return err
				}
				for index := range 201 {
					id := tracker.NativeWorkItemID(fmt.Sprintf("wi_progress_%d", index))
					if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: "prj_second", NativeID: id, Number: int64(10 + index), Title: "Example progress dependency", NativeSourceKey: new("progress-check:" + string(id))}); err != nil {
						return err
					}
					if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", RelatedProjectID: "prj_second", RelatedWorkItemID: id, Type: "dependency", Provenance: "progress_check"}, "add", pgstoretest.HistoryAt()); err != nil {
						return err
					}
				}
				other, err := w.WorkItem(t.Context(), "prj_first", "wi_first_2")
				if err != nil {
					return err
				}
				otherID = other.ID
				return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_2", RelatedProjectID: "prj_second", RelatedWorkItemID: "wi_progress_0", Type: "dependency", Provenance: "progress_check"}, "add", pgstoretest.HistoryAt())
			}); err != nil {
				t.Fatal(err)
			}
			prepare := func(id, kind, actorKind, agent string, sequence int64) eventstore.Event {
				t.Helper()
				raw, err := json.Marshal(tracker.CollaborationEvent{ID: id, OrganizationID: org, ProjectID: "prj_first", AggregateID: "wi_first_1", AggregateSequence: sequence, Type: kind, SchemaVersion: 1, RecordedAt: pgstoretest.HistoryAt(), Actor: tracker.Actor{Kind: actorKind, PrincipalID: "example-owner", Agent: agent}})
				if err != nil {
					t.Fatal(err)
				}
				event, err := content.PrepareEvent(t.Context(), org, raw)
				if err != nil {
					t.Fatal(err)
				}
				return event
			}
			ownComment := prepare("event_progress_own", "comment.created", "machine", "progress-check", 0)
			terminal := prepare("event_progress_terminal", "run.finished", "runner", "", otherID)
			for _, event := range []eventstore.Event{ownComment, terminal} {
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
					t.Fatal(err)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					want := int64(0)
					if event.Type == "run.finished" {
						want = otherID
					}
					owner := r.(eventstore.ProgressEvidenceReader)
					sequence, err := owner.ProgressEvidenceSequence(t.Context(), "prj_first", "wi_first_1")
					if err == nil && sequence != want {
						t.Fatalf("%s evidence sequence=%d want=%d", event.Type, sequence, want)
					}
					if err != nil {
						return err
					}
					sequence, err = owner.ProgressEvidenceSequence(t.Context(), "prj_first", "wi_first_2")
					if err == nil && sequence != 0 {
						t.Fatalf("progress evidence crossed item scope: sequence=%d", sequence)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				edited, err := r.WorkItem(t.Context(), "prj_first", "wi_first_1")
				if err == nil && (edited.ID == otherID || edited.EventSequence != otherID) {
					t.Fatalf("fixture did not separate item ID from event sequence: id=%d event=%d other=%d", edited.ID, edited.EventSequence, otherID)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			assertCount := func(want int) {
				t.Helper()
				count := 0
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					cursor := corestore.RelationshipCursor{}
					for range 2 {
						page, err := r.Relationships(t.Context(), "prj_first", "wi_first_1", cursor, corestore.MaxPageSize)
						if err != nil {
							return err
						}
						count += len(page.Items)
						if page.Next == (corestore.RelationshipCursor{}) {
							return nil
						}
						cursor = page.Next
					}
					return corestore.ErrInvalidPage
				}); err != nil {
					t.Fatal(err)
				}
				if count != want {
					t.Fatalf("tenant %s dependencies=%d want=%d", org, count, want)
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.Relationships(t.Context(), "prj_first", "wi_first_2", corestore.RelationshipCursor{}, 10)
					if err != nil {
						return err
					}
					owned, err := r.ProgressDependencies(t.Context(), "prj_first", "wi_first_2", corestore.WorkItemBlockerCursor{}, 10)
					if err == nil && (len(page.Items) != 2 || len(owned) != 1 || owned[0].WorkItemID != "wi_progress_0") {
						t.Fatalf("edited item invalidated another same-project item's owned edge: item=%+v owned=%+v", page, owned)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			assertCount(203)
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				page, err := r.ProgressDependencies(t.Context(), "prj_first", "wi_first_1", corestore.WorkItemBlockerCursor{}, corestore.MaxPageSize)
				if err != nil {
					return err
				}
				if len(page) != 201 || page[0].CreatedAt.IsZero() {
					t.Fatalf("owned page mixed ordinary edges or lost timestamps: %+v", page)
				}
				last := page[corestore.MaxPageSize-1]
				tail, err := r.ProgressDependencies(t.Context(), "prj_first", "wi_first_1", corestore.WorkItemBlockerCursor{ProjectID: last.ProjectID, IssueID: last.IssueID}, corestore.MaxPageSize)
				if err == nil && (len(tail) != 1 || tail[0].IssueID != page[corestore.MaxPageSize].IssueID) {
					t.Fatalf("owned continuation lost final edge: %+v", tail)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			event := prepare("event_progress_input", "issue.edited", "human", "", 0)
			var selector string
			var issueID int64
			ctx := postgres.WithTransactionTiming(t.Context(), nil, func(_ time.Time, statement string) {
				if strings.HasPrefix(statement, "SELECT ctid FROM issue_dependencies") {
					selector = statement
				}
			})
			if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
				source, err := w.WorkItem(ctx, "prj_first", "wi_first_1")
				if err != nil {
					return err
				}
				issueID = source.ID
				if _, err := w.AppendEvent(ctx, event); err != nil {
					return err
				}
				return rollback
			}); !errors.Is(err, rollback) {
				t.Fatalf("input rollback=%v", err)
			}
			assertCount(203)
			if selector == "" {
				t.Fatal("owned invalidation selector was not observed")
			}
			for _, mode := range []string{"force_generic_plan", "force_custom_plan"} {
				func() {
					tx, err := runtimePool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback(t.Context())
					if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(org)); err != nil {
						t.Fatal(err)
					}
					var scoped bool
					if err := tx.QueryRow(t.Context(), `SELECT pg_has_role(current_user,'detent_runtime_v2','USAGE') AND NOT pg_has_role(current_user,'detent_platform_v2','USAGE') AND NOT (SELECT rolsuper FROM pg_roles WHERE rolname=current_user) AND current_setting('detent.organization_id')=$1`, string(org)).Scan(&scoped); err != nil || !scoped {
						t.Fatalf("progress plan lacks runtime tenant scope: %v", err)
					}
					if _, err := tx.Exec(t.Context(), "SET LOCAL plan_cache_mode="+mode); err != nil {
						t.Fatal(err)
					}
					statement := pgx.Identifier{"progress_expiry_" + mode}.Sanitize()
					if _, err := tx.Exec(t.Context(), "PREPARE "+statement+" AS "+selector); err != nil {
						t.Fatal(err)
					}
					var raw []byte
					if err := tx.QueryRow(t.Context(), "EXPLAIN(ANALYZE,BUFFERS,FORMAT JSON) EXECUTE "+statement+"($1,$2,$3,$4)", pgx.QueryExecModeSimpleProtocol, string(org), "prj_first", issueID, corestore.MaxRelationshipPreparationPages*corestore.MaxPageSize+1).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					type node struct {
						Type      string  `json:"Node Type"`
						Relation  string  `json:"Relation Name"`
						Index     string  `json:"Index Name"`
						Condition string  `json:"Index Cond"`
						Rows      float64 `json:"Actual Rows"`
						Removed   float64 `json:"Rows Removed by Filter"`
						Loops     float64 `json:"Actual Loops"`
						Plans     []node
					}
					var plans []struct{ Plan node }
					if err := json.Unmarshal(raw, &plans); err != nil {
						t.Fatal(err)
					}
					found := false
					var inspect func(node)
					inspect = func(plan node) {
						if plan.Relation == "issue_dependencies" {
							if (plan.Rows+plan.Removed)*plan.Loops > 201 {
								t.Errorf("progress selector examined ordinary edges: %+v", plan)
							}
							if plan.Index == "issue_dependencies_progress_owner_lookup" && strings.Contains(plan.Condition, "organization_id") && strings.Contains(plan.Condition, "dependent_project_id") && strings.Contains(plan.Condition, "dependent_issue_id") {
								found = true
							}
						}
						for _, child := range plan.Plans {
							inspect(child)
						}
					}
					if len(plans) != 1 {
						t.Fatalf("progress selector plans=%s", raw)
					}
					inspect(plans[0].Plan)
					if !found {
						t.Errorf("%s progress selector lacks provenance index: %s", mode, raw)
					}
					if _, err := tx.Exec(t.Context(), "DEALLOCATE "+statement); err != nil {
						t.Fatal(err)
					}
					if err := tx.Rollback(t.Context()); err != nil {
						t.Fatal(err)
					}
				}()
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
				t.Fatal(err)
			}
			assertCount(2)
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				page, err := r.ProgressDependencies(t.Context(), "prj_first", "wi_first_1", corestore.WorkItemBlockerCursor{}, corestore.MaxPageSize)
				if err == nil && len(page) != 0 {
					t.Fatalf("ordinary same-prefix edge was presented as progress owned: %+v", page)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				page, err := r.Relationships(t.Context(), "prj_first", "wi_first_1", corestore.RelationshipCursor{}, 10)
				if err != nil {
					return err
				}
				if len(page.Items) != 2 || page.Items[0].RelatedWorkItemID != "wi_second_1" || page.Items[1].RelatedWorkItemID != "wi_second_2" {
					t.Fatalf("source invalidation removed ordinary same-prefix dependency: %+v", page)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: org, ProjectID: "prj_first", WorkItemID: "wi_first_1", RelatedProjectID: "prj_second", RelatedWorkItemID: "wi_progress_0", Type: "dependency", Provenance: "progress_check"}, "add", pgstoretest.HistoryAt()); err != nil {
					return err
				}
				_, err := w.AppendEvent(t.Context(), event)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			assertCount(3)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.RemoveOwnedDependencies(t.Context(), "prj_first", "wi_first_1", "progress-check:", []corestore.WorkItemReference{{ProjectID: "prj_second", WorkItemID: "wi_second_1"}})
			}); err != nil {
				t.Fatal(err)
			}
			assertCount(3)
		}
	})
}
