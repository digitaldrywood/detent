package githubbinding_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const browserHostedOrganizationBase = "/api/v2/organizations/org_browser_preview"

func TestHostedRunnerCheckoutAssociation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, report, bound string
		repository          string
		status              int
		appBinding          bool
	}{
		{name: "new project without checkout", status: http.StatusOK},
		{name: "repository name normalized", repository: " Acme / Private ", status: http.StatusOK},
		{name: "matching checkout", report: "Acme/Private", status: http.StatusOK},
		{name: "other checkout does not prevent clone", report: "Other/Repository", status: http.StatusOK},
		{name: "existing binding is immutable", bound: "Other/Repository", status: http.StatusUnprocessableEntity},
		{name: "existing GitHub App binding is immutable", appBinding: true, status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := &scriptedReconcileBackend{}
			f := newBrowserBindingFixture(t)

			runner := enrollAppRunner(t, f, "Private checkout host", "test")
			base := browserHostedOrganizationBase + "/projects/" + f.project
			path := base + "/machines/" + string(runner.MachineID) + "/heartbeat"
			heartbeat := map[string]any{"display_name": "Private checkout host", "capacity": 1, "version": "test", "checkout_repository": test.report}
			if test.report != "" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat), http.StatusOK)
			}
			if test.bound != "" || test.appBinding {
				updateOnboardingFixtureProject(t, f.bindingFixture, "org_browser_preview", tracker.ProjectID(f.project), func(project *projectstore.Record) {
					project.Settings.CheckoutRepository = test.bound
					if test.appBinding {
						project.Settings.Repository, project.Settings.RepositoryEnabled = "Other/Repository", true
					}
				})
			}
			before := f.setupRequest(t, "owner", http.MethodGet, base+"/onboarding", nil)
			requireNativeStatus(t, before, http.StatusOK)
			var setup onboarding.Project
			decodeHubResponse(t, before, &setup)
			if test.bound == "" && !test.appBinding && (setup.RepositoryBindingReason == nil || setup.RepositoryBindingReason.Code != "repository_binding_required" || setup.Steps[1].State == "ready") {
				t.Fatalf("unbound setup = %+v", setup)
			}
			repository := "Acme/Private"
			if test.repository != "" {
				repository = test.repository
			}
			expectedRevision := "1"
			if test.bound != "" || test.appBinding {
				expectedRevision = "2"
			}
			request := map[string]any{"idempotency_key": "checkout-association", "expected_revision": expectedRevision, "repository": repository, "source": "runner_checkout"}
			requireNativeStatus(t, f.setupRequest(t, "viewer", http.MethodPost, base+"/onboarding/repository", request), http.StatusForbidden)
			response := f.setupRequest(t, "owner", http.MethodPost, base+"/onboarding/repository", request)
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				return
			}
			response = f.setupRequest(t, "owner", http.MethodGet, base+"/integration", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var integration ProjectIntegration
			decodeHubResponse(t, response, &integration)
			if integration.CheckoutRepository != "Acme/Private" || integration.RepositoryEnabled || integration.Repository != "" || integration.Revision != 2 {
				t.Fatalf("association = %+v", integration)
			}
			if integration.GitHubTransportAvailable == nil || *integration.GitHubTransportAvailable {
				t.Fatalf("hosted transport = %+v", integration)
			}
			if integration.CheckoutVerified != (test.report == "Acme/Private") {
				t.Fatalf("checkout verified = %v before runner report", integration.CheckoutVerified)
			}
			requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPost, base+"/onboarding/repository", request), http.StatusOK)
			response = performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/org_browser_preview/projects/"+f.project, runner.Credential, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var definition tracker.NativeProject
			decodeHubResponse(t, response, &definition)
			if definition.Repository != "Acme/Private" || definition.CloneURL != "https://github.com/Acme/Private.git" {
				t.Fatalf("runner project = %+v", definition)
			}
			response = f.setupRequest(t, "owner", http.MethodGet, base+"/onboarding", nil)
			requireNativeStatus(t, response, http.StatusOK)
			setup = onboarding.Project{}
			decodeHubResponse(t, response, &setup)
			if setup.RepositoryBindingReason != nil || setup.Repository != definition.Repository || setup.Steps[1].State != "ready" {
				t.Fatalf("bound setup disagrees with runner project: %+v", setup)
			}
			heartbeat["checkout_repository"] = "https://alice:private-secret@github.com/Acme/Private.git"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if strings.Contains(response.Body.String(), "private-secret") || strings.Contains(response.Body.String(), runner.Credential) {
				t.Fatal("invalid checkout report exposed credentials")
			}
			heartbeat["checkout_repository"] = "Acme/Private"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat), http.StatusOK)
			var reported string
			if err := f.repository.ReadTenant(t.Context(), "org_browser_preview", corestore.Serving, func(r tenantstore.Reader) error {
				report, err := r.RunnerCheckoutReport(t.Context(), runner.RunnerID, tracker.ProjectID(f.project))
				reported = report.Repository
				return err
			}); err != nil || reported != "Acme/Private" {
				t.Fatalf("checkout report = %q: %v", reported, err)
			}
			response = f.setupRequest(t, "owner", http.MethodGet, base+"/integration", nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &integration)
			if !integration.CheckoutVerified {
				t.Fatal("matching checkout was not verified")
			}
			if test.report == "" {
				for _, observation := range []string{"stale checkout", "revoked runner"} {
					if err := f.repository.WriteTenant(t.Context(), "org_browser_preview", corestore.Serving, func(w tenantstore.Writer) error {
						if observation == "stale checkout" {
							report, err := w.RunnerCheckoutReport(t.Context(), runner.RunnerID, tracker.ProjectID(f.project))
							if err != nil {
								return err
							}
							report.ReportedAt = f.config.Clock().Add(-10 * time.Minute)
							return w.ReplaceRunnerCheckoutReport(t.Context(), report)
						}
						identity, err := w.RunnerIdentity(t.Context(), runner.RunnerID)
						if err != nil {
							return err
						}
						return w.RevokeAccessCredential(t.Context(), identity.TokenID, f.config.Clock())
					}); err != nil {
						t.Fatal(err)
					}
					response = f.setupRequest(t, "owner", http.MethodGet, base+"/integration", nil)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &integration)
					if integration.CheckoutVerified {
						t.Fatalf("%s verified a checkout", observation)
					}
					if observation == "stale checkout" {
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat), http.StatusOK)
						response = f.setupRequest(t, "owner", http.MethodGet, base+"/integration", nil)
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &integration)
						if !integration.CheckoutVerified {
							t.Fatal("refresh did not restore checkout verification before revocation")
						}
					}
				}
			}
			response = f.setupRequest(t, "owner", http.MethodGet, base+"/integration", nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &integration)
			if integration.Repository != "" || integration.RepositoryEnabled {
				t.Fatalf("checkout association created a GitHub integration: %+v", integration)
			}

			if calls := backend.Requests(); len(calls) != 0 {
				t.Fatalf("hosted checkout association called GitHub: %+v", calls)
			}
		})
	}
}
