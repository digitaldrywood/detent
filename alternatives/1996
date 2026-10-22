package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOnboardingProgressRetryAndProjectIsolation(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "setup")
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other")
	request := map[string]any{"idempotency_key": "setup-1", "progress": onboarding.Progress{Repository: "existing", Doctor: true, Artifacts: "local"}}
	for _, tt := range []struct {
		name, path, token string
		body              any
		status            int
	}{
		{"save", f.base, f.token, request, http.StatusOK},
		{"retry", f.base, f.token, request, http.StatusOK},
		{"stale", f.base, f.token, map[string]any{"idempotency_key": "setup-2", "progress": onboarding.Progress{Repository: "generate"}}, http.StatusConflict},
		{"other project", other.base, f.token, request, http.StatusNotFound},
		{"unknown input", f.base, f.token, map[string]any{"idempotency_key": "setup-secret", "progress": map[string]any{"repository": "workflow source"}}, http.StatusUnprocessableEntity},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodPut, tt.path+"/onboarding", tt.token, tt.body)
			requireNativeStatus(t, response, tt.status)
		})
	}
	cfg := f.service.config
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = openTestService(t, cfg)
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var setup onboarding.Project
	decodeHubResponse(t, response, &setup)
	if setup.Progress.Revision != 1 || !setup.Progress.Doctor || setup.Progress.Repository != "existing" || setup.Progress.Artifacts != "local" || setup.Steps[3].Name != "Run files" || setup.Steps[3].State != "ready" || setup.Ready {
		t.Fatalf("saved state = %+v", setup)
	}
	response = performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/onboarding", other.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &setup)
	if setup.Progress.Revision != 0 {
		t.Fatal("setup leaked to another project")
	}
}

func TestOnboardingRepositoryPoliciesAndRunners(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, kind, scope                  string
		auto, match, admin, differentAdmin bool
	}{
		{"human review", "human_review", "projects", false, true, false, false},
		{"auto merge missing tag", "command", "projects", true, false, false, false},
		{"organization runner without project grant", "human_review", "organization", false, true, false, false},
		{"administrable sibling project runner", "human_review", "projects", false, true, true, false},
		{"unowned sibling project runner", "human_review", "projects", false, true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", tt.name)
			r := prepareRunnerForScope(t, f, tt.scope, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
			adminToken := testHubAdminToken
			if tt.differentAdmin {
				admin, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "other-runner-administrator", Scope: apiScopeAdmin})
				if err != nil {
					t.Fatal(err)
				}
				adminToken = admin.Token
			}
			descriptor := hubTestPolicy()
			descriptor.Gates.Kind, descriptor.Gates.AutoPromote = tt.kind, tt.auto
			if tt.kind == "human_review" {
				descriptor.Gates.AutomatedReview = ""
			}
			if !tt.match {
				descriptor.Requirements.RequiredTags = []string{"gpu"}
			}
			descriptor = descriptor.WithID()
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			decodeHubResponse(t, response, &setup)
			if len(setup.Runners) != 1 || setup.Runners[0].Runner.RunnerID != r.binding.RunnerID || setup.Runners[0].OtherProjectCount != 0 {
				t.Fatalf("project-scoped runner missing from its routed project: %+v", setup.Runners)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", adminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &setup)
			if setup.Policy == nil || setup.Policy.Policy.ID != descriptor.ID || len(setup.Runners) != 1 {
				t.Fatalf("readiness = %+v", setup)
			}
			if (len(setup.Runners[0].Exclusions) == 0) != tt.match {
				t.Fatalf("exclusions = %+v", setup.Runners[0].Exclusions)
			}
			if strings.Contains(response.Body.String(), r.redemption.Credential) || len(setup.Runners[0].Runner.Leases) != 0 {
				t.Fatal("private runner data exposed")
			}
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "other")
			token := other.token
			if tt.admin || tt.scope == "organization" {
				token = adminToken
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/onboarding", token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &setup)
			if tt.differentAdmin {
				response = performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var before runnerauth.Runner
				decodeHubResponse(t, response, &before)
				routing := before.Routing
				routing.ProjectIDs = append(slices.Clone(routing.ProjectIDs), other.project.ID)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", token, runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: routing}), http.StatusForbidden)
				if len(setup.Runners) != 0 {
					t.Fatalf("unowned runner exposed for project routing: %+v", setup.Runners)
				}
				return
			}
			if tt.admin || tt.scope == "organization" {
				if len(setup.Runners) != 1 || setup.Runners[0].Runner.RunnerID != r.binding.RunnerID || setup.Runners[0].OtherProjectCount != 1 {
					t.Fatalf("administrable or organization runner missing from ungranted project: %+v", setup.Runners)
				}
			} else if len(setup.Runners) != 0 {
				t.Fatalf("project-scoped runner exposed to ungranted project: %+v", setup.Runners)
			}
			if !tt.admin {
				return
			}
			if !slices.ContainsFunc(setup.Runners[0].Exclusions, func(e runnerauth.Exclusion) bool { return e.Code == "project_access_denied" }) {
				t.Fatalf("ungranted runner lacks routing exclusion: %+v", setup.Runners[0])
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, r.identityPath()+"/routing", token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var before runnerauth.Runner
			decodeHubResponse(t, response, &before)
			current := before.Routing
			current.ProjectRanks = map[tracker.ProjectID]int{f.project.ID: 7}
			response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", token, runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: current})
			requireNativeStatus(t, response, http.StatusOK)
			var routed runnerauth.Runner
			decodeHubResponse(t, response, &routed)
			current = routed.Routing
			current.ProjectIDs = append(current.ProjectIDs, other.project.ID)
			current.ProjectRanks[other.project.ID] = 0
			response = performHubAPIRequest(t, f.service, http.MethodPut, r.identityPath()+"/routing", token, runnerauth.RoutingChange{ExpectedRevision: routed.Revision, Routing: current})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &routed)
			if !slices.Contains(routed.ProjectIDs, f.project.ID) || !slices.Contains(routed.ProjectIDs, other.project.ID) || routed.ProjectRanks[f.project.ID] != 7 {
				t.Fatalf("routing lost sibling grant or rank: %+v", routed.Routing)
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/onboarding", token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &setup)
			if len(setup.Runners) != 1 || slices.ContainsFunc(setup.Runners[0].Exclusions, func(e runnerauth.Exclusion) bool { return e.Code == "project_access_denied" }) {
				t.Fatalf("routed runner remains unavailable: %+v", setup.Runners)
			}
			if setup.Runners[0].LocalChecks != nil || setup.Steps[2].State == "ready" {
				t.Fatalf("routing forged local validation evidence: %+v", setup)
			}
			for _, doctor := range []string{"failed", "passed"} {
				checks := runnerauth.LocalChecks{Checkout: "passed", Doctor: doctor, Provider: "passed", ProviderKinds: []string{"codex"}}
				response = performHubAPIRequest(t, f.service, http.MethodPost, other.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential, map[string]any{"capacity": 2, "version": "test", "local_checks": checks})
				requireNativeStatus(t, response, http.StatusOK)
				response = performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/onboarding", token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &setup)
				if len(setup.Runners) != 1 || setup.Runners[0].LocalChecks == nil {
					t.Fatalf("newly routed runner lost local checks: %+v", setup.Runners)
				}
				reported := setup.Runners[0].LocalChecks
				if reported.Checkout != "passed" || reported.Doctor != doctor || reported.Provider != "passed" || reported.ObservedAt.IsZero() || (setup.Steps[2].State == "ready") != (doctor == "passed" && setup.Policy != nil) {
					t.Fatalf("newly routed runner validation did not follow its report: %+v", setup)
				}
			}
			for _, visibility := range []struct {
				name, statement string
			}{
				{"removed", "UPDATE runner_identities SET removed_at='2026-01-01T00:00:00Z' WHERE id=?"},
				{"another organization", "UPDATE runner_identities SET removed_at=NULL, organization_id='org_onboarding_other' WHERE id=?"},
			} {
				if visibility.name == "another organization" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO organizations(id,name,created_at) VALUES ('org_onboarding_other','Other organization','2026-01-01T00:00:00Z')"); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), visibility.statement, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				response = performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/onboarding", token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &setup)
				if len(setup.Runners) != 0 {
					t.Fatalf("%s runner exposed: %+v", visibility.name, setup.Runners)
				}
			}
		})
	}
}

func TestHostedOnboardingPermissionsAndArtifacts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, role, grant    string
		read, write, admin   int
		routed, manage, owns bool
	}{
		{"owner", "owner", "write", 200, 200, 200, false, false, false},
		{"owner reuses own runner", "owner", "write", 200, 200, 200, false, false, true},
		{"owner with routed runner", "owner", "write", 200, 200, 200, true, false, false},
		{"admin", "admin", "write", 200, 200, 200, false, false, false},
		{"admin reuses own runner", "admin", "write", 200, 200, 200, false, false, true},
		{"member", "member", "write", 200, 200, 404, false, false, false},
		{"member managing runner projects only", "member", "write", 200, 200, 404, false, true, true},
		{"member managing unowned runner", "member", "write", 200, 200, 404, false, true, false},
		{"member managing routed runner", "member", "write", 200, 200, 404, true, true, false},
		{"member with routed runner", "member", "write", 200, 200, 404, true, false, false},
		{"viewer", "viewer", "read", 200, 404, 404, false, false, false},
		{"viewer with routed runner", "viewer", "read", 200, 404, 404, true, false, false},
		{"ungranted owner", "owner", "", 404, 404, 404, false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			ref, _, _ := seedHostedArtifact(t, f)
			owner := f.user(t, "runner-owner", "owner", "owner@example.test", "write", "")
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT 'prj_onboarding_sibling',organization_id,'Sibling project',profile,states_json,created_at FROM projects WHERE id=?", f.project); err != nil {
				t.Fatal(err)
			}
			user := f.user(t, "setup-user", tt.role, "setup@example.test", tt.grant, "")
			if tt.manage {
				f.grant(t, user, true, true)
				sibling := f
				sibling.project = "prj_onboarding_sibling"
				sibling.grant(t, user, true, true)
			}
			if tt.owns {
				owner = user
			}
			binding := runnerauth.NewBinding()
			projects := []tracker.ProjectID{"prj_onboarding_sibling"}
			if tt.routed {
				projects = append(projects, f.project)
			}
			enrollmentResponse := f.request(t, owner, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments", runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: projects, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat}, TTLSeconds: 60})
			requireNativeStatus(t, enrollmentResponse, http.StatusCreated)
			var enrollment runnerauth.Enrollment
			decodeHubResponse(t, enrollmentResponse, &enrollment)
			credential, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "example-host", DisplayName: "Example runner", Capacity: 2, Version: "test"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
			if tt.manage {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at) SELECT 'prj_onboarding_unrelated',organization_id,'Unrelated project',profile,states_json,created_at FROM projects WHERE id=?", f.project); err != nil {
					t.Fatal(err)
				}
			}
			route := "/api/v2/organizations/org_security/runners/" + binding.RunnerID + "/routing"
			routeRead := f.request(t, user, http.MethodGet, route, nil)
			wantRoute := http.StatusNotFound
			if tt.role == "owner" || tt.role == "admin" || tt.manage {
				wantRoute = http.StatusOK
			}
			requireNativeStatus(t, routeRead, wantRoute)
			if wantRoute == http.StatusOK && !tt.routed && !tt.owns {
				var before runnerauth.Runner
				decodeHubResponse(t, routeRead, &before)
				routing := before.Routing
				routing.ProjectIDs = append(slices.Clone(routing.ProjectIDs), f.project)
				requireNativeStatus(t, f.request(t, user, http.MethodPut, route, runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: routing}), http.StatusForbidden)
			}
			if wantRoute == http.StatusNotFound && strings.Contains(routeRead.Body.String(), "Sibling project") {
				t.Fatal("runner read disclosed an inaccessible project name")
			}
			requireNativeStatus(t, f.request(t, user, http.MethodGet, strings.Replace(route, "org_security", "org_other", 1), nil), http.StatusNotFound)
			response := f.request(t, user, http.MethodGet, f.base+"/onboarding", nil)
			requireNativeStatus(t, response, tt.read)
			if tt.read == 200 {
				var setup onboarding.Project
				decodeHubResponse(t, response, &setup)
				wantRunners := 0
				if (tt.role == "owner" || tt.role == "admin" || tt.manage) && (tt.routed || tt.owns) {
					wantRunners = 1
				}
				if len(setup.Runners) != wantRunners {
					t.Fatalf("runner administration boundary: got %+v, want %d runners", setup.Runners, wantRunners)
				}
				if tt.manage && tt.owns {
					response = f.request(t, user, http.MethodGet, route, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var before runnerauth.Runner
					decodeHubResponse(t, response, &before)
					issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
					if err != nil {
						t.Fatal(err)
					}
					expiry := time.Now().Add(time.Hour)
					for _, keyProjects := range [][]string{{string(f.project)}, {string(f.project), "prj_onboarding_sibling"}} {
						key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "scoped-runner-read-" + strings.Join(keyProjects, "-"), Scope: apiScopeOperator, Issuer: &issuer, KeyScope: apikey.ScopeWrite, ExpiresAt: &expiry, ProjectIDs: keyProjects, ProjectAccess: hostedProjectsSelected})
						if err != nil {
							t.Fatal(err)
						}
						want := http.StatusNotFound
						if len(keyProjects) == 2 {
							want = http.StatusOK
						}
						requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, route, key.Token, nil), want)
					}
					routing := before.Routing
					routing.ProjectIDs = append(slices.Clone(routing.ProjectIDs), f.project)
					response = f.request(t, user, http.MethodPut, route, runnerauth.RoutingChange{ExpectedRevision: before.Revision, Routing: routing})
					requireNativeStatus(t, response, http.StatusOK)
					var after runnerauth.Runner
					decodeHubResponse(t, response, &after)
					if !slices.Contains(after.ProjectIDs, "prj_onboarding_sibling") || !slices.Contains(after.ProjectIDs, f.project) || after.ProjectRanks["prj_onboarding_sibling"] != before.ProjectRanks["prj_onboarding_sibling"] {
						t.Fatalf("authorized routing lost existing grants or rank: before=%+v after=%+v", before.Routing, after.Routing)
					}
				}
				if len(setup.Artifacts) != 1 || setup.Artifacts[0].ServiceID != ref.ServiceID || setup.Artifacts[0].PublisherTokenID != "" {
					t.Fatalf("artifact binding = %+v", setup.Artifacts)
				}
			}
			requireNativeStatus(t, f.request(t, user, http.MethodPut, f.base+"/onboarding", map[string]any{"idempotency_key": "save", "progress": onboarding.Progress{Repository: "existing"}}), tt.write)
			descriptor := hubTestPolicy()
			requireNativeStatus(t, f.request(t, user, http.MethodPut, f.base+"/onboarding/policy", policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}), tt.admin)
			if tt.role == "owner" && tt.grant != "" {
				sibling := f
				sibling.project = "prj_onboarding_sibling"
				sibling.grant(t, user, true, true)
				issuer, _, err := f.service.hostedSessionCredential(t.Context(), auth.Session{Identity: user.identity.Hosted, Email: user.identity.Email}, apikey.HashToken(user.token))
				if err != nil {
					t.Fatal(err)
				}
				expiry := time.Now().Add(time.Hour)
				key, err := f.service.createAPITokenFor(t.Context(), tokenRequest{Name: "scoped-unassigned-runner-read", Scope: apiScopeOperator, Issuer: &issuer, KeyScope: apikey.ScopeWrite, ExpiresAt: &expiry, ProjectIDs: []string{string(f.project), string(sibling.project)}, ProjectAccess: hostedProjectsSelected})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM token_grants WHERE token_id=(SELECT token_id FROM runner_identities WHERE id=?)", binding.RunnerID); err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, f.request(t, user, http.MethodGet, route, nil), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, route, key.Token, nil), http.StatusNotFound)
			}

		})
	}
}

func (f *browserHostedFixture) setupRequest(t *testing.T, account, method, path string, value any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, f.server.URL+path, strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", f.server.URL)
	if cookie := f.cookies[account]; cookie != nil {
		request.AddCookie(cookie)
		request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func TestHostedProjectSetupJourney(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	first := f.createProject(t, "Resumable project")
	if again := f.createProject(t, "Resumable project"); again != first {
		t.Fatal("project retry created duplicate")
	}
	base := "/api/v2/organizations/org_browser_preview/projects/" + first
	request := map[string]any{"idempotency_key": "progress", "progress": onboarding.Progress{Repository: "existing", Doctor: true, Provider: true, Artifacts: "local"}}
	for range 2 {
		requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/onboarding", request), http.StatusOK)
	}
	response := f.page(t, "owner", "/api/v2/organizations/org_browser_preview/projects")
	requireNativeStatus(t, response, http.StatusOK)
	var projects []hostedProjectView
	decodeHubResponse(t, response, &projects)
	if projects == nil {
		t.Fatal("project list omitted the created project")
	}
	index := slices.IndexFunc(projects, func(project hostedProjectView) bool { return project.ID == first })
	if index < 0 || projects[index].Onboarding.Ready || len(projects[index].Onboarding.Steps) == 0 {
		t.Fatalf("project list readiness = %+v", projects)
	}
	response = f.page(t, "owner", base+"/onboarding")
	requireNativeStatus(t, response, http.StatusOK)
	var setup onboarding.Project
	decodeHubResponse(t, response, &setup)
	if setup.Progress.Repository != "existing" || setup.Ready {
		t.Fatalf("onboarding = %+v", setup)
	}
	issue := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "first-issue"}, Title: "First native issue", Body: "No GitHub issue required" + issueContractTestSections, State: "Todo"}
	for range 2 {
		requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPost, base+"/work-items", issue), http.StatusOK)
	}
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM issues WHERE project_id=?", first).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("issues=%d", count)
	}
	requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {first}, "write": {"true"}, "runner": {"true"}}), http.StatusSeeOther)
}

func TestHostedRunnerCheckoutAssociation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, report, bound string
		status              int
		appBinding          bool
	}{
		{name: "new project without checkout", status: http.StatusOK},
		{name: "matching checkout", report: "Acme/Private", status: http.StatusOK},
		{name: "other checkout does not prevent clone", report: "Other/Repository", status: http.StatusOK},
		{name: "existing binding is immutable", bound: "Other/Repository", status: http.StatusUnprocessableEntity},
		{name: "existing GitHub App binding is immutable", appBinding: true, status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			backend := &scriptedReconcileBackend{}
			f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false, func(cfg *Config) {
				cfg.ReconcileBackend = backend
			})
			grantAppRunners(t, f)
			runner := enrollAppRunner(t, f, "Private checkout host", "test")
			base := browserHostedOrganizationBase + "/projects/" + f.project
			path := base + "/machines/" + string(runner.MachineID) + "/heartbeat"
			heartbeat := map[string]any{"display_name": "Private checkout host", "capacity": 1, "version": "test", "checkout_repository": test.report}
			if test.report != "" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat), http.StatusOK)
			}
			if test.bound != "" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository=? WHERE id=?", test.bound, f.project); err != nil {
					t.Fatal(err)
				}
			}
			if test.appBinding {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO repositories(id, github_node_id, github_owner, github_name, created_at, updated_at) VALUES(1, 'repo_other', 'Other', 'Repository', ?, ?)", formatHubTime(time.Now()), formatHubTime(time.Now())); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id=1 WHERE id=?", f.project); err != nil {
					t.Fatal(err)
				}
			}
			request := map[string]any{"idempotency_key": "checkout-association", "expected_revision": "1", "repository": "Acme/Private", "source": "runner_checkout"}
			requireNativeStatus(t, f.setupRequest(t, "viewer", http.MethodPost, base+"/onboarding/repository", request), http.StatusNotFound)
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
			heartbeat["checkout_repository"] = "https://alice:private-secret@github.com/Acme/Private.git"
			response = performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if strings.Contains(response.Body.String(), "private-secret") || strings.Contains(response.Body.String(), runner.Credential) {
				t.Fatal("invalid checkout report exposed credentials")
			}
			heartbeat["checkout_repository"] = "Acme/Private"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat), http.StatusOK)
			var reported string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT repository FROM runner_checkout_repositories WHERE runner_id=? AND project_id=?", runner.RunnerID, f.project).Scan(&reported); err != nil || reported != "Acme/Private" {
				t.Fatalf("checkout report = %q: %v", reported, err)
			}
			response = f.setupRequest(t, "owner", http.MethodGet, base+"/integration", nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &integration)
			if !integration.CheckoutVerified {
				t.Fatal("matching checkout was not verified")
			}
			if test.report == "" {
				for _, observation := range []struct {
					name, sql string
					value     string
				}{
					{name: "revoked runner", sql: "UPDATE api_tokens SET revoked_at=? WHERE id=(SELECT token_id FROM runner_identities WHERE id=?)", value: formatHubTime(time.Now())},
					{name: "stale checkout", sql: "UPDATE runner_checkout_repositories SET reported_at=? WHERE runner_id=?", value: formatHubTime(time.Now().Add(-10 * time.Minute))},
				} {
					if _, err := f.service.database.db.ExecContext(t.Context(), observation.sql, observation.value, runner.RunnerID); err != nil {
						t.Fatal(err)
					}
					response = f.setupRequest(t, "owner", http.MethodGet, base+"/integration", nil)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &integration)
					if integration.CheckoutVerified {
						t.Fatalf("%s verified a checkout", observation.name)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=NULL WHERE id=(SELECT token_id FROM runner_identities WHERE id=?)", runner.RunnerID); err != nil {
						t.Fatal(err)
					}
				}
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM repositories").Scan(&count); err != nil || count != 0 {
				t.Fatalf("GitHub repository projection count = %d: %v", count, err)
			}
			if calls := backend.Requests(); len(calls) != 0 {
				t.Fatalf("hosted checkout association called GitHub: %+v", calls)
			}
		})
	}
}

func TestOnboardingCustomerBindingValidation(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	ref, _, _ := seedHostedArtifact(t, f)
	user := f.user(t, "setup-owner", "owner", "setup@example.test", "write", "")
	for _, tt := range []struct {
		name, mode, origin string
		want               int
	}{
		{"customer", "customer", "https://artifacts.example.test", 200},
		{"hosted without opt in", "hosted", "https://artifacts.example.test", 422},
		{"unsafe origin", "customer", "https://user:secret@example.test", 422},
	} {
		t.Run(tt.name, func(t *testing.T) {
			binding := artifact.Binding{ServiceID: ref.ServiceID, Origin: tt.origin, Mode: tt.mode, PublisherTokenID: "artifact-publisher"}
			requireNativeStatus(t, f.request(t, user, http.MethodPut, f.base+"/onboarding/artifact-services/"+ref.ServiceID, binding), tt.want)
		})
	}
}

func seedOnboardingBrowserJourney(t *testing.T) *browserHostedFixture {
	t.Helper()
	f := newBrowserHostedFixture(t, true)
	return seedHostedOnboardingJourney(t, f)
}

func seedHostedOnboardingJourney(t *testing.T, f *browserHostedFixture) *browserHostedFixture {
	t.Helper()
	organization := "/api/v2/organizations/" + f.service.config.Hosted.OrganizationID
	for _, project := range []string{f.project, f.privateProject} {
		requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {project}, "write": {"true"}, "runner": {"true"}}), http.StatusSeeOther)
	}
	base := organization + "/projects/" + f.project
	descriptor := hubTestPolicy()
	descriptor.Gates.Kind, descriptor.Gates.AutomatedReview = "human_review", ""
	descriptor = descriptor.WithID()
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}), http.StatusOK)
	automatic := hubTestPolicy()
	automatic.Gates.AutoPromote = true
	automatic.Requirements.RequiredTags = []string{"gpu"}
	automatic = automatic.WithID()
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, organization+"/projects/"+f.privateProject+"/onboarding/policy", policy.Change{ExpectedID: hubTestPolicy().ID, Policy: automatic}), http.StatusOK)
	binding := runnerauth.NewBinding()
	enrollmentRequest := runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project), tracker.ProjectID(f.privateProject)}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 900}
	response := f.setupRequest(t, "owner", http.MethodPost, organization+"/runner-enrollments", enrollmentRequest)
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "customer-build-host", DisplayName: "Customer build runner", Capacity: 2, Version: "test", OS: "linux", Architecture: "amd64"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, organization+"/runner-enrollments/redeem", enrollment.Token, redemption), http.StatusCreated)
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/onboarding", map[string]any{"idempotency_key": "ready", "progress": onboarding.Progress{Repository: "existing", Doctor: true, Provider: true, Artifacts: "local"}}), http.StatusOK)
	response = f.setupRequest(t, "owner", http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "first-run"}, Title: "First native run", Body: issueContractTestSections, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	response = performHubAPIRequest(t, f.service, http.MethodPost, base+"/claims", credential, tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: binding.MachineID, SessionID: "first-run", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
	requireNativeStatus(t, response, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, response, &lease)
	start := nativeStartedEvent(lease)
	finish := start
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 2, "succeeded"
	for _, event := range []tracker.NativeRunEvent{start, finish} {
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/work-items/"+string(issue.WorkItemID)+"/events", credential, event), http.StatusOK)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, base+"/leases/"+string(lease.ID)+"/release", credential, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
	return f
}

func TestHostedOnboardingFirstRun(t *testing.T) {
	t.Parallel()
	f := seedOnboardingBrowserJourney(t)
	for _, tt := range []struct{ project, contains string }{
		{f.project, `"latest_run":"succeeded"`},
		{f.privateProject, `"gpu"`},
	} {
		t.Run(tt.project, func(t *testing.T) {
			response := f.page(t, "owner", "/api/v2/organizations/org_browser_preview/projects/"+tt.project+"/onboarding")
			requireNativeStatus(t, response, http.StatusOK)
			if !strings.Contains(response.Body.String(), tt.contains) {
				t.Fatalf("missing %q", tt.contains)
			}
		})
	}
}

func TestOnboardingBrowserPreview(t *testing.T) {
	if os.Getenv("DETENT_ONBOARDING_BROWSER_PREVIEW") == "" {
		t.Skip("isolated onboarding browser preview")
	}
	f := seedOnboardingBrowserJourney(t)
	interrupted := f.createProject(t, "Interrupted setup")
	requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {interrupted}, "write": {"true"}, "runner": {"true"}}), http.StatusSeeOther)
	info := map[string]string{"owner": f.server.URL + "/__preview/account/owner", "success": f.server.URL + "/projects/" + f.project, "auto_merge": f.server.URL + "/projects/" + f.privateProject, "interrupted": f.server.URL + "/projects/" + interrupted, "stop": f.server.URL + "/__preview/stop"}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.TempDir(), "2194-browser.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Onboarding browser fixture: %s", f.server.URL)
	timer := time.NewTimer(15 * time.Minute)
	defer timer.Stop()
	select {
	case <-f.stop:
	case <-timer.C:
	case <-t.Context().Done():
	}
}

func TestOnboardingOffersThePoliciesRunnersReported(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "observed policy")
	clearNativeFixturePolicy(t, f)
	first := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	first.enroll(t)
	second := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	second.enroll(t)
	reader := prepareRunner(t, f, runnerauth.Read)
	reader.enroll(t)
	observedIDs := func(t *testing.T) ([]string, *policy.Approval) {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var setup onboarding.Project
		decodeHubResponse(t, response, &setup)
		ids := []string{}
		for _, observed := range setup.ObservedPolicies {
			ids = append(ids, observed.Policy.ID+"@"+observed.RunnerID)
		}
		return ids, setup.Policy
	}
	report := func(t *testing.T, token string, body any, status int) {
		t.Helper()
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/policy/observed", token, body), status)
	}
	original := hubTestPolicy()
	original.Workflow = &policy.Workflow{Source: "detent.yaml", States: nativeFixtureStates()}
	original = original.WithID()
	changed := original
	changed.Gates.AutoPromote = true
	changed.SourceRevision = strings.Repeat("b", 40)
	changed.Workflow = &policy.Workflow{Source: "detent.yaml", States: append(nativeFixtureStates(), tracker.NativeState{Name: "Repair", Dispatchable: true})}
	changed = changed.WithID()

	if ids, _ := observedIDs(t); len(ids) != 0 {
		t.Fatalf("observed policies before any report = %v", ids)
	}
	report(t, reader.redemption.Credential, original, http.StatusForbidden)
	report(t, first.redemption.Credential, policy.Descriptor{ID: "nope"}, http.StatusUnprocessableEntity)
	report(t, first.redemption.Credential, original, http.StatusNoContent)
	report(t, second.redemption.Credential, changed, http.StatusNoContent)
	ids, approved := observedIDs(t)
	want := []string{changed.ID + "@" + second.binding.RunnerID, original.ID + "@" + first.binding.RunnerID}
	if approved != nil || !slices.Equal(ids, want) {
		t.Fatalf("observed policies = %v, want %v (approved %+v)", ids, want, approved)
	}
	project, err := readNativeProject(t.Context(), f.service.database.db, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
	if err != nil || !reflect.DeepEqual(project.States, f.project.States) {
		t.Fatalf("unapproved runner report changed workflow: %#v, %v", project, err)
	}

	approveHubTestPolicy(t, f.service, f.base+"/policy", original)
	ids, approved = observedIDs(t)
	if approved == nil || approved.Policy.ID != original.ID || !slices.Equal(ids, want[:1]) {
		t.Fatalf("after approving the first runner's policy: observed %v, approved %+v", ids, approved)
	}

	report(t, first.redemption.Credential, changed, http.StatusNoContent)
	if ids, _ = observedIDs(t); len(ids) != 1 || !strings.HasPrefix(ids[0], changed.ID+"@") {
		t.Fatalf("two runners reporting the same descriptor = %v, want it once", ids)
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var setup onboarding.Project
	decodeHubResponse(t, response, &setup)
	if len(setup.ObservedPolicies[0].RunnerIDs) != 2 || setup.ObservedPolicies[0].Conflict {
		t.Fatalf("converged reports lost runner identities or remained conflicting: %+v", setup.ObservedPolicies)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, first.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodDelete, second.identityPath(), testHubAdminToken, nil), http.StatusNoContent)
	if ids, _ = observedIDs(t); len(ids) != 0 {
		t.Fatalf("removed runners retain actionable reports: %v", ids)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/onboarding/policy", testHubAdminToken, policy.Change{ExpectedID: original.ID, ObservedPolicyID: changed.ID}), http.StatusUnprocessableEntity)
}

func TestOnboardingReportedConfigurationDoesNotBlockRunner(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		selected bool
		legacy   bool
	}{{"default project", false, false}, {"selected project", true, false}, {"legacy approval default project", false, true}, {"legacy approval selected project", true, true}} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newNativeFixture(t, nil, "", "default project")
			target := f
			if test.selected {
				target = newNativeFixture(t, f.service, f.project.OrganizationID, "selected project")
			}
			runner := prepareRunner(t, target, runnerauth.Read, runnerauth.Heartbeat)
			runner.enroll(t)
			client, err := hubclient.New(hubclient.Config{URL: "https://pending-policy.example.test", TokenSource: func() string { return runner.redemption.Credential }, HTTPClient: &http.Client{Transport: policyAPITransport{service: f.service}}})
			if err != nil {
				t.Fatal(err)
			}
			scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{
				OrganizationID: f.project.OrganizationID, NativeProjects: map[string]tracker.ProjectID{"selected": target.project.ID},
				Machine: hubclient.Machine{ID: runner.binding.MachineID, Hostname: "policy-host", Capacity: 2, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute,
			})
			if err != nil {
				t.Fatal(err)
			}
			original := resolvedWorkflowPolicy(t, nativeFixtureStates(), strings.Repeat("a", 40), false)
			candidate := resolvedWorkflowPolicy(t, append(nativeFixtureStates(), policy.State{Name: "Merging", Dispatchable: true}, policy.State{Name: "Blocked"}), strings.Repeat("b", 40), false)
			if test.legacy {
				legacy, err := workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{Config: workflowconfig.Default()}, original)
				if err != nil {
					t.Fatal(err)
				}
				legacy.Authored, legacy.DefinitionSources = nil, nil
				original, err = workflowconfig.ResolvePolicy(legacy)
				if err != nil {
					t.Fatal(err)
				}
			}
			approveHubTestPolicy(t, f.service, target.base+"/policy", original)
			local, err := workflowconfig.ApplyNativePolicy(workflowconfig.Workflow{Config: workflowconfig.Default()}, candidate)
			if err != nil {
				t.Fatal(err)
			}
			source := &policy.RepositorySource{Repository: "acme/orders", Commit: strings.Repeat("b", 40)}
			check := func() error {
				resolved, err := scheduler.ResolveProjectWorkflow(t.Context(), "selected", local, source)
				if err != nil {
					return err
				}
				descriptor, err := workflowconfig.ResolvePolicy(resolved)
				if err != nil {
					return err
				}
				return scheduler.CheckProjectPolicyWithSource(t.Context(), "selected", "", descriptor, source)
			}
			if err := check(); err != nil {
				t.Fatalf("reported configuration blocked runner: %v", err)
			}
			read := func(base, token string) onboarding.Project {
				t.Helper()
				response := performHubAPIRequest(t, f.service, http.MethodGet, base+"/onboarding", token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var setup onboarding.Project
				decodeHubResponse(t, response, &setup)
				return setup
			}
			setup := read(target.base, target.token)
			if setup.Policy == nil || setup.Policy.Policy.ID != original.ID || len(setup.ObservedPolicies) != 1 {
				t.Fatalf("pending policy not offered alongside its approval: %+v", setup)
			}
			pending := setup.ObservedPolicies[0]
			if pending.Conflict || pending.PreviouslyApproved || pending.RunnerID != runner.binding.RunnerID || !reflect.DeepEqual(pending.Policy, candidate) {
				t.Fatalf("runner candidate cannot be approved exactly: %+v", pending)
			}
			if test.selected && len(read(f.base, f.token).ObservedPolicies) != 0 {
				t.Fatal("selected project's candidate leaked into the default project")
			}
			for _, invalid := range []struct {
				name   string
				change policy.Change
				status int
			}{
				{"unknown observation", policy.Change{ExpectedID: original.ID, ObservedPolicyID: "policy_" + strings.Repeat("f", 64)}, http.StatusUnprocessableEntity},
				{"stale approval", policy.Change{ObservedPolicyID: pending.Policy.ID}, http.StatusConflict},
				{"ambiguous input", policy.Change{ExpectedID: original.ID, ObservedPolicyID: pending.Policy.ID, Policy: pending.Policy}, http.StatusUnprocessableEntity},
			} {
				t.Run(invalid.name, func(t *testing.T) {
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, target.base+"/onboarding/policy", testHubAdminToken, invalid.change), invalid.status)
				})
			}
			change := policy.Change{ExpectedID: setup.Policy.Policy.ID, ObservedPolicyID: pending.Policy.ID}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, target.base+"/onboarding/policy", runner.redemption.Credential, change), http.StatusForbidden)
			if test.selected {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/onboarding/policy", testHubAdminToken, change), http.StatusUnprocessableEntity)
			}
			for range 2 {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, target.base+"/onboarding/policy", testHubAdminToken, change), http.StatusOK)
			}
			if err := check(); err != nil {
				t.Fatalf("same runner cannot load the approved candidate: %v", err)
			}
			applied := read(target.base, target.token)
			if applied.Policy == nil || !reflect.DeepEqual(applied.Policy.Policy, candidate) || len(applied.ObservedPolicies) != 0 {
				t.Fatalf("approval did not apply the reported descriptor: %+v", applied)
			}
		})
	}
}

func TestOnboardingRunnerLocalChecks(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "runner checks")
	clearNativeFixturePolicy(t, f)
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other checks")
	clearNativeFixturePolicy(t, other)
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	contexts := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/checkout-report-test", func(c echo.Context) error {
		contexts <- c.Request().Context()
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/checkout-report-test", testHubAdminToken, nil), http.StatusOK)
	ctx := operatortool.BindConnection(<-contexts, "checkout-report", "direct-test")
	executor := hubFleetExecutor{service: f.service}
	if err := executor.OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	for _, tt := range []struct {
		name, checkout, doctor, provider, setup, cause string
		valid                                          bool
	}{
		{"missing report", "", "", "", "", "", true},
		{"missing checkout", "failed", "pending", "pending", "", "", true},
		{"failed doctor", "passed", "failed", "passed", "", "", true},
		{"missing auth", "passed", "passed", "failed", "", "", true},
		{"checkout timeout", "failed", "pending", "pending", "failed", "context deadline exceeded", true},
		{"checkout auth", "failed", "pending", "pending", "failed", "authentication failed", true},
		{"checkout missing repository", "failed", "pending", "pending", "failed", "repository not found", true},
		{"success", "passed", "passed", "passed", "", "", true},
		{"failed setup", "passed", "passed", "passed", "failed", "", true},
		{"repaired setup", "passed", "passed", "passed", "passed", "", true},
		{"reject setup output", "passed", "passed", "passed", "secret-token", "", false},
		{"reject command output", "passed", "secret-token", "passed", "", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checks := runnerauth.LocalChecks{Checkout: tt.checkout, Doctor: tt.doctor, Provider: tt.provider, ProviderKinds: []string{"codex"}, Setup: tt.setup}
			body := map[string]any{"capacity": 1, "version": "test"}
			if tt.cause != "" {
				checks.CheckoutMessage = "Cannot clone repository: " + tt.cause
				checks.CheckoutFix = "Check the reported repository failure"
				problem := runnerauth.NewProblem("settings_invalid")
				problem.ProjectID, problem.Subject = string(f.project.ID), "checkout"
				problem.Message, problem.FixHint = checks.CheckoutMessage, checks.CheckoutFix
				problem.ErrorOutput = "git exited with status 128\n" + tt.cause
				body["problems"] = []runnerauth.Problem{problem}
			}
			if tt.checkout != "" {
				body["local_checks"] = checks
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, body)
			expected := http.StatusOK
			if !tt.valid {
				expected = http.StatusUnprocessableEntity
			}
			requireNativeStatus(t, response, expected)
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			decodeHubResponse(t, response, &setup)
			executionState := "ready"
			if tt.cause != "" {
				if setup.Runners[0].Runner.ConnectionHealth != "online" {
					t.Fatalf("checkout failure took runner offline: %+v", setup.Runners[0])
				}
			}
			if setup.Policy != nil || setup.Steps[0].State != executionState {
				t.Fatalf("enrollment lost runner health: %+v", setup)
			}
			if tt.valid && tt.checkout != "" && (setup.Runners[0].LocalChecks == nil || setup.Runners[0].LocalChecks.Doctor != tt.doctor || setup.Runners[0].LocalChecks.Setup != tt.setup || setup.Runners[0].LocalChecks.ObservedAt.IsZero()) {
				t.Fatalf("report=%+v", setup)
			}
			if tt.valid && (setup.Steps[2].State == "ready") != (tt.checkout != "" && checks.Passed() && setup.Policy != nil) {
				t.Fatalf("local validation forged success or lost passed evidence: %+v", setup)
			}
			if tt.cause != "" {
				if setup.Runners[0].LocalChecks.CheckoutMessage != checks.CheckoutMessage || setup.Runners[0].LocalChecks.CheckoutFix != checks.CheckoutFix {
					t.Fatalf("onboarding lost checkout guidance: %+v", setup.Runners[0])
				}
				result, err := executor.Execute(ctx, operatortool.Call{Name: operatortool.ListRunnerRouting, Arguments: json.RawMessage(`{"project_id":"` + string(f.project.ID) + `"}`)})
				if err != nil {
					t.Fatal(err)
				}
				var envelope struct {
					Data struct {
						Runners []runnerauth.Runner `json:"runners"`
					} `json:"data"`
				}
				if err := json.Unmarshal(result.Content, &envelope); err != nil {
					t.Fatal(err)
				}
				if len(envelope.Data.Runners) != 1 || !slices.ContainsFunc(envelope.Data.Runners[0].Problems, func(p runnerauth.Problem) bool {
					return p.ProjectID == string(f.project.ID) && p.Subject == "checkout" && p.Message == checks.CheckoutMessage && strings.Contains(p.ErrorOutput, "128\n"+tt.cause)
				}) {
					t.Fatalf("routing lost checkout cause: %s", result.Content)
				}
			}
			if tt.checkout == "" && setup.Runners[0].LocalChecks != nil {
				t.Fatal("missing report became diagnostic evidence")
			}
			if strings.Contains(response.Body.String(), r.redemption.Credential) || strings.Contains(response.Body.String(), "secret-token") {
				t.Fatal("sensitive report exposed")
			}
			response = performHubAPIRequest(t, f.service, http.MethodGet, other.base+"/onboarding", other.token, nil)
			decodeHubResponse(t, response, &setup)
			if len(setup.Runners) != 0 {
				t.Fatal("runner checks leaked to another project")
			}
		})
	}
}

type policyAPITransport struct{ service *Service }

func (transport policyAPITransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	transport.service.Handler().ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func TestNativeSharedConfigurationAcrossRunners(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "shared policy")
	httpClient := &http.Client{Transport: policyAPITransport{service: f.service}}
	makeRunner := func(host string, planning, validator, promotion bool) (*hubclient.Scheduler, workflowconfig.Workflow, *hubclient.NativeClient) {
		t.Helper()
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim)
		r.redemption.DisplayName, r.redemption.Hostname = host, host
		r.enroll(t)
		client, err := hubclient.New(hubclient.Config{URL: "https://shared-policy.example.test", TokenSource: func() string { return r.redemption.Credential }, HTTPClient: httpClient})
		if err != nil {
			t.Fatal(err)
		}
		native, err := client.Native(f.project.OrganizationID, f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		scheduler, err := hubclient.NewScheduler(client, hubclient.SchedulerConfig{
			OrganizationID: f.project.OrganizationID, NativeProjects: map[string]tracker.ProjectID{"shared": f.project.ID},
			Machine: hubclient.Machine{ID: r.binding.MachineID, Hostname: host, Capacity: 2, Version: "test"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		workflow, err := workflowconfig.ParseWorkflow([]byte(fmt.Sprintf("---\ntracker:\n  kind: hub_native\n  api_key: %s-credential\nworkspace:\n  root: %s/worktrees\nworker:\n  ssh_hosts: [local]\nhooks:\n  runner_setup: %s/setup.sh\n  before_run: %s/isolate.sh\nplan:\n  enabled: %t\ngate:\n  run: true\n  validator:\n    enabled: %t\nagent:\n  auto_promote:\n    enabled: %t\n---\nImplement the issue and preserve explicit human review holds.\n", host, host, host, host, planning, validator, promotion)))
		if err != nil {
			t.Fatal(err)
		}
		workflow.Config.Plan.Enabled, workflow.Config.Gate.Validator.Enabled, workflow.Config.Agent.AutoPromote.Enabled = planning, validator, promotion
		workflow.Config.Gate.Run = "true"
		workflow.Config.Agent.MaxConcurrentAgents = len(host)
		return scheduler, workflow, native
	}
	pro, proWorkflow, proClient := makeRunner("MacBook Pro", true, true, false)
	air, airWorkflow, airClient := makeRunner("MacBook Air", false, false, true)
	descriptors := make([]policy.Descriptor, 2)
	for i, runner := range []*hubclient.Scheduler{pro, air} {
		workflow := []workflowconfig.Workflow{proWorkflow, airWorkflow}[i]
		descriptor, err := workflowconfig.ResolvePolicy(workflow)
		if err != nil {
			t.Fatal(err)
		}
		descriptors[i] = descriptor
		if err := runner.CheckProjectPolicy(t.Context(), "shared", "", descriptor); err != nil {
			t.Fatalf("runner configuration report required approval: %v", err)
		}
	}
	read := func() onboarding.Project {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var setup onboarding.Project
		decodeHubResponse(t, response, &setup)
		return setup
	}
	initial := read()
	if initial.Policy == nil || initial.Policy.Policy.ID != descriptors[1].ID {
		t.Fatalf("latest runner configuration was not selected: %+v", initial.Policy)
	}
	if len(initial.ObservedPolicies) != 1 || initial.ObservedPolicies[0].Policy.ID != descriptors[0].ID || !initial.ObservedPolicies[0].PreviouslyApproved || !initial.ObservedPolicies[0].Conflict {
		t.Fatalf("historical conflicting report lost its provenance: %+v", initial.ObservedPolicies)
	}
	combined, err := workflowconfig.ParseWorkflowOverlay(proWorkflow.DefinitionSources.Workflow, []byte("---\nagent:\n  auto_promote:\n    enabled: true\n---\n"), "operator-approved.local.md")
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := workflowconfig.ResolvePolicy(combined)
	if err != nil {
		t.Fatal(err)
	}
	if err := pro.CheckProjectPolicy(t.Context(), "shared", "", descriptor); err != nil {
		t.Fatal(err)
	}
	for i, runner := range []*hubclient.Scheduler{pro, air} {
		local := []workflowconfig.Workflow{proWorkflow, airWorkflow}[i]
		resolved, err := runner.ResolveProjectWorkflow(t.Context(), "shared", local, nil)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := workflowconfig.ResolvePolicy(resolved)
		if err != nil {
			t.Fatal(err)
		}
		if err := runner.CheckProjectPolicy(t.Context(), "shared", "", actual); err != nil {
			t.Fatal(err)
		}
		if err := actual.Match(descriptor); err != nil {
			t.Fatal(err)
		}
		if !resolved.Config.Plan.Enabled || !resolved.Config.Gate.Validator.Enabled || !resolved.Config.Agent.AutoPromote.Enabled {
			t.Fatal("shared combined gates did not reach both runners")
		}
		if resolved.Config.Workspace.Root != local.Config.Workspace.Root || resolved.Config.Hooks != local.Config.Hooks || resolved.Config.Tracker.APIKey != local.Config.Tracker.APIKey || resolved.Config.Agent.MaxConcurrentAgents != local.Config.Agent.MaxConcurrentAgents {
			t.Fatal("shared definition replaced host configuration")
		}
	}
	for i, client := range []*hubclient.NativeClient{proClient, airClient} {
		approval, err := client.ProjectPolicy(t.Context())
		if err != nil || approval.Policy.ID != descriptor.ID {
			t.Fatalf("runner approval = %+v, %v", approval, err)
		}
		inspected, err := client.ResolveProjectWorkflow(t.Context(), []workflowconfig.Workflow{proWorkflow, airWorkflow}[i], nil)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := workflowconfig.ResolvePolicy(inspected)
		if err != nil || actual.Match(descriptor) != nil {
			t.Fatalf("authenticated inspection differs from runtime: %+v, %v", actual, err)
		}
	}
	if setup := read(); len(setup.ObservedPolicies) != 0 {
		t.Fatalf("converged runners retain stale mismatch reports: %+v", setup.ObservedPolicies)
	}
	if os.Getenv("DETENT_POLICY_CAPTURE") != "" {
		evidence := map[string]onboarding.Project{"conflict": initial, "resolved": read()}
		raw, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(os.TempDir(), "native-policy-evidence.json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunnerFirstOnboardingSteps(t *testing.T) {
	for _, test := range []struct {
		name                                      string
		runner, repository, checks, config, files bool
		states                                    []string
	}{
		{name: "no runner", states: []string{"action_required", "action_required", "action_required", "action_required"}},
		{name: "routed runner without checkout", runner: true, states: []string{"ready", "action_required", "action_required", "action_required"}},
		{name: "verified checkout without config", runner: true, repository: true, checks: true, states: []string{"ready", "ready", "action_required", "action_required"}},
		{name: "all checks passed", runner: true, repository: true, checks: true, config: true, states: []string{"ready", "ready", "ready", "action_required"}},
		{name: "run files saved", runner: true, repository: true, checks: true, config: true, files: true, states: []string{"ready", "ready", "ready", "ready"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", "runner-first")
			if !test.config {
				if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM project_policies WHERE scope=?", string(f.project.OrganizationID)+"/"+string(f.project.ID)); err != nil {
					t.Fatal(err)
				}
			}
			if test.repository {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository=? WHERE id=?", "example-org/example-project", f.project.ID); err != nil {
					t.Fatal(err)
				}
			}
			if test.runner {
				r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
				r.enroll(t)
				if test.checks {
					raw, err := json.Marshal(map[tracker.ProjectID]runnerauth.LocalChecks{f.project.ID: {Checkout: "passed", Doctor: "passed", Provider: "passed"}})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET local_checks_json=? WHERE id=?", string(raw), r.binding.RunnerID); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.files {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/onboarding", f.token, map[string]any{"idempotency_key": "run-files", "progress": onboarding.Progress{Repository: "existing", Doctor: true, Provider: true, Artifacts: "local"}}), http.StatusOK)
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			decodeHubResponse(t, response, &setup)
			if len(setup.Steps) != 4 {
				t.Fatalf("steps = %+v", setup.Steps)
			}
			for index, name := range []string{"Runner", "Repository", "Checkout", "Run files"} {
				if setup.Steps[index].Name != name || setup.Steps[index].State != test.states[index] {
					t.Fatalf("steps = %+v", setup.Steps)
				}
			}
			if setup.Ready != test.files {
				t.Fatalf("ready = %t", setup.Ready)
			}
		})
	}
}
