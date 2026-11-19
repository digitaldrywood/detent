package hubserver

import (
	"context"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEntryKeySelectedCatalogExposesScopedTools(t *testing.T) {
	t.Parallel()
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		organization := tracker.OrganizationID(org)
		seeded := 0
		for _, size := range []int{2} {
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				for index := seeded; index < size; index++ {
					project := tracker.ProjectID(fmt.Sprintf("%s_project_%d", org, index))
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
						return err
					}
					if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: org + "_owner", ProjectID: project, CanWrite: true, ManageRunner: true}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			seeded = size
			t.Run(fmt.Sprintf("%s/%d_projects", org, size), func(t *testing.T) {
				credential := hubapi.APICredential{ID: org + "_owner", Hash: org + "_owner", HostedPrincipal: org + "_owner", Scope: hubapi.APIScopeAdmin, HostedRole: "owner", HostedKeyScope: apikey.ScopeAdmin, HostedProjectAccess: hubapi.HostedProjectsAll, Hosted: &auth.HostedIdentity{Subject: org + "_owner", OrganizationID: "provider_" + org}}
				identity := hubscope.OperatorIdentity(credential, org)
				ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity})
				ctx = context.WithValue(ctx, operatorCatalogKey{}, operatorCatalogAuthority{identity: identity, credential: credential})
				for _, name := range []string{operatortool.WorkItem, operatortool.EditComment, operatortool.BoardSessionHistory, operatortool.ExplainItem, "rename_project", operatortool.GetRunnerUpdate, operatortool.CreateRunnerEnrollment} {
					available, err := service.entryKeyToolAvailable(ctx, name)
					if err != nil || !available {
						t.Fatalf("%s availability = %t, %v", name, available, err)
					}
				}
			})
		}
	}
}
