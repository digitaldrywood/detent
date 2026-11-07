package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationModelRecordsWithoutSQLite(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 21, 0, 0, 0, time.UTC)
	service := &Service{config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return now }}}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
					return err
				}
				return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "admin", Name: "Example", Hash: "example", Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			scope := nativeScope{organization: org, credential: apiCredential{ID: "admin", Hash: "example", Scope: apiScopeAdmin}, requireHostedAdmin: true}
			initial, err := service.readOrganizationModels(t.Context(), org)
			if err != nil || initial.Revision != 0 || initial.Selection == nil {
				t.Fatalf("initial selection = %+v, error = %v", initial, err)
			}
			if initial.Effective.Model("normal") != "gpt-6.1-sol" || initial.Effective.Stages["plan"].Effort == nil || *initial.Effective.Stages["plan"].Effort != "low" || initial.Effective.Stages["validator"].Effort == nil || *initial.Effective.Stages["validator"].Effort != "medium" {
				t.Fatalf("organization defaults missing: %+v", initial.Effective)
			}
			request := cloudModelSelectionRequest{Mutation: tracker.Mutation{IdempotencyKey: "models"}, Selection: &config.ModelSelection{Enabled: new(org == "org_first"), Preset: new("sol_first"), NormalModel: new(string(org) + "-model")}}
			raw, err := service.executeNativeMutation(t.Context(), scope, nativeCommandOptions{OperationID: "models"}, request.Mutation, request, nil)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := service.executeNativeMutation(t.Context(), scope, nativeCommandOptions{OperationID: "models"}, request.Mutation, request, nil)
			if err != nil || !bytes.Equal(raw, replay) {
				t.Fatalf("receipt replay changed: %s error=%v", replay, err)
			}
			changed := request
			changed.Selection = &config.ModelSelection{Enabled: new(org != "org_first"), Preset: new("sol_first")}
			if _, err := service.executeNativeMutation(t.Context(), scope, nativeCommandOptions{OperationID: "models"}, changed.Mutation, changed, nil); err == nil {
				t.Fatal("changed request replay succeeded")
			}
			stale := request
			stale.IdempotencyKey = "stale"
			if _, err := service.executeNativeMutation(t.Context(), scope, nativeCommandOptions{OperationID: "models"}, stale.Mutation, stale, nil); err == nil {
				t.Fatal("stale revision succeeded")
			}
			response := httptest.NewRecorder()
			c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/model-selection", nil), response)
			c.Set("native_scope", scope)
			if err := service.getCloudModelSelection(c); err != nil {
				t.Fatal(err)
			}
			var selection cloudModelSelection
			if err := json.Unmarshal(response.Body.Bytes(), &selection); err != nil {
				t.Fatal(err)
			}
			if selection.Revision != 1 || selection.Selection.Enabled == nil || *selection.Selection.Enabled != (org == "org_first") {
				t.Fatalf("stored selection = %+v", selection)
			}
			inherited, err := service.projectRecordModel(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org}})
			if err != nil || inherited.Effective.Enabled == nil || *inherited.Effective.Enabled != (org == "org_first") {
				t.Fatalf("inherited model = %+v error=%v", inherited, err)
			}
			if inherited.Effective.Model("normal") != string(org)+"-model" || inherited.Effective.Stages["plan"].Effort == nil || *inherited.Effective.Stages["plan"].Effort != "low" || inherited.Effective.Stages["validator"].Effort == nil || *inherited.Effective.Stages["validator"].Effort != "medium" {
				t.Fatalf("project inheritance missing: %+v", inherited.Effective)
			}
			var project projectstore.Record
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				project, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "example", Name: "Example", Profile: "native"}, States: nativeFixtureStates()})
				return err
			}); err != nil {
				t.Fatal(err)
			}
			projectScope := scope
			projectScope.project = project.Core.ID
			override := cloudModelSelectionRequest{Mutation: tracker.Mutation{IdempotencyKey: "override"}, ExpectedRevision: tracker.Revision(project.Settings.ModelRevision), Selection: &config.ModelSelection{Enabled: new(false)}}
			if _, err := service.executeNativeMutation(t.Context(), projectScope, nativeCommandOptions{OperationID: "project_models"}, override.Mutation, override, nil); err != nil {
				t.Fatal(err)
			}
			project, err = service.readProjectRecord(t.Context(), projectScope)
			if err != nil {
				t.Fatal(err)
			}
			overridden, err := service.projectRecordModel(t.Context(), project)
			if err != nil || overridden.Selection == nil || overridden.Selection.Enabled == nil || *overridden.Selection.Enabled || overridden.Effective.Enabled == nil || *overridden.Effective.Enabled {
				t.Fatalf("project override = %+v error=%v", overridden, err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(t.Context(), "admin", now) }); err != nil {
				t.Fatal(err)
			}
			if _, err := service.executeNativeMutation(t.Context(), scope, nativeCommandOptions{OperationID: "models"}, request.Mutation, request, nil); !errors.Is(err, accessstore.ErrDenied) {
				t.Fatalf("revoked credential replay error = %v", err)
			}
		})
	}
}
