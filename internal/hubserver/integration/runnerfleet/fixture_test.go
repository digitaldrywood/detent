package runnerfleet_test

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runneradvice"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	testHubAdminToken         = "example-runner-fleet-admin"
	testHubAdminCredential    = "test_hub_admin"
	issueContractTestSections = "\n## Acceptance criteria\nComplete the requested work.\n## Must not break\nExisting behavior.\n## How we know it worked\nRun the project checks."
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type Config = hubserver.Config

type testHub struct {
	service *hubserver.Service
	config  Config
	admin   *sql.DB
	owner   *testing.T
}

func (h *testHub) Handler() http.Handler { return h.service.Handler() }

func (h *testHub) Close() error { return h.service.Close() }

type runnerCapacityChange struct {
	runnerauth.CapacityRequest
	ExpectedRevision int64  `json:"expected_revision"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
}

type runnerRoutingRequest struct {
	runnerauth.RoutingChange
	IdempotencyKey string                   `json:"idempotency_key,omitempty"`
	IsolationTier  *string                  `json:"isolation_tier"`
	HostServices   *[]string                `json:"host_services"`
	Availability   *runnerauth.Availability `json:"availability"`
}

func secretKeys(t *testing.T) *hubsecrets.Keyring {
	t.Helper()
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32)) + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func seedAdmin(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, now time.Time) {
	t.Helper()
	now = now.UTC().Truncate(time.Microsecond)
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: testHubAdminCredential, Hash: apikey.HashToken(testHubAdminToken), Name: "Hub test administrator", Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: hubapi.NewID("hub"), CursorKey: bytes.Repeat([]byte{0x5a}, 32)})
	}); err != nil {
		t.Fatal(err)
	}
}

func newClockedPostgresNativeService(t *testing.T, cfg Config) *testHub {
	t.Helper()
	admin := hubtest.Database(t)
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	repository := hubtest.RepositoryOn(t, admin, hubtest.WithClock(cfg.Clock))
	if cfg.SecretKeys == nil {
		cfg.SecretKeys = secretKeys(t)
	}
	if cfg.RunDetails == nil {
		cfg.RunDetails = hubtest.NewDetails()
	}
	for _, organization := range []tracker.OrganizationID{"org_first_second", "org_first"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-"), Now: cfg.Clock()})
		seedAdmin(t, repository, organization, cfg.Clock())
	}
	if _, err := admin.ExecContext(t.Context(), "UPDATE organizations SET local=true WHERE organization_id=$1", "org_first"); err != nil {
		t.Fatal(err)
	}
	return openHub(t, &testHub{admin: admin, owner: t}, cfg, repository)
}

func newPostgresNativeService(t *testing.T, cfg Config) *testHub {
	t.Helper()
	return newClockedPostgresNativeService(t, cfg)
}

func openHub(t *testing.T, h *testHub, cfg Config, repository *postgres.Repository) *testHub {
	t.Helper()
	connection := hubtest.PostgresConfig(h.owner, h.admin, hubtest.WithClock(cfg.Clock))
	cfg.Postgres = &connection
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	cfg.TenantRepository = repository
	cfg.AccessRepository = repository
	cfg.BillingRepository = repository
	cfg.InitialAdminToken = []byte(testHubAdminToken)
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.service = service
	h.config = cfg
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func reopenPostgresNativeService(t *testing.T, h *testHub) *testHub {
	t.Helper()
	return openHub(t, &testHub{admin: h.admin, owner: h.owner}, h.config, hubtest.RepositoryOn(h.owner, h.admin, hubtest.WithClock(h.config.Clock)))
}

func usagePostgresAdmin(t *testing.T, h *testHub) *sql.DB {
	t.Helper()
	return h.admin
}

type nativeFixture struct {
	service *testHub
	project tracker.NativeProject
	base    string
	token   string
}

func newDefaultNativeFixture(t *testing.T, cfg Config) nativeFixture {
	t.Helper()
	return newNativeFixture(t, newClockedPostgresNativeService(t, cfg), "", "Example project")
}

func newPostgresDefaultNativeFixture(t *testing.T, cfg Config) nativeFixture {
	t.Helper()
	return newNativeFixture(t, newPostgresNativeService(t, cfg), "", "Example project")
}

func newNativeFixture(t *testing.T, h *testHub, organization tracker.OrganizationID, name string) nativeFixture {
	t.Helper()
	if h == nil {
		h = newPostgresNativeService(t, Config{})
	}
	if organization == "" {
		organization = "org_first"
	}
	repository := h.config.TenantRepository
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.AccessOrganization(t.Context())
		return err
	}); errors.Is(err, corestore.ErrNotFound) {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Now: h.config.Clock()})
		seedAdmin(t, repository, organization, h.config.Clock())
	} else if err != nil {
		t.Fatal(err)
	}
	now := h.config.Clock().UTC().Truncate(time.Microsecond)
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{Name: name, RequireDependencies: true})
	credential := hubtest.Credential(t, repository, organization, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}, Now: now})
	hubtest.Policy(t, repository, h.config.RunDetails, project, hubtest.PolicyOptions{Now: now})
	return nativeFixture{service: h, project: project, base: "/api/v2/organizations/" + string(organization) + "/projects/" + string(project.ID), token: credential.Token}
}

func (f nativeFixture) create(t *testing.T, name string) tracker.NativeIssue {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-" + name}, Title: name, Body: strings.Repeat("Full issue content. ", 80) + issueContractTestSections, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	return issue
}

func hubTestPolicy() policy.Descriptor { return hubtest.PolicyDescriptor() }

func approveHubTestPolicy(t *testing.T, h *testHub, path string, descriptor policy.Descriptor) {
	t.Helper()
	current := performHubAPIRequest(t, h, http.MethodGet, path, testHubAdminToken, nil)
	var approval policy.Approval
	if current.Code == http.StatusOK {
		decodeHubResponse(t, current, &approval)
	}
	requireNativeStatus(t, performHubAPIRequest(t, h, http.MethodPut, path, testHubAdminToken, policy.Change{ExpectedID: approval.Policy.ID, Policy: descriptor}), http.StatusOK)
}

func performHubAPIRequest(t *testing.T, h *testHub, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, request)
	return response
}

func requireNativeStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body.String())
	}
}

func decodeHubResponse(t *testing.T, response *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}

type runnerFixture struct {
	nativeFixture
	binding    runnerauth.Binding
	enrollment runnerauth.Enrollment
	redemption runnerauth.Redemption
	identity   runnerauth.Identity
	base       string
}

func prepareRunner(t *testing.T, f nativeFixture, operations ...string) runnerFixture {
	t.Helper()
	return prepareRunnerForScope(t, f, "projects", operations...)
}

func prepareRunnerForScope(t *testing.T, f nativeFixture, scope string, operations ...string) runnerFixture {
	t.Helper()
	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/v2/organizations/" + string(f.project.OrganizationID)
	projects := []tracker.ProjectID{f.project.ID}
	if scope == "organization" {
		projects = nil
	}
	response := performHubAPIRequest(t, f.service, http.MethodPost, base+"/runner-enrollments", testHubAdminToken, runnerauth.EnrollmentRequest{Scope: scope, Binding: binding, ProjectIDs: projects, Operations: operations, TTLSeconds: 60})
	requireNativeStatus(t, response, http.StatusCreated)
	var enrollment runnerauth.Enrollment
	decodeHubResponse(t, response, &enrollment)
	return runnerFixture{nativeFixture: f, binding: binding, enrollment: enrollment, base: base,
		redemption: runnerauth.Redemption{BackendIsolation: isolation.Report{"test": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "customer-host", DisplayName: "Runner", Capacity: 2, Version: "test"}}
}

func (r *runnerFixture) enroll(t *testing.T) {
	t.Helper()
	response := performHubAPIRequest(t, r.service, http.MethodPost, r.base+"/runner-enrollments/redeem", r.enrollment.Token, r.redemption)
	requireNativeStatus(t, response, http.StatusCreated)
	decodeHubResponse(t, response, &r.identity)
}

func (r runnerFixture) identityPath() string { return r.base + "/runners/" + r.binding.RunnerID }

func (r runnerFixture) reportCapacity(t *testing.T) {
	t.Helper()
	reportRunnerCapacity(t, r.service, r.nativeFixture.base, r.redemption.Credential, r.binding.MachineID, r.redemption.DisplayName, r.redemption.BackendIsolation, r.redemption.Capacity)
}

func reportRunnerCapacity(t *testing.T, h *testHub, projectBase, credential string, machine tracker.MachineID, displayName string, backends isolation.Report, capacity int) {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, h, http.MethodPost, projectBase+"/machines/"+string(machine)+"/heartbeat", credential, map[string]any{
		"display_name": displayName, "capacity": capacity, "version": "test", "backend_isolation": backends,
		"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: h.config.Clock()},
	}), http.StatusOK)
}

func readRunnerRouting(t *testing.T, h *testHub, organization tracker.OrganizationID, runner string) (runnerauth.Runner, error) {
	t.Helper()
	response := performHubAPIRequest(t, h, http.MethodGet, "/api/v2/organizations/"+string(organization)+"/runners/"+runner+"/routing", testHubAdminToken, nil)
	if response.Code != http.StatusOK {
		return runnerauth.Runner{}, errors.New(strings.TrimSpace(response.Body.String()))
	}
	var result runnerauth.Runner
	decodeHubResponse(t, response, &result)
	return result, nil
}

func capacityReport(now time.Time) providercapacity.Report {
	return providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "local", SharedAccountAlias: "team", Models: []string{"test-model"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
}

func publishCapacity(t *testing.T, f nativeFixture, r runnerFixture, reports ...providercapacity.Report) {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(r.binding.MachineID)+"/heartbeat", r.redemption.Credential,
		map[string]any{"backend_isolation": r.redemption.BackendIsolation, "display_name": "runner", "capacity": 8, "version": "test", "provider_reports": reports,
			"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: 8, ClientLimit: 8, RuntimeLimit: 8, Manageable: true, ObservedAt: f.service.config.Clock()}})
	requireNativeStatus(t, response, http.StatusOK)
}

func providerClaim(r runnerFixture, issue tracker.NativeIssue, session string) tracker.NativeClaim {
	return tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: r.binding.MachineID, SessionID: session, TTLSeconds: 90,
		ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability},
		ProviderCandidates: []tracker.NativeCapacityCandidate{{WorkItemID: issue.WorkItemID, Revision: issue.Revision, Requirement: providercapacity.Requirement{Role: "implement", Backend: "codex", Model: "test-model"}}}}
}

func hostedPostgresAdmin(t *testing.T, h *testHub) *sql.DB {
	t.Helper()
	return h.admin
}

func nativeAttemptTestDatabase(t *testing.T, f nativeFixture) *sql.DB {
	t.Helper()
	return f.service.admin
}

func formatHubTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func clearNativeFixturePolicy(t *testing.T, f nativeFixture) {
	t.Helper()
	if _, err := f.service.admin.ExecContext(t.Context(), "DELETE FROM project_policies WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, f.project.ID); err != nil {
		t.Fatal(err)
	}
}

func seedStartupOrganization(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID) {
	t.Helper()
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: "example-" + strings.TrimPrefix(string(organization), "org_"), CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization), AllocationGeneration: 1})
	}); err != nil {
		t.Fatal(err)
	}
}

func nativeStartedEvent(lease tracker.NativeLease) tracker.NativeRunEvent {
	return tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"},
			LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, RunID: hubapi.NewID("run"), AttemptID: hubapi.NewID("attempt")}}
}

func sharedRunner(t *testing.T, first runnerFixture) runnerFixture {
	t.Helper()
	binding := runnerauth.NewBinding()
	binding.MachineID = first.binding.MachineID
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	r := runnerFixture{nativeFixture: first.nativeFixture, binding: binding, base: first.base,
		redemption: runnerauth.Redemption{Binding: binding, Credential: credential, Hostname: "shared", DisplayName: "Second", Capacity: 8, Version: "test", OS: "linux", Architecture: "arm64"}}
	response := performHubAPIRequest(t, r.service, http.MethodPost, r.base+"/runner-enrollments", testHubAdminToken,
		runnerauth.EnrollmentRequest{Binding: binding, SharedMachine: true, ProjectIDs: []tracker.ProjectID{r.project.ID}, Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60})
	requireNativeStatus(t, response, http.StatusCreated)
	decodeHubResponse(t, response, &r.enrollment)
	r.enroll(t)
	return r
}

func hostHistory(t *testing.T, f nativeFixture, organization tracker.OrganizationID, runner string, from, to time.Time) ([]runneradvice.HostHour, error) {
	t.Helper()
	return runneradvice.New(runneradvice.Deps{Repository: f.service.config.TenantRepository, Now: f.service.config.Clock}).HostHistory(t.Context(), organization, runner, from, to)
}

func readRunnerRanks(t *testing.T, h *testHub, organization tracker.OrganizationID, runner string) (map[tracker.ProjectID]int, error) {
	t.Helper()
	var ranks map[tracker.ProjectID]int
	err := h.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		identity, err := r.RunnerIdentity(t.Context(), runner)
		ranks = identity.ProjectRanks
		return err
	})
	return ranks, err
}
