package postgres_test

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRelationshipsAndLabelsAreTenantScopedAndAtomic(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), Local: true, CreatedAt: historyAt()})
		}); err != nil {
			t.Fatal(err)
		}
		seedRepository(t, repository, organization)
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
	if err := writeRelationship("org_first", "prj_first", "wi_first_1", "prj_second", "wi_second_1", "dependency"); err != nil {
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
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	organization := tracker.OrganizationID("org_history")
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), Local: true, CreatedAt: historyAt()})
	}); err != nil {
		t.Fatal(err)
	}
	seedRepository(t, repository, organization)

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
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", Slug: string(organization), Local: true, CreatedAt: historyAt()})
		}); err != nil {
			t.Fatal(err)
		}
		seedRepository(t, repository, organization)
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
				if err := w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: organization, ProjectID: "prj_first", WorkItemID: pair[0], RelatedProjectID: "prj_second", RelatedWorkItemID: pair[1], Type: "dependency"}, "add", historyAt()); err != nil {
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
}
