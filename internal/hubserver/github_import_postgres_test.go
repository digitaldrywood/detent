package hubserver

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLinkedImportUsesPostgresProjectRepository(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := &Service{config: Config{TenantRepository: repository}}
	if err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org", Name: "Example", Slug: "example", Local: true, CreatedAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
	for _, project := range []tracker.ProjectID{"first", "second"} {
		err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			record.Settings.Repository = "example-org/" + string(project)
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		})
		if err != nil {
			t.Fatalf("create import project: %v (%v)", err, errors.Unwrap(err))
		}
		for _, tt := range []struct{ name, repository, message string }{
			{"matching", "example-org/" + string(project), "transport is unavailable"},
			{"foreign", "example-org/foreign", linkedIssueRepositoryMismatch},
		} {
			t.Run(string(project)+"/"+tt.name, func(t *testing.T) {
				_, err := service.fetchLinkedSnapshot(t.Context(), nativeScope{organization: "org", project: project}, "https://github.com/"+tt.repository+"/issues/1")
				if err == nil || !strings.Contains(err.Error(), tt.message) {
					t.Fatalf("error=%v, want %q", err, tt.message)
				}
			})
		}
	}
}

func TestPostgresCheckoutBindingCommands(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := &Service{config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: time.Now}}
	seedPostgresHubAdmin(t, repository, service, nativeOrganization{ID: "org", Name: "Example", Slug: "example"})
	for _, project := range []tracker.ProjectID{"first", "second"} {
		err := repository.WriteTenant(t.Context(), "org", corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org", ID: project, Name: string(project), Profile: "native"}, States: nativeFixtureStates()})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		scope := nativeScope{organization: "org", project: project, credential: apiCredential{ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Scope: apiScopeAdmin}}
		options := nativeCommandOptions{OperationID: projectOperationID(scope, "POST", "/integration/repository")}
		binding := bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: "binding"}, ExpectedRevision: 1, Repository: "example-org/" + string(project), Source: "runner_checkout"}
		first, err := service.bindNativeRepositoryCommand(t.Context(), scope, options, binding)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := service.bindNativeRepositoryCommand(t.Context(), scope, options, binding)
		if err != nil || string(replay) != string(first) {
			t.Fatalf("binding replay=%s error=%v", replay, err)
		}
		record, err := service.readProjectRecord(t.Context(), scope)
		if err != nil {
			t.Fatal(err)
		}
		if record.Settings.CheckoutRepository != binding.Repository || record.Settings.Revision != 2 {
			t.Fatalf("binding=%+v", record.Settings)
		}
	}
}
