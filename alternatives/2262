package githubbinding_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCheckoutBindingRefusals(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, profile, checkout, repository, code string
		revision                                  tracker.Revision
	}{
		{"invalid repository", "native", "", "bad", "repository_invalid", 1},
		{"wrong profile", "github_compatible", "", "example-org/project", "repository_profile_invalid", 1},
		{"immutable checkout", "native", "example-org/other", "example-org/project", "repository_already_bound", 1},
		{"immutable GitHub repository", "native", "", "example-org/project", "repository_already_bound", 1},
		{"stale revision", "native", "", "example-org/project", "revision_conflict", 2},
		{"active lease", "native", "", "example-org/project", "repository_binding_busy", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newBindingFixture(t, hubserver.Config{})
			f := fixture.project(t, "binding-refusal")
			service := f.service
			revision := tt.revision
			if tt.profile != "native" {
				project := tracker.ProjectID(hubapi.NewID("prj"))
				createOnboardingFixtureProject(t, fixture, f.project.OrganizationID, f.project.ID, project, "Compatible binding refusal", tt.profile)
				f.project.ID, f.project.Profile = project, tt.profile
				f.base = "/api/v2/organizations/" + string(f.project.OrganizationID) + "/projects/" + string(project)
			}
			if tt.checkout != "" || tt.name == "immutable GitHub repository" {
				project := updateOnboardingFixtureProject(t, fixture, f.project.OrganizationID, f.project.ID, func(project *projectstore.Record) {
					project.Settings.CheckoutRepository = tt.checkout
					if tt.name == "immutable GitHub repository" {
						project.Settings.Repository, project.Settings.RepositoryEnabled = "example-org/other", true
					}
				})
				revision = tracker.Revision(project.Settings.Revision)
			}
			if tt.name == "active lease" {
				issue := hubtest.Issue(t, fixture.repository, fixture.config.RunDetails, f.project, hubtest.IssueOptions{Title: "active work"})
				runner := hubtest.Runner(t, fixture.repository, f.project.OrganizationID, hubtest.RunnerOptions{Projects: []tracker.ProjectID{f.project.ID}})
				lease := hubtest.Lease(t, fixture.repository, runner, issue, hubtest.LeaseOptions{})
				if lease.ID == "" || lease.WorkItemID != issue.Issue.WorkItemID {
					t.Fatalf("binding fixture did not claim its work item: %+v", lease)
				}
			}
			response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/onboarding/repository", testHubAdminToken, bindNativeRepositoryRequest{Mutation: tracker.Mutation{IdempotencyKey: "refusal"}, ExpectedRevision: revision, Repository: tt.repository, Source: "runner_checkout"})
			var refusal hubapi.Error
			decodeHubResponse(t, response, &refusal)
			if response.Code < 400 || refusal.Code != tt.code || refusal.Message == "" {
				t.Fatalf("refusal = %d %s", response.Code, response.Body.String())
			}
			call := projectCall(t, "bind_native_repository", string(f.project.ID), "tool-refusal", operatortool.RepositoryInput{ExpectedRevision: revision, Repository: tt.repository, Source: "runner_checkout"})
			response = performHubWorkCall(t, service, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/mcp", testHubAdminToken, call.Name, map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{"name": call.Name, "arguments": call.Arguments, "_meta": json.RawMessage(fleetProtocolMeta)},
			})
			requireNativeStatus(t, response, http.StatusOK)
			var envelope struct {
				Result struct {
					IsError bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
					Failure operatortool.ConflictError `json:"structuredContent"`
				} `json:"result"`
			}
			decodeHubResponse(t, response, &envelope)
			if !envelope.Result.IsError || envelope.Result.Failure.Code != tt.code || (tt.code != "revision_conflict" && envelope.Result.Failure.Message != refusal.Message) || (tt.code == "revision_conflict" && (envelope.Result.Failure.Message == "" || envelope.Result.Failure.CurrentRevision != 1 || len(envelope.Result.Content) != 1 || !strings.Contains(envelope.Result.Content[0].Text, "revision_conflict"))) {
				t.Fatalf("tool refusal = %s", response.Body.String())
			}
		})
	}
}

func createOnboardingFixtureProject(t *testing.T, fixture bindingFixture, organization tracker.OrganizationID, template, id tracker.ProjectID, name, profile string) {
	t.Helper()
	if err := fixture.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		source, err := w.ProjectRecord(t.Context(), template)
		if err != nil {
			return err
		}
		_, err = w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: name, Profile: profile, RequireDependencies: source.Core.RequireDependencies}, States: source.States})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
func updateOnboardingFixtureProject(t *testing.T, fixture bindingFixture, organization tracker.OrganizationID, id tracker.ProjectID, mutate func(*projectstore.Record)) projectstore.Record {
	t.Helper()
	var stored projectstore.Record
	if err := fixture.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), id)
		if err != nil {
			return err
		}
		mutate(&project)
		stored, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return stored
}
