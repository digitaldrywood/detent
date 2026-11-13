package billingcosts_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/usagecosts"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type memoryRunDetails struct {
	mu          sync.Mutex
	data        map[string][]byte
	fail        bool
	uploads     int
	reads       int
	keyReads    map[string]int
	writerReads int
	afterStore  func(attachment.ArchiveReference)
	beforeRead  func(context.Context) error
}

func (m *memoryRunDetails) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	m.mu.Lock()
	if m.fail {
		m.mu.Unlock()
		return attachment.ArchiveReference{}, errors.New("upload unavailable")
	}
	if m.data == nil {
		m.data = make(map[string][]byte)
	}
	if _, ok := m.data[ref.Key]; !ok {
		m.uploads++
	}
	m.data[ref.Key] = bytes.Clone(data)
	afterStore := m.afterStore
	m.mu.Unlock()
	if afterStore != nil {
		afterStore(ref)
	}
	return ref, nil
}

func (m *memoryRunDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if m.beforeRead != nil {
		if err := m.beforeRead(ctx); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	m.reads++
	if m.keyReads != nil {
		m.keyReads[ref.Key]++
	}

	data, ok := m.data[ref.Key]
	if !ok {
		return nil, artifact.ErrMissing
	}
	return bytes.Clone(data), nil
}
func validateDetail(ctx context.Context, store attachment.DetailStore, organization tracker.OrganizationID, ref attachment.ArchiveReference, expected []byte) error {
	if ref.Validate(string(organization)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(expected)) {
		return artifact.ErrIntegrity
	}
	actual, err := store.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return errors.Join(artifact.ErrIntegrity, err)
	}
	if !bytes.Equal(actual, expected) {
		return artifact.ErrIntegrity
	}
	return nil
}

func keys(t *testing.T) *hubsecrets.Keyring {
	t.Helper()
	k, e := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32)) + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		}
		return ""
	})
	if e != nil {
		t.Fatal(e)
	}
	return k
}
func performHubAPIRequest(t *testing.T, service *testHub, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if payload != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	service.Handler().ServeHTTP(w, r)
	return w
}

func requireNativeStatus(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status = %d, want %d: %s", r.Code, want, r.Body.String())
	}
}

func decodeHubResponse(t *testing.T, r *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}

func costHTTP(t *testing.T, repo tenantstore.Repository, details attachment.DetailStore, clocks ...func() time.Time) *echo.Echo {
	now := time.Now
	if len(clocks) > 0 {
		now = clocks[0]
	}
	t.Helper()
	h := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repo, Now: now}, func(e *echo.Group, d hubtest.HTTPDeps) {
		service := usagecosts.New(usagecosts.Deps{Repository: repo, Details: details, Now: now, ContentContext: func(ctx context.Context) context.Context { return ctx }, RecordError: hubauth.RecordError, AccountUsage: func(context.Context, tenantstore.Writer, time.Time, string) error { return nil }, ValidateDetail: validateDetail})
		usagecosts.RegisterRoutes(e, usagecosts.HTTPDeps{Service: service, Native: d.Native, InstanceAdmin: d.API(hubauth.Admin), Scope: func(c echo.Context, admin bool) hubapi.Scope {
			scope := hubauth.RequestScope(c)
			scope.Admin = admin
			return scope
		}, CanManage: func(c echo.Context) bool {
			v, _ := hubauth.RequestCredential(c)
			return v.Scope == hubauth.Admin || v.HostedRole == "owner" || v.HostedRole == "admin"
		}, Error: d.Error, AccessRead: func(ctx context.Context, org tracker.OrganizationID, read func(tenantstore.Reader) error) error {
			return repo.ReadTenant(ctx, org, corestore.Serving, read)
		}, RefuseImport: func(c echo.Context) error {
			return d.Error(c, &hubapi.Error{Status: http.StatusForbidden, Code: "forbidden", Message: "Importing infrastructure usage requires owner or admin access", Public: true})
		}})
	})
	return h.Echo
}

func seedAdmin(t *testing.T, repo tenantstore.Repository, org tracker.OrganizationID, clocks ...func() time.Time) {
	t.Helper()
	now := time.Now().UTC()
	if len(clocks) > 0 {
		now = clocks[0]()
	}
	if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Name: "Example admin", Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
}

type hostedSecurityProvider struct {
	mu                   sync.Mutex
	sessions             map[string]auth.HostedIdentity
	members              map[string]auth.Membership
	codes                map[string]auth.Identity
	invitations          map[string]auth.Invitation
	invitationRoles      map[string]string
	inviteErrAfterCreate error
	exchanges            int
	revokeErr            error
	revoked              []string
}

func newHostedSecurityProvider() *hostedSecurityProvider {
	return &hostedSecurityProvider{
		sessions:        make(map[string]auth.HostedIdentity),
		members:         make(map[string]auth.Membership),
		codes:           make(map[string]auth.Identity),
		invitations:     make(map[string]auth.Invitation),
		invitationRoles: make(map[string]string),
	}
}

func (p *hostedSecurityProvider) AuthorizationURL(state, nonce, verifier string) string {
	return "https://identity.example.test/authorize?" + url.Values{"state": {state}, "nonce": {nonce}, "code_challenge": {verifier}}.Encode()
}

func (p *hostedSecurityProvider) Exchange(_ context.Context, code, _, _ string) (auth.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.exchanges++
	identity, ok := p.codes[code]
	if !ok {
		return auth.Identity{}, auth.ErrHostedIdentity
	}
	delete(p.codes, code)
	return identity, nil
}

func (p *hostedSecurityProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.sessions[identity.SessionID]
	if !ok {
		return auth.HostedIdentity{}, auth.ErrHostedIdentity
	}
	return current, nil
}

func (p *hostedSecurityProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var members []auth.Membership
	for _, membership := range p.members {
		if (user == "" || membership.UserID == user) && (organization == "" || membership.OrganizationID == organization) {
			members = append(members, membership)
		}
	}
	return members, nil
}

func (p *hostedSecurityProvider) Organization(_ context.Context, id string) (auth.Organization, error) {
	return auth.Organization{ID: id, ExternalID: "org_security", Name: "Organization"}, nil
}

func (p *hostedSecurityProvider) CreateOrganization(_ context.Context, externalID, name string) (auth.Organization, error) {
	return auth.Organization{ID: "org_provider", ExternalID: externalID, Name: name}, nil
}

func (p *hostedSecurityProvider) CreateMembership(_ context.Context, user, organization, role string) (auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: organization, Status: "active"}
	member.Role.Slug = role
	p.members[member.ID] = member
	return member, nil
}

func (p *hostedSecurityProvider) SetMembershipRole(_ context.Context, id, role string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	member, ok := p.members[id]
	if !ok {
		return auth.ErrHostedIdentity
	}
	member.Role.Slug = role
	p.members[id] = member
	return nil
}

func (p *hostedSecurityProvider) RevokeMembership(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	member, ok := p.members[id]
	if !ok {
		return auth.ErrHostedIdentity
	}
	member.Status = "inactive"
	p.members[id] = member
	return nil
}

func (p *hostedSecurityProvider) Invite(_ context.Context, organization, email, role, _ string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation := auth.Invitation{ID: fmt.Sprintf("invitation_%d", len(p.invitations)+1), Email: email, OrganizationID: organization, State: "pending", ExpiresAt: time.Now().Add(time.Hour)}
	p.invitations[invitation.ID] = invitation
	p.invitationRoles[invitation.ID] = role
	return invitation, p.inviteErrAfterCreate
}

func (p *hostedSecurityProvider) Invitation(_ context.Context, token string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[token]
	if !ok {
		return auth.Invitation{}, auth.ErrHostedIdentity
	}
	return invitation, nil
}

func (p *hostedSecurityProvider) RevokeInvitation(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[id]
	if !ok || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	invitation.State = "revoked"
	p.invitations[id] = invitation
	return nil
}

func (p *hostedSecurityProvider) ResendInvitation(ctx context.Context, id string) error {
	invitation, err := p.Invitation(ctx, id)
	if err != nil || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	return nil
}

func (p *hostedSecurityProvider) AcceptInvitation(_ context.Context, token, user string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[token]
	if !ok || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	invitation.State, invitation.AcceptedUserID = "accepted", user
	p.invitations[token] = invitation
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: invitation.OrganizationID, Status: "active"}
	member.Role.Slug = p.invitationRoles[token]
	p.members[member.ID] = member
	return nil
}

func (p *hostedSecurityProvider) RevokeSession(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.revoked = append(p.revoked, id)
	if p.revokeErr != nil {
		return p.revokeErr
	}
	delete(p.sessions, id)
	return nil
}

type Config = hubserver.Config
type leaseTestClock struct {
	mu    sync.Mutex
	value time.Time
}

func (c *leaseTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.value }
func (c *leaseTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = c.value.Add(d)
}

type testHub struct {
	service    *hubserver.Service
	config     Config
	admin      *sql.DB
	owner      *testing.T
	database   *costDatabase
	costHTTP   *echo.Echo
	costRoutes bool
}

func (h *testHub) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.config.Hosted == nil && (strings.HasSuffix(r.URL.Path, "/usage/monthly") || strings.HasSuffix(r.URL.Path, "/usage/sprites")) {
			if !h.costRoutes {
				echo.New().ServeHTTP(w, r)
				return
			}
			h.costHTTP.ServeHTTP(w, r)
			return
		}
		h.service.Handler().ServeHTTP(w, r)
	})
}
func (h *testHub) Close() error { return h.service.Close() }
func newClockedPostgresNativeService(t *testing.T, cfg Config) *testHub {
	t.Helper()
	admin := hubtest.Database(t)
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	repo := hubtest.RepositoryOn(t, admin, hubtest.WithClock(cfg.Clock))
	if cfg.RunDetails == nil {
		cfg.RunDetails = &memoryRunDetails{}
	}
	cfg.SecretKeys = keys(t)
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		hubtest.Organization(t, repo, hubtest.OrganizationOptions{ID: org, Now: cfg.Clock()})
		seedAdmin(t, repo, org, cfg.Clock)
		if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: hubapi.NewID("hub"), CursorKey: bytes.Repeat([]byte{0x5a}, 32)})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.ExecContext(t.Context(), "UPDATE organizations SET local=true WHERE organization_id=$1", "org_first"); err != nil {
		t.Fatal(err)
	}
	return openHub(t, &testHub{admin: admin, owner: t, costRoutes: true}, cfg, repo)
}
func openHub(t *testing.T, h *testHub, cfg Config, repo *postgres.Repository) *testHub {
	t.Helper()
	pg := hubtest.PostgresConfig(h.owner, h.admin, hubtest.WithClock(cfg.Clock))
	cfg.Postgres = &pg
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	cfg.TenantRepository = repo
	cfg.AccessRepository = repo
	cfg.BillingRepository = repo
	cfg.InitialAdminToken = []byte(testHubAdminToken)
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.service = service
	h.config = cfg
	h.database = &costDatabase{usagecosts.New(usagecosts.Deps{Repository: repo, Details: cfg.RunDetails}), cfg.RunDetails, repo}
	h.costHTTP = costHTTP(t, repo, cfg.RunDetails, cfg.Clock)
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}
func reopenPostgresNativeService(t *testing.T, h *testHub) *testHub {
	t.Helper()
	return openHub(t, h, h.config, hubtest.RepositoryOn(h.owner, h.admin, hubtest.WithClock(h.config.Clock)))
}
func usagePostgresAdmin(t *testing.T, h *testHub) *sql.DB { t.Helper(); return h.admin }

type nativeFixture struct {
	service     *testHub
	project     tracker.NativeProject
	base, token string
}

func newDefaultNativeFixture(t *testing.T, cfg Config) nativeFixture {
	t.Helper()
	return newNativeFixture(t, newClockedPostgresNativeService(t, cfg), "", "Example")
}
func newNativeFixture(t *testing.T, h *testHub, org tracker.OrganizationID, name string) nativeFixture {
	t.Helper()
	if org == "" {
		org = "org_first"
	}
	if err := h.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error { _, err := r.AccessOrganization(t.Context()); return err }); errors.Is(err, corestore.ErrNotFound) {
		hubtest.Organization(t, h.config.TenantRepository, hubtest.OrganizationOptions{ID: org, Now: h.config.Clock()})
		seedAdmin(t, h.config.TenantRepository, org, h.config.Clock)
	} else if err != nil {
		t.Fatal(err)
	}
	project := hubtest.Project(t, h.config.TenantRepository, org, hubtest.ProjectOptions{Name: name})
	credential := hubtest.Credential(t, h.config.TenantRepository, org, hubtest.CredentialOptions{Projects: []tracker.ProjectID{project.ID}, Now: h.config.Clock()})
	hubtest.Policy(t, h.config.TenantRepository, h.config.RunDetails, project, hubtest.PolicyOptions{Now: h.config.Clock()})
	return nativeFixture{h, project, "/api/v2/organizations/" + string(org) + "/projects/" + string(project.ID), credential.Token}
}

type attemptDiffFixture struct {
	nativeFixture
	worker, runnerID string
	issue            tracker.NativeIssue
	policy           string
	lease            tracker.NativeLease
	attempt, run     string
}

func claimedAttemptDiffFixture(t *testing.T, enrolled bool, cfg Config) *attemptDiffFixture {
	return claimAttemptDiffFixture(t, enrolled, newDefaultNativeFixture(t, cfg))
}
func claimAttemptDiffFixture(t *testing.T, enrolled bool, f nativeFixture) *attemptDiffFixture {
	t.Helper()
	repo := f.service.config.TenantRepository
	now := f.service.config.Clock()
	details := f.service.config.RunDetails
	policy := hubtest.Policy(t, repo, details, f.project, hubtest.PolicyOptions{Now: now})
	issue := hubtest.Issue(t, repo, details, f.project, hubtest.IssueOptions{State: "In Progress", Now: now})
	runner := hubtest.Runner(t, repo, f.project.OrganizationID, hubtest.RunnerOptions{Projects: []tracker.ProjectID{f.project.ID}, Now: now, Capacity: 2})
	lease := hubtest.Lease(t, repo, runner, issue, hubtest.LeaseOptions{PolicyID: policy.Descriptor.ID, TTL: 10 * time.Minute, Now: now})
	return &attemptDiffFixture{nativeFixture: f, worker: runner.Credential.Token, runnerID: runner.Identity.RunnerID, issue: issue.Issue, policy: policy.Descriptor.ID, lease: lease, attempt: hubapi.NewID("attempt"), run: hubapi.NewID("run")}
}
func pendingEventPath(f *attemptDiffFixture) string {
	return f.base + "/work-items/" + string(f.issue.WorkItemID) + "/events"
}

func nativeStartedEvent(lease tracker.NativeLease) tracker.NativeRunEvent {
	return tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"},
			LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, RunID: hubapi.NewID("run"), AttemptID: hubapi.NewID("attempt")}}
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

func reportRunnerCapacity(t *testing.T, service *testHub, projectBase, credential string, machine tracker.MachineID, displayName string, backends isolation.Report, capacity int) {
	t.Helper()
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, projectBase+"/machines/"+string(machine)+"/heartbeat", credential, map[string]any{
		"display_name": displayName, "capacity": capacity, "version": "test", "backend_isolation": backends,
		"capacity_configuration": runnerauth.CapacityConfig{Revision: strings.Repeat("c", 64), LocalLimit: capacity, ClientLimit: capacity, RuntimeLimit: capacity, Manageable: true, ObservedAt: service.config.Clock()},
	}), http.StatusOK)
}

func costTestObservation(metric string, from, to time.Time) costObservation {
	quantity, rate := 2.0, int64(10_000)
	unit := "GB-hour"
	if metric == "cpu" {
		unit = "CPU-hour"
	}
	return costObservation{Provider: "fly_sprites", ProviderAccount: "customer", ResourceID: "sprite-immutable-id", ResourceName: "worker", Bucket: spriteCostBucket, Metric: metric, SourceID: "meter-" + metric, Revision: 1, From: from, To: to, Quantity: &quantity, Unit: unit, QuantityBasis: "measured", Currency: "USD", Basis: "estimated", UnitPriceMicros: &rate, RateSource: "https://fly.io/sprites/", RateEffectiveAt: from.AddDate(0, -1, 0), EvidenceSource: "measurement-batch-1", ObservedAt: to, FreshUntil: to.AddDate(1, 0, 0), Coverage: "complete"}
}

func recordPostgresTestCost(t *testing.T, d *costDatabase, organization, project string, observation costObservation, now time.Time) (bool, error) {
	t.Helper()
	if err := normalizeCostObservation(&observation, now); err != nil {
		return false, err
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		return false, err
	}
	ref, err := d.details.StoreDetail(t.Context(), organization, raw)
	if err != nil {
		return false, err
	}
	var changed bool
	err = d.repository.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		changed, err = w.RecordBillingCost(t.Context(), billingstore.CostObservation{OrganizationID: tracker.OrganizationID(organization), ProjectID: tracker.ProjectID(project), Observation: observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: now})
		return err
	})
	return changed, err
}

type costDatabase struct {
	*usagecosts.Service
	details    attachment.DetailStore
	repository tenantstore.Repository
}

const testHubAdminToken = "detent_test_hub_admin_token_00000000000000000000000000000000"
const spriteCostBucket = usagecosts.SpriteBucket

type costObservation = usagecosts.Observation
type monthlyCostReport = usagecosts.Report
type usageWindow = usagecosts.Window
type spriteUsageRequest = usagecosts.SpriteRequest
type nativeScope struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
}

func normalizeCostObservation(o *costObservation, now time.Time) error {
	return usagecosts.Normalize(o, now)
}
func costMonth(value string, now time.Time) (usageWindow, error) { return usagecosts.Month(value, now) }
func (d *costDatabase) monthlyCosts(ctx context.Context, org, scope string, projects []string, window usageWindow, now time.Time) (monthlyCostReport, error) {
	return d.Monthly(ctx, org, scope, projects, window, now)
}

type UsageConfig = hubserver.UsageConfig
type UsagePrice = hubserver.UsagePrice

func callTool(t *testing.T, f nativeFixture, call operatortool.Call) (operatortool.Result, error) {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": call.Name, "arguments": call.Arguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": mcp.ProtocolVersion, "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "example-cost-client", "version": "1"}}}}
	raw, err := json.Marshal(body)
	if err != nil {
		return operatortool.Result{}, err
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/mcp", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+testHubAdminToken)
	request.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", call.Name)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		return operatortool.Result{}, fmt.Errorf("MCP status %d: %s", response.Code, response.Body.String())
	}
	var result struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		return operatortool.Result{}, err
	}
	if result.Error != nil || result.Result.IsError || len(result.Result.Content) != 1 {
		return operatortool.Result{}, fmt.Errorf("MCP result: %s", response.Body.String())
	}
	return operatortool.Result{Content: json.RawMessage(result.Result.Content[0].Text)}, nil
}
func projectAction(t *testing.T, f nativeFixture, call operatortool.Call) chat.Action {
	t.Helper()
	result, err := callTool(t, f, call)
	if err != nil {
		t.Fatal(err)
	}
	var action chat.Action
	if err := json.Unmarshal(result.Content, &action); err != nil {
		t.Fatal(err)
	}
	return action
}

const browserHostedOrganizationBase = "/api/v2/organizations/org_browser_preview"

type usageReport struct {
	MonthlyCosts *monthlyCostReport `json:"monthly_costs"`
	Total        struct {
		Cost float64 `json:"cost"`
	} `json:"total"`
}
type usageHostedFixture struct {
	service                 *testHub
	project, privateProject string
	actors                  map[string]hubtest.HTTPActor
	provider                *hostedSecurityProvider
}

func newUsageHostedFixture(t *testing.T) usageHostedFixture {
	t.Helper()
	admin := hubtest.Database(t)
	repo := hubtest.RepositoryOn(t, admin)
	details := &memoryRunDetails{}
	now := time.Now().UTC()
	org := tracker.OrganizationID("org_browser_preview")
	if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org), Deployment: "origin", PublicURL: "https://example.test"})
	}); err != nil {
		t.Fatal(err)
	}
	project := hubtest.Project(t, repo, org, hubtest.ProjectOptions{Name: "Public"})
	private := hubtest.Project(t, repo, org, hubtest.ProjectOptions{Name: "Private"})
	provider := newHostedSecurityProvider()
	cfg := Config{Clock: time.Now, RunDetails: details, SecretKeys: keys(t), Hosted: &hostedplans.HostedConfig{OrganizationID: string(org), WorkOSOrganizationID: "provider_" + string(org), Provider: provider, PublicURL: "https://example.test"}}
	h := openHub(t, &testHub{admin: admin, owner: t}, cfg, repo)
	harness := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repo, Organization: org, Now: cfg.Clock}, func(*echo.Group, hubtest.HTTPDeps) {})
	f := usageHostedFixture{service: h, project: string(project.ID), privateProject: string(private.ID), provider: provider, actors: map[string]hubtest.HTTPActor{}}
	for _, entry := range []struct {
		name, role string
		projects   []tracker.ProjectID
	}{{"owner", "owner", []tracker.ProjectID{project.ID, private.ID}}, {"viewer", "viewer", []tracker.ProjectID{project.ID}}} {
		actor := harness.MemberSession(t, hubtest.MemberOptions{Role: entry.role, Projects: entry.projects, Email: entry.name + "@example.test", Now: now})
		f.actors[entry.name] = actor
		var session accessstore.Session
		if err := repo.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			session, err = r.AccessSession(t.Context(), apikey.HashToken(actor.Token))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		member := actor.Member.Member
		membership := auth.Membership{ID: member.MembershipID, UserID: member.UserID, OrganizationID: cfg.Hosted.WorkOSOrganizationID, Status: "active"}
		membership.Role.Slug = entry.role
		provider.members[membership.ID] = membership
		provider.sessions[session.SessionID] = auth.HostedIdentity{Subject: member.UserID, OrganizationID: cfg.Hosted.WorkOSOrganizationID, SessionID: session.SessionID, CreatedAt: session.IdentityCreatedAt, ExpiresAt: session.IdentityExpiresAt}
	}
	return f
}
func (f usageHostedFixture) usage(t *testing.T, account, path string, status int) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	f.actors[account].Apply(request)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	requireNativeStatus(t, response, status)
	return response
}

func (*hostedSecurityProvider) HasUser(context.Context, string) (bool, error) { return true, nil }
func (*hostedSecurityProvider) VerifyAccess(context.Context, string) (auth.HostedAccess, error) {
	return auth.HostedAccess{}, auth.ErrHostedIdentity
}
func (*hostedSecurityProvider) RefreshAccess(context.Context, string, string) (auth.HostedAccess, auth.HostedTokens, error) {
	return auth.HostedAccess{}, auth.HostedTokens{}, auth.ErrHostedIdentity
}
