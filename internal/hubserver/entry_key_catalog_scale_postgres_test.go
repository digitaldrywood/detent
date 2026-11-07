package hubserver

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEntryKeySelectedCatalogCostDoesNotGrowWithProjectCount(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		organization := tracker.OrganizationID(org)
		seeded := 0
		for _, size := range []int{2, 50} {
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
				credential := apiCredential{ID: org + "_owner", Hash: org + "_owner", HostedPrincipal: org + "_owner", Scope: apiScopeAdmin, HostedRole: "owner", HostedKeyScope: apikey.ScopeAdmin, HostedProjectAccess: hostedProjectsAll, Hosted: &auth.HostedIdentity{Subject: org + "_owner", OrganizationID: "provider_" + org}}
				identity := operatorIdentity(credential, org)
				ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity})
				ctx = context.WithValue(ctx, operatorCatalogKey{}, operatorCatalogAuthority{identity: identity, credential: credential})
				counter := &entryKeyReadCounter{}
				ctx = context.WithValue(ctx, entryKeyReadCounterContextKey{}, counter)
				transactions, statements := 0, 0
				ctx = postgres.WithTransactionTiming(ctx, func(name string, _ time.Time) {
					if name == "pg_begin" {
						transactions++
					}
				}, func(time.Time, string) { statements++ })
				counted := &entryKeyReadCountingRepository{Repository: service.config.TenantRepository}
				isolated := &Service{config: service.config, database: service.database}
				isolated.config.TenantRepository = counted
				for _, name := range []string{operatortool.WorkItem, operatortool.EditComment, operatortool.BoardSessionHistory, operatortool.ExplainItem, "rename_project", operatortool.GetRunnerUpdate, operatortool.CreateRunnerEnrollment} {
					available, err := isolated.entryKeyToolAvailable(ctx, name)
					if err != nil || !available {
						t.Fatalf("%s availability = %t, %v", name, available, err)
					}
				}
				if counted.readCalls != 0 || transactions != 0 || statements != 0 || counter.projectAccessPageReads.Load() != 0 {
					t.Fatalf("selected availability read tenants=%d transactions=%d statements=%d runner grants=%d", counted.readCalls, transactions, statements, counter.projectAccessPageReads.Load())
				}
				t.Logf("projects=%d selected_tools=7 tenant_reads=%d transactions=%d statements=%d", size, counted.readCalls, transactions, statements)
			})
		}
	}
}
