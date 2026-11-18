package labels_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/labels"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeLabelCatalogue(t *testing.T) {
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	service := openHub(t, admin, repository)
	createProject := func(name, key string) (nativeOrganization, tracker.NativeProject) {
		t.Helper()
		organization := nativeOrganization{ID: tracker.OrganizationID("org_" + key), Name: name, Slug: key}
		if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			now := time.Now()
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization.ID, Name: organization.Name, Slug: organization.Slug, CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization.ID, ProviderID: "provider_" + key, AllocationGeneration: 1}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: "bootstrap-admin", Name: "Example admin", Hash: apikey.HashToken(testHubAdminToken), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now, Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization.ID, ID: "other-admin", Name: "Other admin", Hash: apikey.HashToken(testHubAdminToken + "_other"), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now, Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateBillingPlan(t.Context(), billingstore.Plan{OrganizationID: organization.ID, PlanReference: billingstore.PlanReference{ID: "example", Version: 1}, Name: "Example plan", Features: []string{"collaboration"}, Allowances: map[string]int64{"api_mutations": 100}}); err != nil {
				return err
			}
			return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization.ID, Base: billingstore.PlanReference{ID: "example", Version: 1}}, 0)
		}); err != nil {
			t.Fatal(err)
		}
		projects := "/api/v2/organizations/" + string(organization.ID) + "/projects"
		response := performHubAPIRequest(t, service, http.MethodPost, projects, testHubAdminToken, map[string]any{"idempotency_key": key, "name": name + " project", "states": hubtest.States()})
		requireNativeStatus(t, response, http.StatusOK)
		var project tracker.NativeProject
		decodeHubResponse(t, response, &project)
		if err := repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
			record, err := w.ProjectRecord(t.Context(), project.ID)
			if err != nil {
				return err
			}
			record.Settings.DuplicateChallenge = false
			_, err = w.UpdateProjectRecord(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return organization, project
	}
	firstOrganization, firstProject := createProject("First organization", "first-project")
	secondOrganization, secondProject := createProject("Second organization", "second-project")
	firstBase := "/api/v2/organizations/" + string(firstOrganization.ID) + "/projects/" + string(firstProject.ID)
	assertCatalog := func(base string, want []labels.Label) {
		t.Helper()
		response := performHubAPIRequest(t, service, http.MethodGet, base+"/labels", testHubAdminToken, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var catalog labels.List
		decodeHubResponse(t, response, &catalog)
		if catalog.Items == nil || !slices.Equal(catalog.Items, want) {
			t.Fatalf("catalog=%+v want=%+v", catalog.Items, want)
		}
	}
	assertCatalog(firstBase, []labels.Label{})
	createIssue := func(organization nativeOrganization, project tracker.NativeProject, key, title string, labels []string) tracker.NativeIssue {
		t.Helper()
		path := "/api/v2/organizations/" + string(organization.ID) + "/projects/" + string(project.ID) + "/work-items"
		token := testHubAdminToken
		if key == "target" {
			token += "_other"
		}
		response := performHubAPIRequest(t, service, http.MethodPost, path, token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: title, Body: workItemRecordBody, Labels: labels})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		return issue
	}
	source := createIssue(firstOrganization, firstProject, "source", "Source issue", []string{"feature", "feature", "effort:medium"})
	target := createIssue(firstOrganization, firstProject, "target", "Target issue", []string{"feature", "another", "priority:high"})
	foreign := createIssue(secondOrganization, secondProject, "foreign", "Foreign issue", []string{"other"})
	if err := repository.ReadTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		labels, err := r.LabelNames(t.Context(), firstProject.ID, corestore.LabelCursor{}, 10)
		if err != nil {
			return err
		}
		if !slices.Equal(labels.Items, []string{"another", "effort:medium", "feature", "priority:high"}) {
			t.Fatalf("label membership=%+v", labels)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertCatalog(firstBase, []labels.Label{{Name: "feature", Count: 2, Color: "#8a6bbf"}, {Name: "another", Count: 1, Color: labels.Color("another")}})
	secondBase := "/api/v2/organizations/" + string(secondOrganization.ID) + "/projects/" + string(secondProject.ID)
	assertCatalog(secondBase, []labels.Label{{Name: "other", Count: 1, Color: labels.Color("other")}})
	for _, test := range []struct {
		principal string
		labels    []labels.Label
	}{
		{principal: source.Actor.PrincipalID, labels: []labels.Label{{Name: "feature", Count: 1, Color: labels.Color("feature")}}},
		{principal: target.Actor.PrincipalID, labels: []labels.Label{{Name: "another", Count: 1, Color: labels.Color("another")}, {Name: "feature", Count: 1, Color: labels.Color("feature")}}},
		{principal: "other-reporter", labels: []labels.Label{}},
		{labels: []labels.Label{}},
	} {
		recorder := httptest.NewRecorder()
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, firstBase+"/labels", nil), recorder)
		handler := labels.ListHandler(labels.HTTPDeps{
			Service: func() *labels.Service { return labels.New(labels.Deps{Repository: repository}) },
			Scope: func(echo.Context) hubapi.Scope {
				return hubapi.NewScope(hubapi.Facts{Organization: firstOrganization.ID, Project: firstProject.ID, Actor: tracker.Actor{PrincipalID: test.principal}, HostedRole: "reporter"}, nil)
			},
			ValidateQuery: validateQuery,
			Error:         hubtest.HTTPError,
		})
		if err := handler(c); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, recorder, http.StatusOK)
		var catalog labels.List
		decodeHubResponse(t, recorder, &catalog)
		if catalog.Items == nil || !slices.Equal(catalog.Items, test.labels) {
			t.Fatalf("reporter %q catalog=%+v want=%+v", test.principal, catalog.Items, test.labels)
		}
	}

	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, firstBase+"/labels?colour=red", testHubAdminToken, nil), http.StatusUnprocessableEntity)
	mismatched := "/api/v2/organizations/" + string(firstOrganization.ID) + "/projects/" + string(secondProject.ID) + "/labels"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, mismatched, testHubAdminToken, nil), http.StatusNotFound)
	updatedLabels := []string{"planning"}
	labelUpdate := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "update-labels"}, ExpectedRevision: 1, Labels: &updatedLabels}
	labelPath := itemPathFor(firstOrganization.ID, firstProject.ID, source.WorkItemID)
	changed := performHubAPIRequest(t, service, http.MethodPatch, labelPath, testHubAdminToken, labelUpdate)
	requireNativeStatus(t, changed, http.StatusOK)
	repeated := performHubAPIRequest(t, service, http.MethodPatch, labelPath, testHubAdminToken, labelUpdate)
	requireNativeStatus(t, repeated, http.StatusOK)
	if changed.Body.String() != repeated.Body.String() {
		t.Fatal("label replay changed response")
	}
	labelUpdate.IdempotencyKey = "stale-labels"
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, labelPath, testHubAdminToken, labelUpdate), http.StatusConflict)
	reserved := []string{"detent:workspace"}
	reservedUpdate := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "reserved-label"}, ExpectedRevision: 2, Labels: &reserved}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPatch, labelPath, testHubAdminToken, reservedUpdate), http.StatusUnprocessableEntity)
	assertCatalog(firstBase, []labels.Label{{Name: "another", Count: 1, Color: labels.Color("another")}, {Name: "feature", Count: 1, Color: "#8a6bbf"}, {Name: "planning", Count: 1, Color: labels.Color("planning")}})
	assertCatalog(secondBase, []labels.Label{{Name: "other", Count: 1, Color: labels.Color("other")}})
	emptyLabels := []string{}
	removeLabels := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "remove-labels"}, ExpectedRevision: foreign.Revision, Labels: &emptyLabels}
	removed := performHubAPIRequest(t, service, http.MethodPatch, itemPathFor(secondOrganization.ID, secondProject.ID, foreign.WorkItemID), testHubAdminToken, removeLabels)
	requireNativeStatus(t, removed, http.StatusOK)
	var cleared tracker.NativeIssue
	decodeHubResponse(t, removed, &cleared)
	if cleared.Revision != foreign.Revision+1 || len(cleared.Labels) != 0 {
		t.Fatalf("removed labels=%+v revision=%d", cleared.Labels, cleared.Revision)
	}
	assertCatalog(secondBase, []labels.Label{})
	if err := repository.ReadTenant(t.Context(), firstOrganization.ID, corestore.Serving, func(r tenantstore.Reader) error {
		labels, err := r.LabelNames(t.Context(), firstProject.ID, corestore.LabelCursor{}, 10)
		if err != nil {
			return err
		}
		if !slices.Equal(labels.Items, []string{"another", "feature", "planning", "priority:high"}) {
			t.Fatalf("updated label membership=%+v", labels)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
