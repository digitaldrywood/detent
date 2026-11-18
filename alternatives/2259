package githubbinding_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresCheckoutBindingCommands(t *testing.T) {
	now := time.Date(2026, 11, 11, 1, 0, 0, 0, time.UTC)
	importer := &bindingImporter{}
	backend := &scriptedReconcileBackend{}
	fixture := newBindingFixture(t, hubserver.Config{Clock: func() time.Time { return now }, ReconcileBackend: backend, ImportBackend: importer})
	repository := fixture.repository
	service := fixture.service
	reconciler, err := hubmaintenance.New(bindingRegistry{organization: string(fixture.organization)}, []hubmaintenance.Feature{service.GitHubReconciliationMaintenanceFeature()})
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []tracker.ProjectID{"first", "second"} {
		err := repository.WriteTenant(t.Context(), fixture.organization, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: fixture.organization, ID: project, Name: string(project), Profile: "native"}, States: hubtest.States()})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		base := "/api/v2/organizations/" + string(fixture.organization) + "/projects/" + string(project)
		options := base + "/integration/repository"
		binding := bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: "binding"}, ExpectedRevision: 1, Repository: "example-org/" + string(project), Source: "runner_checkout"}
		first, err := bindingCommand(t, service, options, binding)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := bindingCommand(t, service, options, binding)
		if err != nil || string(replay) != string(first) {
			t.Fatalf("binding replay=%s error=%v", replay, err)
		}
		record, err := fixture.readProject(t.Context(), project)
		if err != nil {
			t.Fatal(err)
		}
		if record.Settings.CheckoutRepository != binding.Repository || record.Settings.Revision != 2 {
			t.Fatalf("binding=%+v", record.Settings)
		}
		t.Run(string(project)+"/sync binding fence and rebind", func(t *testing.T) {
			checkoutNode := "R_checkout_" + string(project)
			checkoutSource := reconcileStep{snapshot: ReconcileSnapshot{Repository: RepositorySource{NodeID: checkoutNode, Owner: "example-org", Name: string(project), UpdatedAt: now}}}
			backend.steps = []reconcileStep{checkoutSource, checkoutSource, checkoutSource}
			backend.requests = nil
			readOutcome := func() (projectstore.GitHubReconciliation, error) {
				t.Helper()
				var row projectstore.GitHubReconciliation
				err := repository.ReadTenant(t.Context(), fixture.organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					row, err = r.(projectstore.GitHubReconciliationReader).GitHubReconciliation(t.Context(), project)
					return err
				})
				return row, err
			}
			githubBinding := bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: "different-checkout"}, ExpectedRevision: 2, Repository: "example-org/sync-" + string(project)}
			githubOptions := base + "/integration/repository"
			_, err := bindingCommand(t, service, githubOptions, githubBinding)
			var refusal *hubapi.Error
			if !errors.As(err, &refusal) || refusal.Code != "repository_already_bound" {
				t.Fatalf("different checkout binding was not refused: %v", err)
			}
			if row, err := readOutcome(); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("refused binding wrote an outcome: %+v, %v", row, err)
			}
			unchanged, err := fixture.readProject(t.Context(), project)
			if err != nil || !reflect.DeepEqual(unchanged.Settings, record.Settings) {
				t.Fatalf("refused binding changed project settings: %+v, %v", unchanged.Settings, err)
			}

			githubBinding.IdempotencyKey, githubBinding.Repository = "sync-bind", binding.Repository
			response, err := bindingCommand(t, service, githubOptions, githubBinding)
			if err != nil {
				t.Fatal(err)
			}
			var result ProjectIntegration
			if err := json.Unmarshal(response, &result); err != nil || result.Repository != binding.Repository || result.CheckoutRepository != binding.Repository || result.Revision != 3 {
				t.Fatalf("sync binding=%s error=%v", response, err)
			}
			if row, err := readOutcome(); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("binding wrote an outcome before scheduled verification: %+v, %v", row, err)
			}
			replayed, err := bindingCommand(t, service, githubOptions, githubBinding)
			if err != nil || string(replayed) != string(response) {
				t.Fatalf("sync binding replay=%s error=%v", replayed, err)
			}

			if err := reconciler.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			outcome, err := readOutcome()
			if err != nil || outcome.Repository != binding.Repository || outcome.NodeID != checkoutNode || outcome.LastSuccessfulSweepAt == nil || !outcome.LastSuccessfulSweepAt.Equal(now) || outcome.LastFailureAt != nil {
				t.Fatalf("scheduled checkout outcome=%+v error=%v", outcome, err)
			}

			githubBinding.IdempotencyKey, githubBinding.ExpectedRevision = "sync-rebind", 3
			response, err = bindingCommand(t, service, githubOptions, githubBinding)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(response, &result); err != nil || result.Repository != binding.Repository || result.CheckoutRepository != binding.Repository || result.Revision != 4 {
				t.Fatalf("sync rebind=%s error=%v", response, err)
			}
			if row, err := readOutcome(); err != nil || !reflect.DeepEqual(row, outcome) {
				t.Fatalf("rebind changed the current checkout outcome: %+v, %v", row, err)
			}

			githubBinding.IdempotencyKey, githubBinding.ExpectedRevision, githubBinding.Repository = "other-sync", 4, "example-org/other"
			_, err = bindingCommand(t, service, githubOptions, githubBinding)
			if !errors.As(err, &refusal) || refusal.Code != "repository_already_bound" {
				t.Fatalf("different existing sync binding was not refused: %v", err)
			}
			requests := backend.Requests()
			if len(requests) != 3 || requests[0].Repository.Name != string(project) || requests[1].Repository.Name != string(project) || requests[2].Repository.Name != string(project) || len(importer.requests) != 0 {
				t.Fatalf("provider requests did not follow binding and checkout identity: %+v", requests)
			}
		})
	}
}

func bindingCommand(t *testing.T, service *hubserver.Service, path string, input bindNativeRepositoryRequest) (json.RawMessage, error) {
	t.Helper()
	response := performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, input)
	raw := json.RawMessage(response.Body.Bytes())
	if response.Code == http.StatusOK {
		return raw, nil
	}
	var failure hubapi.Error
	decodeHubResponse(t, response, &failure)
	failure.Status, failure.Public = response.Code, true
	return nil, &failure
}
func (f bindingFixture) readProject(ctx context.Context, project tracker.ProjectID) (projectstore.Record, error) {
	var record projectstore.Record
	err := f.repository.ReadTenant(ctx, f.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		record, err = r.ProjectRecord(ctx, project)
		return err
	})
	return record, err
}

type bindingImporter struct {
	requests []githubcontract.GitHubImportRequest
}

func (b *bindingImporter) FetchImportPage(_ context.Context, request githubcontract.GitHubImportRequest) (githubcontract.GitHubImportPage, error) {
	b.requests = append(b.requests, request)
	return githubcontract.GitHubImportPage{}, nil
}

type bindingRegistry struct{ organization string }

func (r bindingRegistry) Page(_ context.Context, after string, limit int) ([]hubmaintenance.Tenant, error) {
	if limit <= 0 || r.organization <= after {
		return nil, nil
	}
	return []hubmaintenance.Tenant{{Organization: r.organization, Generation: 1}}, nil
}
