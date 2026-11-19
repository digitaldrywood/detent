package githubhosted

import (
	"context"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

func TestLinkedImportUsesPostgresProjectRepository(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	org := hubtest.Organization(t, repository, hubtest.OrganizationOptions{}).OrganizationID
	intake := LinkedIntakeDeps{ReadProject: func(ctx context.Context, scope hubscope.NativeScope) (projectstore.Metadata, error) {
		var project projectstore.Metadata
		err := repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			project, err = r.ProjectMetadata(ctx, scope.Project)
			return err
		})
		return project, hubapi.ProjectRecordError(err)
	}}
	type bindingCase struct{ name, repository, message string }
	for _, id := range []tracker.ProjectID{"first", "second"} {
		hubtest.Project(t, repository, org, hubtest.ProjectOptions{ID: id})
		bound := "example-org/" + string(id)
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), id)
			if err != nil {
				return err
			}
			record.Settings.Repository = bound
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		cases := []bindingCase{{"matching", bound, "transport is unavailable"}}
		if id == "second" {
			cases = append(cases, bindingCase{
				"foreign", "example-org/first", "GitHub issue must belong to this native project's attached repository",
			})
		}
		for _, test := range cases {
			t.Run(string(id)+"/"+test.name, func(t *testing.T) {
				scope := hubscope.NativeScope{Organization: org, Project: id}
				_, err := intake.Fetch(t.Context(), scope, "https://github.com/"+test.repository+"/issues/1")
				if err == nil || !strings.Contains(err.Error(), test.message) {
					t.Fatalf("error=%v, want %q", err, test.message)
				}
			})
		}
	}
}
