package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestDoctorBudgetOverridesShowsActiveOverride(t *testing.T) {
	details := &clihangDetails{data: make(map[string][]byte)}
	var config hubserver.Config
	service, _ := clihangPostgresHub(t, "budget-token", nil, clihangPostgresOptions{config: &config, details: details, seed: func(repository *postgres.Repository) error {
		for _, organization := range []tracker.OrganizationID{"org_example", "org_other"} {
			raw, err := json.Marshal("---\ntracker:\n  kind: hub_native\nbudget:\n  enabled: true\n  per_day_max_usd: 100\n  per_issue_max_usd: 10\n---\nExample workflow.\n")
			if err != nil {
				return err
			}
			ref, err := details.StoreDetail(t.Context(), string(organization), raw)
			if err != nil {
				return err
			}
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				for _, id := range []tracker.ProjectID{"prj_quiet", "prj_busy"} {
					row, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: string(id), Slug: strings.TrimPrefix(string(id), "prj_"), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
					if err != nil {
						return err
					}
					row.Settings.WorkflowKey, row.Settings.WorkflowSHA256, row.Settings.WorkflowBytes, row.Settings.WorkflowContentBytes = ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize
					if _, err := w.UpdateProjectRecord(t.Context(), row); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	}})
	now := time.Now().UTC().Truncate(time.Second)
	var firstCreated time.Time
	for index, organization := range []string{"org_example", "org_other"} {
		if index > 0 {
			copied := *config.Hosted
			copied.OrganizationID, copied.WorkOSOrganizationID = organization, "provider_"+organization
			config.Hosted = &copied
			var err error
			service, err = hubserver.Open(t.Context(), config)
			if err != nil {
				t.Fatal(err)
			}
		}
		currentService := service
		t.Cleanup(func() {
			if err := currentService.Close(); err != nil {
				t.Error(err)
			}
		})
		server := httptest.NewServer(service.Handler())
		t.Cleanup(server.Close)
		configPath := filepath.Join(t.TempDir(), "global.yaml")
		token := "budget-token"
		if index > 0 {
			token += "-other"
		}
		settings, err := globalconfig.Default()
		if err != nil {
			t.Fatal(err)
		}
		settings.Client = globalconfig.HubClient{URL: server.URL, OrganizationID: organization, TokenEnvironment: "EXAMPLE_BUDGET_TOKEN", NativeProjects: map[string]string{"quiet": "prj_quiet", "busy": "prj_busy"}}
		if err := globalconfig.Write(configPath, settings); err != nil {
			t.Fatal(err)
		}
		opts := defaultOptions()
		opts.lookupEnv = func(name string) string {
			if name == "EXAMPLE_BUDGET_TOKEN" {
				return token
			}
			return ""
		}
		opts.httpDo = server.Client().Do
		owner, err := openHubBudgetOverrideStore(configPath, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, project := range []string{"quiet", "busy"} {
			if _, err := owner.ActiveBudgetOverride(t.Context(), project, now); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("initial override: %v", err)
			}
			command := newBudgetCommand(&configPath, opts)
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			cap := 200 + index*50
			command.SetArgs([]string{"override", "--project", project, "--per-day-max-usd", fmt.Sprint(cap), "--duration", "4h", "--reason", "release work"})
			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("set command: %v", err)
			}
			value, err := owner.ActiveBudgetOverride(t.Context(), project, now)
			if err != nil || value.PerDayMaxUSD == nil || *value.PerDayMaxUSD != float64(cap) || value.Reason != "release work" {
				t.Fatalf("override=%+v error=%v", value, err)
			}
			if index == 0 && project == "quiet" {
				firstCreated = value.CreatedAt
			}
		}
		writer, workflow, closeStore, err := openBudgetOverrideContext(t.Context(), configPath, "quiet", opts)
		if err != nil || writer == nil || !workflow.Config.Budget.Enabled {
			t.Fatalf("Hub workflow: %+v error=%v", workflow.Config.Budget, err)
		}
		closeStore()
		values, err := owner.ListActiveBudgetOverrides(t.Context(), now)
		if err != nil || len(values) != 2 {
			t.Fatalf("overrides=%+v error=%v", values, err)
		}
		if _, err := owner.SetBudgetOverride(t.Context(), store.BudgetOverride{ProjectID: "quiet", PerDayMaxUSD: new(float64(200 + index*50)), ExpiresAt: now.Add(4 * time.Hour), Reason: "release work"}); err != nil {
			t.Fatal(err)
		}
		check := checkDoctorBudgetOverrides(t.Context(), globalconfig.PathResolution{Path: configPath}, "quiet", now, doctorDeps{lookupEnv: opts.lookupEnv, httpDo: opts.httpDo})
		if check.Status != doctorWarn || !strings.Contains(check.Detail, "expires in 4h0m0s") || !strings.Contains(check.Detail, "release work") || strings.Contains(check.Detail, "busy:") {
			t.Fatalf("checkDoctorBudgetOverrides() = %#v", check)
		}
		native, err := owner.project("quiet")
		if err != nil {
			t.Fatal(err)
		}
		request := struct {
			tracker.Mutation
			PerDayMaxUSD float64   `json:"per_day_max_usd"`
			ExpiresAt    time.Time `json:"expires_at"`
			Reason       string    `json:"reason"`
		}{tracker.Mutation{IdempotencyKey: "example-budget-replay"}, 210, now.Add(5 * time.Hour), "reviewed extension"}
		var original, replay budgetOverrideResult
		if err := owner.client.Request(t.Context(), http.MethodPut, native.Scope()+"/budget-override", request, &original); err != nil {
			t.Fatal(err)
		}
		if err := owner.client.Request(t.Context(), http.MethodPut, native.Scope()+"/budget-override", request, &replay); err != nil {
			t.Fatal(err)
		}
		if original.CreatedAt != replay.CreatedAt || original.ExpiresAt != replay.ExpiresAt || original.Reason != replay.Reason {
			t.Fatalf("receipt replay changed response: %+v %+v", original, replay)
		}
		effective, err := budget.EffectiveConfig(t.Context(), budget.Config{ProjectID: "quiet", Enabled: true, PerDayMaxUSD: 100, Overrides: budgetOverrideStore(newHubBudgetSessionStore(native, "quiet"))}, now)
		if err != nil || effective.PerDayMaxUSD != 210 {
			t.Fatalf("runtime override=%+v error=%v", effective, err)
		}
		request.PerDayMaxUSD = 211
		err = owner.client.Request(t.Context(), http.MethodPut, native.Scope()+"/budget-override", request, &replay)
		var apiError *api.APIError
		if !errors.As(err, &apiError) || apiError.Status != http.StatusConflict {
			t.Fatalf("changed replay: %v", err)
		}
		request.IdempotencyKey, request.PerDayMaxUSD = "example-over-policy", 301
		err = owner.client.Request(t.Context(), http.MethodPut, native.Scope()+"/budget-override", request, &replay)
		if !errors.As(err, &apiError) || apiError.Status != http.StatusUnprocessableEntity {
			t.Fatalf("server cap validation: %v", err)
		}
		request.IdempotencyKey, request.PerDayMaxUSD, request.ExpiresAt = "example-over-lifetime", 210, now.Add(49*time.Hour)
		err = owner.client.Request(t.Context(), http.MethodPut, native.Scope()+"/budget-override", request, &replay)
		if !errors.As(err, &apiError) || apiError.Status != http.StatusUnprocessableEntity {
			t.Fatalf("server lifetime validation: %v", err)
		}
		for _, denied := range []struct {
			token, organization string
			status              int
		}{{"", organization, http.StatusUnauthorized}, {token, "org_foreign", http.StatusNotFound}} {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v2/organizations/"+denied.organization+"/projects/prj_quiet/budget-override", nil)
			if err != nil {
				t.Fatal(err)
			}
			if denied.token != "" {
				req.Header.Set("Authorization", "Bearer "+denied.token)
			}
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != denied.status {
				t.Fatalf("denial=%d want=%d", response.StatusCode, denied.status)
			}
		}
		worker, err := api.New(api.Config{URL: server.URL, HTTPClient: server.Client(), TokenSource: func() string { return token + "-worker" }})
		if err != nil {
			t.Fatal(err)
		}
		err = worker.Request(t.Context(), http.MethodPut, native.Scope()+"/budget-override", request, nil)
		if !errors.As(err, &apiError) || apiError.Status != http.StatusForbidden {
			t.Fatalf("worker mutation permission: %v", err)
		}
		if err := owner.ClearBudgetOverride(t.Context(), "busy"); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.ActiveBudgetOverride(t.Context(), "busy", now); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cleared override: %v", err)
		}
		server.Close()
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
	}
	copied := *config.Hosted
	copied.OrganizationID, copied.WorkOSOrganizationID = "org_example", "provider_org_example"
	config.Hosted = &copied
	reopened, err := hubserver.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(reopened.Handler())
	t.Cleanup(server.Close)
	client, err := api.New(api.Config{URL: server.URL, HTTPClient: server.Client(), TokenSource: func() string { return "budget-token" }})
	if err != nil {
		t.Fatal(err)
	}
	owner := &hubBudgetOverrideStore{client: client, organization: "org_example", projects: map[string]string{"quiet": "prj_quiet", "busy": "prj_busy"}}
	value, err := owner.ActiveBudgetOverride(t.Context(), "quiet", now)
	if err != nil || value.CreatedAt != firstCreated || value.Reason != "reviewed extension" || value.PerDayMaxUSD == nil || *value.PerDayMaxUSD != 210 {
		t.Fatalf("reopened override=%+v error=%v", value, err)
	}
	if _, err := owner.ActiveBudgetOverride(t.Context(), "busy", now); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reopened clear: %v", err)
	}
}
