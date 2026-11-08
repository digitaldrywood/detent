package postgres_test

import (
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestGitHubReconciliationRetainsFencedTenantCursors(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"first", "second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), CreatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"one", "two"} {
				record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo"}}})
				if err != nil {
					return err
				}
				record.Settings.Repository = "example-org/" + string(project)
				if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.ProjectID{"one", "two"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(projectstore.GitHubReconciliationWriter)
				row, err := owner.NextGitHubReconciliation(t.Context(), now.Add(-time.Second))
				if err != nil {
					return err
				}
				if row.ProjectID != project || row.Revision != 0 || row.NodeID != "" || row.Repository != "example-org/"+string(project) {
					t.Fatalf("uninitialized binding=%+v", row)
				}
				row.NodeID, row.NextAttemptAt = "R_"+string(project), now
				return owner.SaveGitHubReconciliation(t.Context(), row)
			}); err != nil {
				t.Fatal(err)
			}
		}
		for _, project := range []tracker.ProjectID{"one", "two"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(projectstore.GitHubReconciliationWriter)
				row, err := owner.NextGitHubReconciliation(t.Context(), now)
				if err != nil {
					return err
				}
				if row.ProjectID != project || row.Revision != 1 || row.AfterWorkItem != "" {
					t.Fatalf("cursor=%+v", row)
				}
				row.AfterWorkItem = "wi_after"
				row.Attempts = 2
				row.NextAttemptAt = now.Add(time.Minute)
				if err := owner.SaveGitHubReconciliation(t.Context(), row); err != nil {
					return err
				}
				if err := owner.SaveGitHubReconciliation(t.Context(), row); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("stale cursor update=%v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			owner := r.(projectstore.GitHubReconciliationReader)
			if _, err := owner.NextGitHubReconciliation(t.Context(), now); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("future retry selected=%v", err)
			}
			row, err := owner.NextGitHubReconciliation(t.Context(), now.Add(time.Minute))
			if err != nil {
				return err
			}
			if row.AfterWorkItem != "wi_after" || row.Attempts != 2 || row.Revision != 2 {
				t.Fatalf("recovered=%+v", row)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
