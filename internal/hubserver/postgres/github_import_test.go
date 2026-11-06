package postgres_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestGitHubImportJobsKeepProjectAndTenantScope(t *testing.T) {
	repository, _ := repositoryFor(t, template.Database(t), smallPool())
	for _, org := range []tracker.OrganizationID{"first", "second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: string(org), Local: true, CreatedAt: time.Now()})
		}); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.ProjectID{"one", "two"} {
			err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"}); err != nil {
					return err
				}
				owner, ok := w.(projectstore.GitHubImportWriter)
				if !ok {
					return fmt.Errorf("typed GitHub import owner unavailable")
				}
				job, err := owner.CreateGitHubImport(t.Context(), projectstore.GitHubImport{OrganizationID: org, ProjectID: project, ID: "same", IssueNumber: 1, Stage: "issue", Status: "pending", ObservedAt: time.Now().UTC()})
				if err != nil {
					return err
				}
				if job.Revision != 1 {
					t.Fatalf("revision=%d", job.Revision)
				}
				job.Stage = "comments"
				job.Gaps = []string{"missing source"}
				job, err = owner.UpdateGitHubImport(t.Context(), job)
				if err != nil {
					return err
				}
				stale := job
				stale.Revision = 1
				if _, err := owner.UpdateGitHubImport(t.Context(), stale); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("stale update=%v", err)
				}
				if err == nil && job.Revision != 2 {
					t.Fatalf("updated revision=%d", job.Revision)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			for _, project := range []tracker.ProjectID{"one", "two"} {
				job, err := r.(projectstore.GitHubImportReader).GitHubImport(t.Context(), project, "same")
				if err != nil {
					return err
				}
				if job.OrganizationID != org || job.ProjectID != project || job.Stage != "comments" || len(job.Gaps) != 1 {
					t.Fatalf("job=%+v", job)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
