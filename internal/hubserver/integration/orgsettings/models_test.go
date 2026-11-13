package orgsettings_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/orgsettings"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationModelRecordsWithoutSQLite(t *testing.T) {
	now := time.Date(2026, 11, 1, 21, 0, 0, 0, time.UTC)
	service, _, repository, cfg := openHub(t, hubserver.Config{Clock: func() time.Time { return now }})
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(org), func(t *testing.T) {
			token := "admin_" + string(org)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
					return err
				}
				return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "admin", Name: "Example", Hash: apikey.HashToken(token), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			scope := hubtest.Scope(t, hubtest.ScopeFacts{Credential: hubtest.SeededCredential{Organization: org, ID: "admin", Hash: apikey.HashToken(token)}, Admin: true, Now: func() time.Time { return now }})
			settings := orgsettings.New(orgsettings.Deps{Repository: repository, AccessRepository: repository, Details: cfg.RunDetails, Now: func() time.Time { return now },
				Recheck: func(ctx context.Context) error {
					return repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error { return scope.Authorize(ctx, r, true) })
				},
				Receipt: func(context.Context, string, string, string) (json.RawMessage, bool, error) { return nil, false, nil },
			})
			readModels := func(t *testing.T, path string) (projectsettings.ModelSelection, error) {
				t.Helper()
				response := performHubAPIRequest(t, service, http.MethodGet, path, token, nil)
				var selection projectsettings.ModelSelection
				if response.Code != http.StatusOK {
					return selection, fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
				}
				decodeHubResponse(t, response, &selection)
				return selection, nil
			}
			mutate := func(t *testing.T, path string, request orgsettings.ModelRequest) error {
				t.Helper()
				response := performHubAPIRequest(t, service, http.MethodPut, path, token, request)
				if response.Code != http.StatusOK {
					return fmt.Errorf("status=%d body=%s", response.Code, response.Body.String())
				}
				return nil
			}
			initial, err := settings.ReadModels(t.Context(), org)
			if err != nil || initial.Revision != 0 || initial.Selection == nil {
				t.Fatalf("initial selection = %+v, error = %v", initial, err)
			}
			if initial.Effective.Model("normal") != "gpt-6.1-sol" || initial.Effective.Stages["plan"].Effort == nil || *initial.Effective.Stages["plan"].Effort != "low" || initial.Effective.Stages["validator"].Effort == nil || *initial.Effective.Stages["validator"].Effort != "medium" {
				t.Fatalf("organization defaults missing: %+v", initial.Effective)
			}
			request := orgsettings.ModelRequest{Mutation: tracker.Mutation{IdempotencyKey: "models"}, Selection: &config.ModelSelection{Enabled: new(org == "org_first"), Preset: new("sol_first"), NormalModel: new(string(org) + "-model")}}
			raw, err := settings.UpdateModels(t.Context(), scope, "models", request)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := settings.UpdateModels(t.Context(), scope, "models", request)
			if err != nil || !bytes.Equal(raw, replay) {
				t.Fatalf("receipt replay changed: %s error=%v", replay, err)
			}
			changed := request
			changed.Selection = &config.ModelSelection{Enabled: new(org != "org_first"), Preset: new("sol_first")}
			if _, err := settings.UpdateModels(t.Context(), scope, "models", changed); err == nil {
				t.Fatal("changed request replay succeeded")
			}
			stale := request
			stale.IdempotencyKey = "stale"
			if _, err := settings.UpdateModels(t.Context(), scope, "models", stale); err == nil {
				t.Fatal("stale revision succeeded")
			}
			selection, err := settings.ReadModels(t.Context(), org)
			if err != nil {
				t.Fatal(err)
			}
			if selection.Revision != 1 || selection.Selection.Enabled == nil || *selection.Selection.Enabled != (org == "org_first") {
				t.Fatalf("stored selection = %+v", selection)
			}
			project := hubtest.Project(t, repository, org, hubtest.ProjectOptions{ID: "example", Name: "Example"})
			projectPath := "/api/v2/organizations/" + string(org) + "/projects/" + string(project.ID) + "/model-selection"
			inherited, err := readModels(t, projectPath)
			if err != nil || inherited.Effective.Enabled == nil || *inherited.Effective.Enabled != (org == "org_first") {
				t.Fatalf("inherited model = %+v error=%v", inherited, err)
			}
			if inherited.Effective.Model("normal") != string(org)+"-model" || inherited.Effective.Stages["plan"].Effort == nil || *inherited.Effective.Stages["plan"].Effort != "low" || inherited.Effective.Stages["validator"].Effort == nil || *inherited.Effective.Stages["validator"].Effort != "medium" {
				t.Fatalf("project inheritance missing: %+v", inherited.Effective)
			}
			override := orgsettings.ModelRequest{Mutation: tracker.Mutation{IdempotencyKey: "override"}, ExpectedRevision: inherited.Revision, Selection: &config.ModelSelection{Enabled: new(false)}}
			if err := mutate(t, projectPath, override); err != nil {
				t.Fatal(err)
			}
			overridden, err := readModels(t, projectPath)
			if err != nil || overridden.Selection == nil || overridden.Selection.Enabled == nil || *overridden.Selection.Enabled || overridden.Effective.Enabled == nil || *overridden.Effective.Enabled {
				t.Fatalf("project override = %+v error=%v", overridden, err)
			}
			for _, test := range []struct {
				name      string
				selection config.ModelSelection
			}{
				{"incomplete enabled policy", config.ModelSelection{Enabled: new(true)}},
				{"omitted enabled stage max", config.ModelSelection{Stages: map[string]config.ModelSelectionStage{"plan": {Effort: new("max")}}}},
				{"omitted enabled unknown stage level", config.ModelSelection{Stages: map[string]config.ModelSelectionStage{"plan": {Level: new("missing")}}}},
				{"omitted enabled level max", config.ModelSelection{Levels: map[string]config.ModelSelectionDefaults{"normal": {Effort: new("max")}}}},
			} {
				t.Run(test.name, func(t *testing.T) {
					before, err := settings.ReadModels(t.Context(), org)
					if err != nil {
						t.Fatal(err)
					}
					invalid := orgsettings.ModelRequest{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ExpectedRevision: before.Revision, Selection: &test.selection}
					_, err = settings.UpdateModels(t.Context(), scope, "models", invalid)
					var failure *hubapi.Error
					if !errors.As(err, &failure) || failure.Code != "invalid_request" || failure.Status != http.StatusUnprocessableEntity {
						t.Errorf("invalid model policy error = %v, want invalid_request with status 422", err)
					}
					after, err := settings.ReadModels(t.Context(), org)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before, after) {
						t.Error("rejected mutation changed organization revision or model policy")
					}
				})
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(t.Context(), "admin", now) }); err != nil {
				t.Fatal(err)
			}
			if _, err := settings.UpdateModels(t.Context(), scope, "models", request); !errors.Is(err, accessstore.ErrDenied) {
				t.Fatalf("revoked credential replay error = %v", err)
			}
		})
	}
}
