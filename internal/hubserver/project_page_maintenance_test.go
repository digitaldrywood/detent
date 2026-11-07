package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/selector"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProjectPageSchedulerAndLiveRoute(t *testing.T) {
	reads := 0
	details := &memoryRunDetails{beforeRead: func(_ context.Context) error { reads++; return nil }}
	service := newPostgresNativeService(t, Config{RunDetails: hubmaintenance.LimitDetails(details)})
	original := service.database.postgres
	details.beforeRead = func(context.Context) error {
		reads++
		if original.Stats().AcquiredConns() != 0 {
			t.Error("project object read held a database connection")
		}
		return nil
	}
	stored, _ := projectRecordDatabases.Load(original)
	admin := stored.(*sql.DB)
	wire := &boardWire{}
	measured := boardWireRepository(t, admin, wire)
	t.Cleanup(func() { service.config.TenantRepository = original; service.config.AccessRepository = original })
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			service.config.TenantRepository, service.config.AccessRepository = original, original
			fixture := newNativeFixture(t, service, org, "Example")
			service.config.Hosted = &HostedConfig{OrganizationID: string(org)}
			scheduler, err := service.newReadModelMaintenance()
			service.config.Hosted = nil
			if err != nil {
				t.Fatal(err)
			}
			for _, size := range []int{2, 50} {
				inherited := config.ModelSelection{Enabled: new(true), NormalModel: new(string(org)), Levels: map[string]config.ModelSelectionDefaults{}, Stages: map[string]config.ModelSelectionStage{}, Rules: &[]config.ModelSelectionRule{}}
				for i := 0; i < size; i++ {
					name := fmt.Sprintf("level_%03d", i)
					inherited.Levels[name] = config.ModelSelectionDefaults{Model: new("normal"), Effort: new("high"), MaxSessionTokens: new(int64(i + 1)), MaxSessionDurationMS: new((1 << 33) + i + 100)}
					inherited.Stages[name] = config.ModelSelectionStage{Level: new(name), IssueComplexity: new(i%2 == 0)}
					*inherited.Rules = append(*inherited.Rules, config.ModelSelectionRule{Name: name, Level: name, Roles: []string{"code"}, Efforts: []string{"high"}, Selector: selector.Selector{And: []selector.Selector{{Labels: selector.Labels{Include: []string{name}}, Or: []selector.Selector{{PriorityIn: []int{1, 2}}, {Fields: []selector.FieldEquals{{Name: "state", Value: "Todo"}}}}}}}})
				}
				override := config.ModelSelection{ComplexModel: new("example-complex"), BackendKinds: &[]string{}, Sources: map[string]string{}}
				prepare := func(value any) eventstore.Receipt {
					t.Helper()
					raw, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					receipt, err := eventstore.NewContent(details).PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org}, raw)
					if err != nil {
						t.Fatal(err)
					}
					return receipt
				}
				inheritedRef, overrideRef, workflowRef := prepare(inherited), prepare(override), prepare("# Workflow "+string(org))
				if err := original.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					models, err := w.OrganizationModels(t.Context())
					if err != nil {
						return err
					}
					if err := w.SetOrganizationModels(t.Context(), policystore.OrganizationModels{OrganizationID: org, Revision: models.Revision + 1, Payload: eventstore.Reference(inheritedRef.PayloadObjectKey, inheritedRef.PayloadSHA256, inheritedRef.PayloadBytes, inheritedRef.PayloadContentBytes)}, models.Revision); err != nil {
						return err
					}
					record, err := w.ProjectRecord(t.Context(), fixture.project.ID)
					if err != nil {
						return err
					}
					record.Settings.WorkflowKey, record.Settings.WorkflowSHA256, record.Settings.WorkflowBytes, record.Settings.WorkflowContentBytes = workflowRef.PayloadObjectKey, workflowRef.PayloadSHA256, workflowRef.PayloadBytes, workflowRef.PayloadContentBytes
					if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
						return err
					}
					record.Settings.ModelKey, record.Settings.ModelSHA256, record.Settings.ModelBytes, record.Settings.ModelContentBytes = overrideRef.PayloadObjectKey, overrideRef.PayloadSHA256, overrideRef.PayloadBytes, overrideRef.PayloadContentBytes
					return w.UpdateProjectModel(t.Context(), record.Settings)
				}); err != nil {
					t.Fatal(err)
				}
				var sourceModel *config.ModelSelection
				read := func(project tracker.ProjectID) tracker.NativeProject {
					t.Helper()
					response := performHubAPIRequest(t, service, http.MethodGet, fixture.base, testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var got tracker.NativeProject
					decodeHubResponse(t, response, &got)
					if got.OrganizationID != org || got.ID != project || got.WorkflowMarkdown != "# Workflow "+string(org) {
						t.Fatalf("wrong project: %+v", got)
					}
					var model config.ModelSelection
					if err := json.Unmarshal(got.ModelSelection, &model); err != nil {
						t.Fatal(err)
					}
					if model.NormalModel == nil || *model.NormalModel != string(org) || model.ComplexModel == nil || *model.ComplexModel != "example-complex" || model.BackendKinds == nil || len(*model.BackendKinds) != 0 {
						t.Fatal("model lost tenant selection or explicit empty override")
					}
					for name, want := range inherited.Levels {
						if !reflect.DeepEqual(model.Levels[name], want) {
							t.Fatalf("model lost level %s", name)
						}
					}
					for name, want := range inherited.Stages {
						if !reflect.DeepEqual(model.Stages[name], want) {
							t.Fatalf("model lost stage %s", name)
						}
					}
					if model.Rules == nil {
						t.Fatal("model lost rules")
					}
					for _, want := range *inherited.Rules {
						found := false
						for _, rule := range *model.Rules {
							if rule.Name == want.Name && reflect.DeepEqual(rule, want) {
								found = true
								break
							}
						}
						if !found {
							t.Fatalf("model lost nested rule %s", want.Name)
						}
					}
					normal, plan := model.Levels["normal"], model.Stages["plan"]
					if normal.Model == nil || *normal.Model != "normal" || normal.Effort == nil || *normal.Effort != "high" || plan.Model == nil || *plan.Model != "complex" || plan.Effort == nil || *plan.Effort != "low" {
						t.Fatal("model lost inherited organization defaults")
					}
					if sourceModel == nil {
						sourceModel = &model
					} else if !reflect.DeepEqual(model, *sourceModel) {
						t.Fatal("projected model differs from source-backed model")
					}

					return got
				}
				service.config.TenantRepository, service.config.AccessRepository = original, original
				reads = 0
				read(fixture.project.ID)
				if reads == 0 {
					t.Fatal("cold project did not fall back to current source")
				}
				for range 64 {
					if err := scheduler.Tick(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				service.config.TenantRepository, service.config.AccessRepository = measured, measured
				reads = 0
				wire.reset()
				read(fixture.project.ID)
				exchanges, statements := wire.stop()
				t.Logf("%s/%dx project GET statements=%d exchanges=%d object reads=%d", org, size, statements, exchanges, reads)
				if statements != 14 || exchanges != 12 {
					t.Errorf("project GET budget changed: %d statements / %d exchanges; want 14 / 12", statements, exchanges)
				}
				if reads != 0 {
					t.Errorf("ready project GET read %d objects; want 0", reads)
				}
			}
			service.config.TenantRepository, service.config.AccessRepository = original, original
			testProjectPagePublication(t, service, fixture)
		})
	}
}

func testProjectPagePublication(t *testing.T, service *Service, f nativeFixture) {
	t.Helper()
	repository := service.database.postgres
	org := f.project.OrganizationID

	var candidate projectstore.PageCandidate
	if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		candidate.Record, err = r.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		candidate.Models, err = r.OrganizationModels(t.Context())
		candidate.SourceAt = service.config.now()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	other := tracker.OrganizationID("org_first")
	if org == other {
		other = "org_first_second"
	}
	err := repository.WriteTenant(t.Context(), other, corestore.Background, func(w tenantstore.Writer) error {
		_, err := w.(projectstore.PageMaintainer).PublishProjectPage(t.Context(), candidate, projectstore.Page{})
		return err
	})
	if !errors.Is(err, corestore.ErrTenantMismatch) {
		t.Fatalf("cross tenant publish=%v", err)
	}

	if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.UpdateProjectModel(t.Context(), candidate.Record.Settings) }); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
		published, err := w.(projectstore.PageMaintainer).PublishProjectPage(t.Context(), candidate, projectstore.Page{})
		if published {
			t.Error("stale project source published")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Background, func(r tenantstore.Reader) error {
		var err error
		candidate, err = r.(projectstore.PageMaintainer).NextProjectPage(t.Context(), "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := repository.WriteTenant(t.Context(), org, corestore.Background, func(w tenantstore.Writer) error {
			published, err := w.(projectstore.PageMaintainer).PublishProjectPage(t.Context(), candidate, projectstore.Page{Body: string(org), Model: config.ModelSelection{Levels: map[string]config.ModelSelectionDefaults{}, Stages: map[string]config.ModelSelectionStage{}, Rules: &[]config.ModelSelectionRule{}}})
			if !published {
				t.Error("current or replayed project source rejected")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		record, err := r.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		page, ready, err := r.(projectstore.PageReader).ProjectPage(t.Context(), record)
		if err != nil {
			return err
		}
		if !ready || page.Body != string(org) || page.Model.Levels == nil || page.Model.Stages == nil || page.Model.Rules == nil {
			t.Errorf("project page lost tenant or explicit empty overrides: %+v", page)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
