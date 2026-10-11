package hubserver

import (
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

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/onboarding"
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
	if setup.Progress.Revision != 1 || !setup.Progress.Doctor || setup.Ready {
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
		name, kind  string
		auto, match bool
	}{
		{"human review", "human_review", false, true},
		{"auto merge missing tag", "command", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newNativeFixture(t, nil, "", tt.name)
			r := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat)
			r.enroll(t)
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
			if setup.Policy == nil || setup.Policy.Policy.ID != descriptor.ID || len(setup.Runners) != 1 {
				t.Fatalf("readiness = %+v", setup)
			}
			if (len(setup.Runners[0].Exclusions) == 0) != tt.match {
				t.Fatalf("exclusions = %+v", setup.Runners[0].Exclusions)
			}
			if strings.Contains(response.Body.String(), r.redemption.Credential) || len(setup.Runners[0].Runner.Leases) != 0 {
				t.Fatal("private runner data exposed")
			}
		})
	}
}

func TestHostedOnboardingPermissionsAndArtifacts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, role, grant  string
		read, write, admin int
	}{
		{"owner", "owner", "write", 200, 200, 200},
		{"member", "member", "write", 200, 200, 404},
		{"viewer", "viewer", "read", 200, 404, 404},
		{"ungranted owner", "owner", "", 404, 404, 404},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newHostedSecurityFixture(t)
			ref, _, _ := seedHostedArtifact(t, f)
			user := f.user(t, "setup-user", tt.role, "setup@example.test", tt.grant, "")
			response := f.request(t, user, http.MethodGet, f.base+"/onboarding", nil)
			requireNativeStatus(t, response, tt.read)
			if tt.read == 200 {
				var setup onboarding.Project
				decodeHubResponse(t, response, &setup)
				if len(setup.Artifacts) != 1 || setup.Artifacts[0].ServiceID != ref.ServiceID || setup.Artifacts[0].PublisherTokenID != "" {
					t.Fatalf("artifact binding = %+v", setup.Artifacts)
				}
			}
			requireNativeStatus(t, f.request(t, user, http.MethodPut, f.base+"/onboarding", map[string]any{"idempotency_key": "save", "progress": onboarding.Progress{Repository: "existing"}}), tt.write)
			descriptor := hubTestPolicy()
			requireNativeStatus(t, f.request(t, user, http.MethodPut, f.base+"/onboarding/policy", policy.Change{Policy: descriptor}), tt.admin)
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
	issue := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "first-issue"}, Title: "First native issue", Body: "No GitHub issue required", State: "Todo"}
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
	backend := &scriptedReconcileBackend{}
	f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false, func(cfg *Config) {
		cfg.ReconcileBackend = backend
	})
	grantAppRunners(t, f)
	runner := enrollAppRunner(t, f, "Private checkout host", "test")
	base := browserHostedOrganizationBase + "/projects/" + f.project
	request := map[string]any{"idempotency_key": "checkout-association", "expected_revision": "1", "repository": "Acme/Private", "source": "runner_checkout"}
	var available ProjectIntegration
	response := f.setupRequest(t, "owner", http.MethodGet, base+"/integration", nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &available)
	if available.GitHubTransportAvailable == nil || *available.GitHubTransportAvailable || available.RepositoryEnabled {
		t.Fatalf("hosted transport = %+v", available)
	}

	for _, test := range []struct {
		name, account string
		status        int
		message       string
	}{
		{"viewer cannot associate", "viewer", http.StatusNotFound, ""},
		{"missing local checkout", "owner", http.StatusUnprocessableEntity, "Start an enrolled runner"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.setupRequest(t, test.account, http.MethodPost, base+"/onboarding/repository", request)
			requireNativeStatus(t, response, test.status)
			if test.message != "" && !strings.Contains(response.Body.String(), test.message) {
				t.Fatalf("error did not explain next action: %s", response.Body.String())
			}
		})
	}

	path := base + "/machines/" + string(runner.MachineID) + "/heartbeat"
	heartbeat := map[string]any{"display_name": "Private checkout host", "capacity": 1, "version": "test", "checkout_repository": "Other/Repository"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat), http.StatusOK)
	response = f.setupRequest(t, "owner", http.MethodPost, base+"/onboarding/repository", request)
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	if !strings.Contains(response.Body.String(), "matching GitHub origin") {
		t.Fatalf("mismatch did not explain next action: %s", response.Body.String())
	}

	heartbeat["checkout_repository"] = "https://alice:private-secret@github.com/Acme/Private.git"
	response = performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat)
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	if strings.Contains(response.Body.String(), "private-secret") || strings.Contains(response.Body.String(), runner.Credential) {
		t.Fatal("invalid checkout report exposed credentials")
	}
	heartbeat["checkout_repository"] = "Acme/Private"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, runner.Credential, heartbeat), http.StatusOK)
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=? WHERE id=(SELECT token_id FROM runner_identities WHERE id=?)", formatHubTime(time.Now()), runner.RunnerID); err != nil {
		t.Fatal(err)
	}
	response = f.setupRequest(t, "owner", http.MethodPost, base+"/onboarding/repository", request)
	requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	if !strings.Contains(response.Body.String(), "Start an enrolled runner") {
		t.Fatalf("revoked runner error did not explain next action: %s", response.Body.String())
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE api_tokens SET revoked_at=NULL WHERE id=(SELECT token_id FROM runner_identities WHERE id=?)", runner.RunnerID); err != nil {
		t.Fatal(err)
	}
	response = f.setupRequest(t, "owner", http.MethodPost, base+"/onboarding/repository", request)
	requireNativeStatus(t, response, http.StatusOK)
	var integration ProjectIntegration
	decodeHubResponse(t, response, &integration)
	if integration.CheckoutRepository != "Acme/Private" || integration.RepositoryEnabled || integration.Repository != "" || integration.Revision != 2 {
		t.Fatalf("association = %+v", integration)
	}
	if strings.Contains(response.Body.String(), runner.Credential) || strings.Contains(response.Body.String(), "private-secret") {
		t.Fatal("credential leaked in association response")
	}
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPost, base+"/onboarding/repository", request), http.StatusOK)
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM repositories").Scan(&count); err != nil || count != 0 {
		t.Fatalf("GitHub repository projection count = %d: %v", count, err)
	}
	if calls := backend.Requests(); len(calls) != 0 {
		t.Fatalf("hosted checkout association called GitHub: %+v", calls)
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
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, base+"/onboarding/policy", policy.Change{Policy: descriptor}), http.StatusOK)
	automatic := hubTestPolicy()
	automatic.Gates.AutoPromote = true
	automatic.Requirements.RequiredTags = []string{"gpu"}
	automatic = automatic.WithID()
	requireNativeStatus(t, f.setupRequest(t, "owner", http.MethodPut, organization+"/projects/"+f.privateProject+"/onboarding/policy", policy.Change{Policy: automatic}), http.StatusOK)
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
	if err != nil || !reflect.DeepEqual(project.States, nativeFixtureStates()) {
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
}

func TestOnboardingPendingRepositoryPolicyApproval(t *testing.T) {
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
			if err := check(); err == nil {
				t.Fatal("runner loaded an unapproved policy")
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
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, target.base+"/onboarding/policy", testHubAdminToken, policy.Change{ExpectedID: setup.Policy.Policy.ID, Policy: pending.Policy}), http.StatusOK)
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
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other checks")
	r := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	r.enroll(t)
	path := f.base + "/machines/" + string(r.binding.MachineID) + "/heartbeat"
	for _, tt := range []struct {
		name, checkout, doctor, provider, setup string
		valid                                   bool
	}{
		{"missing report", "", "", "", "", true},
		{"missing checkout", "failed", "pending", "pending", "", true},
		{"failed doctor", "passed", "failed", "passed", "", true},
		{"missing auth", "passed", "passed", "failed", "", true},
		{"success", "passed", "passed", "passed", "", true},
		{"failed setup", "passed", "passed", "passed", "failed", true},
		{"repaired setup", "passed", "passed", "passed", "passed", true},
		{"reject setup output", "passed", "passed", "passed", "secret-token", false},
		{"reject command output", "passed", "secret-token", "passed", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checks := runnerauth.LocalChecks{Checkout: tt.checkout, Doctor: tt.doctor, Provider: tt.provider, ProviderKinds: []string{"codex"}, Setup: tt.setup}
			body := map[string]any{"capacity": 1, "version": "test"}
			if tt.checkout != "" {
				body["local_checks"] = checks
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, path, r.redemption.Credential, body)
			expected := http.StatusOK
			if !tt.valid {
				expected = http.StatusUnprocessableEntity
			}
			requireNativeStatus(t, response, expected)
			response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/onboarding", f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var setup onboarding.Project
			decodeHubResponse(t, response, &setup)
			if setup.Policy != nil || setup.Steps[0].State != "ready" {
				t.Fatalf("enrollment depends on policy: %+v", setup)
			}
			if tt.valid && tt.checkout != "" && (setup.Runners[0].LocalChecks == nil || setup.Runners[0].LocalChecks.Doctor != tt.doctor || setup.Runners[0].LocalChecks.Setup != tt.setup || setup.Runners[0].LocalChecks.ObservedAt.IsZero()) {
				t.Fatalf("report=%+v", setup)
			}
			if tt.valid && (setup.Steps[1].State == "ready") != (tt.checkout != "" && checks.Passed()) {
				t.Fatalf("local validation forged success or lost passed evidence: %+v", setup)
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
		if err := runner.CheckProjectPolicy(t.Context(), "shared", "", descriptor); err == nil {
			t.Fatal("runner executed without approval")
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
	if len(initial.ObservedPolicies) != 2 || !initial.ObservedPolicies[0].Conflict || !initial.ObservedPolicies[1].Conflict {
		t.Fatalf("conflicting reports = %+v", initial.ObservedPolicies)
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", descriptors[0])
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptors[0].ID, Policy: descriptors[1]}), http.StatusOK)
	historical := read()
	if len(historical.ObservedPolicies) != 1 || !historical.ObservedPolicies[0].PreviouslyApproved || !historical.ObservedPolicies[0].Conflict {
		t.Fatalf("historical report offered as a new approval: %+v", historical.ObservedPolicies)
	}
	combined, err := workflowconfig.ParseWorkflowOverlay(proWorkflow.DefinitionSources.Workflow, []byte("---\nagent:\n  auto_promote:\n    enabled: true\n---\n"), "operator-approved.local.md")
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := workflowconfig.ResolvePolicy(combined)
	if err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptors[0].ID, Policy: descriptor}), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: descriptors[1].ID, Policy: descriptor}), http.StatusOK)
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
