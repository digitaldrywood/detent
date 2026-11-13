package integrationpage_test

import (
	"context"
	"errors"
	"fmt"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integrationpage"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestIntegrationProjectionRejectsStaleSource(t *testing.T) {
	_, repository, admin := pageFixture(t)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		project := pageProject(t, repository, organization, "Example")
		if _, err := admin.ExecContext(t.Context(), "DELETE FROM project_integration_page_dirty WHERE organization_id=$1", organization); err != nil {
			t.Fatal(err)
		}
		var candidate projectstore.IntegrationCandidate
		err := repository.WriteTenant(t.Context(), organization, corestore.Background, func(w tenantstore.Writer) error {
			var err error
			candidate, err = w.(projectstore.IntegrationPageWriter).ClaimIntegrationPage(t.Context())
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if candidate.ProjectID != project.ID || candidate.Generation <= 0 || candidate.SourceAt.IsZero() {
			t.Fatalf("candidate=%+v", candidate)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), project.ID)
			if err != nil {
				return err
			}
			record.Settings.CheckoutRepository = "example-org/repository"
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		stale := projectstore.Integration{Name: "Stale"}
		publish := func() error {
			return repository.WriteTenant(t.Context(), organization, corestore.Background, func(w tenantstore.Writer) error {
				return w.(projectstore.IntegrationPageWriter).PublishIntegrationPage(t.Context(), candidate, &stale, time.Now())
			})
		}
		if err := publish(); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("stale projection accepted: %v", err)
		}
		if err := repository.WriteTenant(t.Context(), organization, corestore.Background, func(w tenantstore.Writer) error {
			writer := w.(projectstore.IntegrationPageWriter)
			latest, err := writer.ClaimIntegrationPage(t.Context())
			if err != nil {
				return err
			}
			record, err := w.ProjectRecord(t.Context(), project.ID)
			if err != nil {
				return err
			}
			page := integrationpage.Record(record)
			return writer.PublishIntegrationPage(t.Context(), latest, &page, time.Now())
		}); err != nil {
			t.Fatal(err)
		}
		if err := publish(); err != nil {
			t.Fatalf("replay failed: %v", err)
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			page, err := r.(projectstore.IntegrationPageReader).IntegrationPage(t.Context(), project.ID)
			if err == nil && (page.Name != "Example" || page.Intake != "automatic" || page.CheckoutRepository != "example-org/repository") {
				t.Errorf("replay overwrote current page: %+v", page)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIntegrationProjectionDoesNotDeadlockDeletion(t *testing.T) {
	_, repository, admin := pageFixture(t)
	hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_first"})
	projectID := tracker.ProjectID("example")
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: "org_first", ID: projectID, Name: "Example", Slug: "example", Profile: "native"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	admin.SetMaxOpenConns(2)
	var candidate projectstore.IntegrationCandidate
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Background, func(w tenantstore.Writer) error {
		var err error
		candidate, err = w.(projectstore.IntegrationPageWriter).ClaimIntegrationPage(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var project string
	if err := tx.QueryRowContext(t.Context(), "SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR UPDATE", "org_first", projectID).Scan(&project); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- repository.WriteTenant(ctx, "org_first", corestore.Background, func(w tenantstore.Writer) error {
			return w.(projectstore.IntegrationPageWriter).PublishIntegrationPage(ctx, candidate, &projectstore.Integration{Name: "Example"}, time.Now())
		})
	}()
	completed := false
	defer func() {
		cancel()
		_ = tx.Rollback()
		if !completed {
			<-finished
		}
	}()
	testenv.WaitUntil(t, t.Context(), func() error {
		var waiting bool
		err := admin.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND (query LIKE 'SELECT id FROM projects%' OR query LIKE 'INSERT INTO project_integration_pages%'))").Scan(&waiting)
		if err != nil || !waiting {
			return fmt.Errorf("publisher not blocked on parent: %v", err)
		}
		return nil
	})
	if _, err := tx.ExecContext(t.Context(), "DELETE FROM projects WHERE organization_id=$1 AND id=$2", "org_first", project); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	err = <-finished
	completed = true
	if err != nil {
		t.Fatalf("publication after deletion: %v", err)
	}
}

func TestIntegrationFailedCandidateDoesNotBlockBackfill(t *testing.T) {
	_, repository, admin := pageFixture(t)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		pageProject(t, repository, organization, "First example")
		hubtest.Project(t, repository, organization, hubtest.ProjectOptions{Name: "Second example"})
		if _, err := admin.ExecContext(t.Context(), "DELETE FROM project_integration_page_dirty WHERE organization_id=$1", organization); err != nil {
			t.Fatal(err)
		}
		seen := map[tracker.ProjectID]bool{}
		for range 4 {
			if err := repository.WriteTenant(t.Context(), organization, corestore.Background, func(w tenantstore.Writer) error {
				candidate, err := w.(projectstore.IntegrationPageWriter).ClaimIntegrationPage(t.Context())
				if err == nil {
					seen[candidate.ProjectID] = true
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		if len(seen) != 2 {
			t.Errorf("failed candidate blocked backfill: discovered %d projects", len(seen))
		}
	}
}

func TestIntegrationBackfillPassesPublishedProject(t *testing.T) {
	service, repository, admin := pageFixture(t)
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization})
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			for _, id := range []tracker.ProjectID{"example_a", "example_z"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: string(id), Profile: "native"}, States: hubtest.States()}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		ctx := (hubmaintenance.Budget{Items: 1, Bytes: integrationpage.MaintenanceBytes}).Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
		if _, err := service.Maintain(ctx, organization); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "DELETE FROM project_integration_page_dirty WHERE organization_id=$1", organization); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE project_integration_page_progress SET after_project_id='' WHERE organization_id=$1", organization); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := service.Maintain(ctx, organization); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.(projectstore.IntegrationPageReader).IntegrationPage(t.Context(), "example_z")
			return err
		}); err != nil {
			t.Fatalf("published project blocked next backfill: %v", err)
		}
	}
}
