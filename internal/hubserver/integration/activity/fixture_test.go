package activity_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/activity"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/labstack/echo/v4"
)

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

func (*hostedSecurityProvider) HasUser(context.Context, string) (bool, error) { return true, nil }
func (*hostedSecurityProvider) VerifyAccess(context.Context, string) (auth.HostedAccess, error) {
	return auth.HostedAccess{}, auth.ErrHostedIdentity
}
func (*hostedSecurityProvider) RefreshAccess(context.Context, string, string) (auth.HostedAccess, auth.HostedTokens, error) {
	return auth.HostedAccess{}, auth.HostedTokens{}, auth.ErrHostedIdentity
}

type runtimeHistoryTraceKey struct{}

type runtimeHistoryTraceCapture struct {
	mu         sync.Mutex
	statements []tracedStatement
}

type runtimeHistoryTrace struct{}

func (runtimeHistoryTrace) record(ctx context.Context, query string, args []any) {
	capture, ok := ctx.Value(runtimeHistoryTraceKey{}).(*runtimeHistoryTraceCapture)
	if !ok {
		return
	}
	values := slices.Clone(args)
	for i, value := range values {
		switch value := value.(type) {
		case []byte:
			values[i] = slices.Clone(value)
		case []string:
			values[i] = slices.Clone(value)
		case []int64:
			values[i] = slices.Clone(value)
		case []tracker.NativeWorkItemID:
			values[i] = slices.Clone(value)
		case []tracker.ProjectID:
			values[i] = slices.Clone(value)
		}
	}
	capture.mu.Lock()
	capture.statements = append(capture.statements, tracedStatement{query: query, args: values})
	capture.mu.Unlock()
}

func (trace runtimeHistoryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	trace.record(ctx, data.SQL, data.Args)
	return ctx
}

func (runtimeHistoryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (runtimeHistoryTrace) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	return ctx
}

func (trace runtimeHistoryTrace) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	trace.record(ctx, data.SQL, data.Args)
}

func (runtimeHistoryTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

func TestMain(m *testing.M) { hubtest.Main(m) }

type Config = hubserver.Config
type HostedConfig = hubserver.HostedConfig
type apiCredential = hubauth.Credential
type nativeScope struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
}

const apiScopeOperator = hubauth.Operator
const hostedProjectsSelected = "selected"
const hostedCookie = hubauth.Cookie
const hubReadTimeout = hubapi.ReadTimeout
const nativeContractTestBody = "Example issue body"
const testHubAdminToken = "detent_test_hub_admin_token_00000000000000000000000000000000"

var newNativeID = hubapi.NewID

func tokenFingerprint(hash string) string        { return hash[:12] }
func formatHubTime(v time.Time) string           { return v.UTC().Format(time.RFC3339Nano) }
func nativeFixtureStates() []tracker.NativeState { return hubtest.States() }
func discardLogger() *slog.Logger                { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func usageDecode(t *testing.T, r *httptest.ResponseRecorder, value any) {
	t.Helper()
	decodeHubResponse(t, r, value)
}
func decodeHubResponse(t *testing.T, r *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}
func requireNativeStatus(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status = %d, want %d: %s", r.Code, want, r.Body.String())
	}
}
func browserHostedStatus(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	requireNativeStatus(t, r, want)
}
func projectRecordPostgresWithAdmin(t *testing.T, count int) (*postgres.Repository, *sql.DB) {
	t.Helper()
	admin := hubtest.Database(t)
	return hubtest.RepositoryOn(t, admin, hubtest.WithMaxConnections(count)), admin
}
func startupConnection(t *testing.T, admin *sql.DB, group, attributes string) pgtransport.Connection {
	t.Helper()
	return hubtest.PostgresConfig(t, admin).Connection
}
func nativeConfig(t *testing.T, admin *sql.DB, user, password string) *pgxpool.Config {
	t.Helper()
	return hubtest.NativeConfig(t, admin, user, password)
}

type testDatabase struct{ postgres *postgres.Repository }
type testHub struct {
	service        *hubserver.Service
	rootMu         sync.Mutex
	config         Config
	database       *testDatabase
	admin          *sql.DB
	owner          *testing.T
	echo           *echo.Echo
	activityRoutes bool
	harness        *hubtest.HTTPHarness
}

func (h *testHub) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.activityRoutes && strings.HasSuffix(r.URL.Path, "/activity") {
			h.harness.Echo.ServeHTTP(w, r)
			return
		}
		if err := h.ensureRoot(); err != nil {
			http.Error(w, "open test Hub failed", http.StatusInternalServerError)
			return
		}
		h.service.Handler().ServeHTTP(w, r)
	})
}
func (h *testHub) runDetailStore() attachment.DetailStore { return h.config.RunDetails }
func (h *testHub) readActivityReport(ctx context.Context, credential apiCredential, request operatortool.ActivityRequest, window operatortool.AnalyticsWindow) (operatortool.ActivityReport, error) {
	a := hubauth.New(hubauth.Deps{Repository: h.config.TenantRepository, AccessRepository: h.config.AccessRepository, Organization: tracker.OrganizationID(h.config.Hosted.OrganizationID), Now: h.config.Clock, Hosted: &hubauth.Hosted{OrganizationID: h.config.Hosted.OrganizationID}})
	return activity.New(activity.Deps{Repository: h.config.TenantRepository, Details: h.runDetailStore, Now: h.config.Clock, Organization: func() string { return h.config.Hosted.OrganizationID }, MissingParticipants: errors.New("project lifecycle participants are unavailable")}).Read(ctx, activity.Caller{Credential: credential, Scope: a.Scope(hubauth.Input{Organization: tracker.OrganizationID(h.config.Hosted.OrganizationID), Credential: credential}), IsNotFound: func(err error) bool {
		var failure *hubapi.Error
		return errors.As(err, &failure) && failure.Code == "not_found"
	}}, request, window)
}

type dynamicRepository struct {
	*postgres.Repository
	hub *testHub
}

func (r dynamicRepository) ReadTenant(ctx context.Context, org tracker.OrganizationID, w corestore.Workload, f func(tenantstore.Reader) error) error {
	return r.hub.config.TenantRepository.ReadTenant(ctx, org, w, f)
}
func (r dynamicRepository) ReadTenantSnapshot(ctx context.Context, org tracker.OrganizationID, w corestore.Workload, f func(tenantstore.Reader) error) error {
	return r.hub.config.TenantRepository.(tenantstore.SnapshotRepository).ReadTenantSnapshot(ctx, org, w, f)
}
func (r dynamicRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, w corestore.Workload, f func(tenantstore.Writer) error) error {
	return r.hub.config.TenantRepository.WriteTenant(ctx, org, w, f)
}
func (r dynamicRepository) ReadRequest(ctx context.Context, org tracker.OrganizationID, f func(context.Context) error) error {
	return r.hub.config.TenantRepository.(tenantstore.RequestReader).ReadRequest(ctx, org, f)
}
func openHub(t *testing.T, admin *sql.DB, repo *postgres.Repository, cfg Config) *testHub {
	t.Helper()
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.RunDetails == nil {
		cfg.RunDetails = &memoryRunDetails{}
	}
	cfg.SecretKeys = keys(t)
	cfg.Logger = discardLogger()
	pg := hubtest.PostgresConfig(t, admin, hubtest.WithClock(cfg.Clock))
	cfg.Postgres = &pg
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	cfg.TenantRepository = repo
	cfg.AccessRepository = repo
	cfg.BillingRepository = repo
	cfg.InitialAdminToken = []byte(testHubAdminToken)
	if cfg.Hosted != nil {
		if cfg.Hosted.WorkOSOrganizationID == "" {
			cfg.Hosted.WorkOSOrganizationID = "provider_" + cfg.Hosted.OrganizationID
		}
		if cfg.Hosted.BootstrapSubject == "" {
			cfg.Hosted.BootstrapSubject = "user"
		}
		if cfg.Hosted.PublicURL == "" {
			cfg.Hosted.PublicURL = "https://example.test"
		}
	}
	h := &testHub{config: cfg, database: &testDatabase{repo}, admin: admin, owner: t, echo: echo.New()}
	proxy := dynamicRepository{repo, h}
	if cfg.Hosted != nil {
		h.harness = hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: proxy, Organization: tracker.OrganizationID(cfg.Hosted.OrganizationID), Now: cfg.Clock, Hosted: &hubauth.Hosted{OrganizationID: cfg.Hosted.OrganizationID}}, func(g *echo.Group, d hubtest.HTTPDeps) {
			httpService := activity.NewHTTP(activity.HTTPDeps{Service: func() *activity.Service {
				return activity.New(activity.Deps{Repository: h.config.TenantRepository, Details: h.runDetailStore, Now: h.config.Clock, Organization: func() string { return h.config.Hosted.OrganizationID }, MissingParticipants: errors.New("project lifecycle participants are unavailable")})
			}, Now: cfg.Clock, Available: func() bool { return true }, Caller: func(ctx context.Context) (activity.Caller, error) {
				input, err := hubauth.Resolve(ctx)
				return activity.Caller{Credential: input.Credential, Scope: d.Auth.Scope(input), IsNotFound: func(err error) bool {
					var failure *hubapi.Error
					return errors.As(err, &failure) && failure.Code == "not_found"
				}}, err
			}, ReadError: hubauth.Classify, Result: func(value any) (operatortool.Result, error) {
				raw, err := json.Marshal(value)
				return operatortool.Result{Content: raw}, err
			}, Error: d.Error, Refuse: func(c echo.Context, status int, code, message string, body any) error { return c.JSON(status, body) }})
			g.GET("/api/v2/organizations/:organization/activity", httpService.Serve, d.Operator)
		})
	}
	return h
}
func performHubAPIRequest(t *testing.T, h *testHub, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	if h.service == nil {
		opened := openHub(t, h.admin, h.database.postgres, h.config)
		h.service = opened.service
		h.config = opened.config
	}
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.Handler().ServeHTTP(w, r)
	return w
}

type browserHostedFixture struct {
	service                 *testHub
	provider                *hostedSecurityProvider
	cookies                 map[string]*http.Cookie
	project, privateProject string
}

func newPostgresBrowserHostedFixtureServing(t *testing.T, allocated bool, organization string, listen bool, configure ...func(*Config)) *browserHostedFixture {
	t.Helper()
	admin := hubtest.Database(t)
	provider := newHostedSecurityProvider()
	cfg := Config{Hosted: &HostedConfig{OrganizationID: organization, WorkOSOrganizationID: "provider_" + organization, Provider: provider, PublicURL: "https://browser.example.test", BootstrapSubject: "user_browser_owner"}}
	for _, apply := range configure {
		apply(&cfg)
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	repo := activityRepository(t, admin, cfg.Clock)
	if err := repo.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: tracker.OrganizationID(organization), Name: organization, Slug: strings.ReplaceAll(organization, "_", "-"), CreatedAt: cfg.Clock()}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: tracker.OrganizationID(organization), ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, Deployment: "origin", PublicURL: cfg.Hosted.PublicURL})
	}); err != nil {
		t.Fatal(err)
	}
	seedIdentity(t, repo, tracker.OrganizationID(organization), cfg.Clock())
	h := openHub(t, admin, repo, cfg)
	h.activityRoutes = true
	f := &browserHostedFixture{service: h, provider: provider, cookies: map[string]*http.Cookie{}}
	for _, account := range []struct{ name, user, role string }{{"owner", "user_browser_owner", "owner"}, {"viewer", "user_browser_viewer", "viewer"}, {"invitee", "user_browser_invitee", "viewer"}} {
		identity := provider.identity(account.user, account.name+"@example.test", cfg.Hosted.WorkOSOrganizationID, "")
		membership, err := provider.CreateMembership(t.Context(), identity.Subject, identity.Hosted.OrganizationID, account.role)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.addHostedMember(t.Context(), identity, membership); err != nil {
			t.Fatal(err)
		}
		token, _ := browserHostedSession(t, h, identity)
		f.cookies[account.name] = &http.Cookie{Name: hostedCookie, Value: token}
	}
	f.project = f.createProject(t, "Example activity")
	f.privateProject = f.createProject(t, "Example private")
	if err := repo.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		viewer := "hosted_" + apikey.HashToken("user_browser_viewer")[:32]
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: tracker.OrganizationID(organization), UserID: "user_browser_viewer", ProjectID: tracker.ProjectID(f.project)}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(organization), TokenID: viewer, ProjectID: tracker.ProjectID(f.project)})
	}); err != nil {
		t.Fatal(err)
	}
	return f
}
func seedIdentity(t *testing.T, repo tenantstore.Repository, org tracker.OrganizationID, now time.Time) {
	t.Helper()
	if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: hubapi.NewID("hub"), CursorKey: bytes.Repeat([]byte{0x5a}, 32)})
	}); err != nil {
		t.Fatal(err)
	}
}
func (f *browserHostedFixture) createProject(t *testing.T, name string) string {
	t.Helper()
	org := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
	project := hubtest.Project(t, f.service.config.TenantRepository, org, hubtest.ProjectOptions{Name: name})
	hubtest.Policy(t, f.service.config.TenantRepository, f.service.runDetailStore(), project, hubtest.PolicyOptions{Now: f.service.config.Clock()})
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "user_browser_owner", ProjectID: project.ID, CanWrite: true}); err != nil {
			return err
		}
		return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "hosted_" + apikey.HashToken("user_browser_owner")[:32], ProjectID: project.ID})
	}); err != nil {
		t.Fatal(err)
	}
	return string(project.ID)
}
func (f *browserHostedFixture) page(t *testing.T, account, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.AddCookie(f.cookies[account])
	w := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(w, r)
	return w
}
func (h *testHub) addHostedMember(ctx context.Context, identity auth.Identity, membership auth.Membership) error {
	org := tracker.OrganizationID(h.config.Hosted.OrganizationID)
	now := h.config.Clock()
	principal := "hosted_" + apikey.HashToken(identity.Subject)[:32]
	return h.config.TenantRepository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(ctx, accessstore.Credential{OrganizationID: org, ID: principal, Hash: apikey.HashToken(principal), Name: identity.Email, Scope: "operator", NativeOnly: true, Kind: "personal", KeyScope: "write", ProjectAccess: "selected", UserID: identity.Subject, MembershipID: membership.ID, CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessMember(ctx, accessstore.Member{OrganizationID: org, UserID: identity.Subject, Email: identity.Email, MembershipID: membership.ID, Role: membership.Role.Slug, PrincipalID: principal, Active: true, CreatedAt: now, UpdatedAt: now})
	})
}
func (p *hostedSecurityProvider) identity(subject, email, organization, support string) auth.Identity {
	now := time.Now().UTC()
	identity := auth.Identity{Subject: subject, Email: email, Hosted: &auth.HostedIdentity{Subject: subject, OrganizationID: organization, SessionID: hubapi.NewID("session"), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), SupportActor: support}}
	p.mu.Lock()
	p.sessions[identity.Hosted.SessionID] = *identity.Hosted
	p.mu.Unlock()
	return identity
}
func browserHostedSession(t *testing.T, h *testHub, identity auth.Identity) (string, auth.Session) {
	t.Helper()
	var member accessstore.Member
	var principal accessstore.Credential
	org := tracker.OrganizationID(h.config.Hosted.OrganizationID)
	if err := h.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), identity.Subject)
		if err != nil {
			return err
		}
		principal, err = r.AccessCredential(t.Context(), member.PrincipalID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	actor := h.harness.SessionFor(t, hubtest.SeededMember{Member: member, Principal: hubtest.SeededCredential{Organization: org, ID: principal.ID, Hash: principal.Hash, Kind: principal.Kind, UserID: member.UserID, MembershipID: member.MembershipID}}, "", "")
	var row accessstore.Session
	if err := h.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		row, err = r.AccessSession(t.Context(), apikey.HashToken(actor.Token))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	hosted := &auth.HostedIdentity{Subject: row.Subject, OrganizationID: row.ProviderOrganizationID, SessionID: row.SessionID, CreatedAt: row.IdentityCreatedAt, ExpiresAt: row.IdentityExpiresAt}
	provider := h.config.Hosted.Provider.(*hostedSecurityProvider)
	provider.mu.Lock()
	provider.sessions[hosted.SessionID] = *hosted
	provider.mu.Unlock()
	return actor.Token, auth.Session{Email: row.Email, Identity: hosted, ExpiresAt: row.ExpiresAt}
}

func hostedPostgresAdmin(t *testing.T, h *testHub) *sql.DB { t.Helper(); return h.admin }

type nativeFixture struct {
	service *testHub
	project tracker.NativeProject
}

func updateWorkflowPolicyFixtureProject(t *testing.T, f nativeFixture, apply func(*projectstore.Record)) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		apply(&project)
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

type authorityRequestKey struct{}

func (h *testHub) operatorAuthority(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.SetRequest(c.Request().WithContext(context.WithValue(c.Request().Context(), authorityRequestKey{}, c.Request())))
		return next(c)
	}
}
func (h *testHub) executeActivityRead(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	if call.Name == "" {
		call.Name = operatortool.Activity
	}
	return (hubFleetExecutor{h}).Execute(ctx, call)
}

type hubFleetExecutor struct{ service *testHub }
type hostedOperatorExecutor = hubFleetExecutor

func (e hubFleetExecutor) OpenConnection(context.Context) error { return nil }
func (e hubFleetExecutor) ListTools(ctx context.Context) ([]operatortool.Definition, error) {
	raw, err := e.request(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []operatortool.Definition `json:"tools"`
	}
	err = json.Unmarshal(raw, &out)
	return out.Tools, err
}
func (e hubFleetExecutor) Execute(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	raw, err := e.request(ctx, "tools/call", map[string]any{"name": call.Name, "arguments": call.Arguments})
	if err != nil {
		return operatortool.Result{}, err
	}
	var out struct {
		Data    json.RawMessage `json:"structuredContent"`
		Content []struct {
			Text string `json:"text"`
		}
		IsError bool `json:"isError"`
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return operatortool.Result{}, err
	}
	if out.IsError {
		return operatortool.Result{}, fmt.Errorf("MCP result: %s", raw)
	}
	if len(out.Data) == 0 && len(out.Content) > 0 {
		out.Data = json.RawMessage(out.Content[0].Text)
	}
	return operatortool.Result{Content: out.Data}, nil
}
func (e hubFleetExecutor) request(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	request, ok := ctx.Value(authorityRequestKey{}).(*http.Request)
	if !ok {
		return nil, operatortool.ErrAccessDenied
	}
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = json.RawMessage(fleetProtocolMeta)
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v2/organizations/"+e.service.config.Hosted.OrganizationID+"/mcp", bytes.NewReader(raw))
	r.Header = request.Header.Clone()
	if cookie, err := request.Cookie(hubauth.Cookie); err == nil {
		sum := sha256.Sum256([]byte("detent-hosted-csrf:" + cookie.Value))
		r.Header.Set("X-CSRF-Token", hex.EncodeToString(sum[:]))
	}

	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("Mcp-Protocol-Version", mcp.ProtocolVersion)
	r.Header.Set("Mcp-Method", method)
	if name, ok := params["name"].(string); ok {
		r.Header.Set("Mcp-Name", name)
	}
	w := httptest.NewRecorder()
	e.service.Handler().ServeHTTP(w, r)
	if w.Code != 200 {
		return nil, fmt.Errorf("MCP status %d: %s", w.Code, w.Body.String())
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		return nil, err
	}
	if len(out.Error) > 0 {
		return nil, fmt.Errorf("MCP error: %s", out.Error)
	}
	return out.Result, nil
}

const fleetProtocolMeta = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"example-activity-client","version":"1"}}`

func nativeAnalyticsStdioCall(t *testing.T, ctx context.Context, executor mcp.Executor, name string) json.RawMessage {
	t.Helper()
	input, inputWriter := io.Pipe()
	output, outputWriter := io.Pipe()
	done := make(chan error, 1)
	t.Cleanup(func() { input.Close(); inputWriter.Close(); output.Close(); outputWriter.Close() })
	go func() { done <- mcp.NewServer(executor, "test").Serve(ctx, input, outputWriter) }()
	request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{},"_meta":` + fleetProtocolMeta + `}}` + "\n"
	if _, err := io.WriteString(inputWriter, request); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Result struct {
			Data json.RawMessage `json:"structuredContent"`
		} `json:"result"`
	}
	var frame json.RawMessage
	if err := json.NewDecoder(output).Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatal(err)
	}
	if err := inputWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(envelope.Result.Data) == 0 || string(envelope.Result.Data) == "null" {
		t.Fatalf("stdio frame=%s", frame)
	}
	return envelope.Result.Data
}

type tracedStatement struct {
	query string
	args  []any
}
type Service = testHub
type database = testDatabase

func decodeActivityRun(ctx context.Context, q any, raw string, data *tracker.NativeRunData) error {
	return reportdata.DecodeRun(ctx, raw, data, func(_ context.Context, raw []byte, v any) error { return json.Unmarshal(raw, v) }, func(_ context.Context, raw string, data *tracker.NativeRunData) error {
		return json.Unmarshal([]byte(raw), data)
	})
}

func (r dynamicRepository) ReadRunnerAuthorized(ctx context.Context, identity tenantauth.RequestIdentity, f tenantstore.AuthorizedRead) error {
	return r.hub.config.TenantRepository.(tenantstore.RequestRepository).ReadRunnerAuthorized(ctx, identity, f)
}
func (r dynamicRepository) WriteRunnerAuthorized(ctx context.Context, identity tenantauth.RequestIdentity, f tenantstore.AuthorizedWrite) error {
	return r.hub.config.TenantRepository.(tenantstore.RequestRepository).WriteRunnerAuthorized(ctx, identity, f)
}
func (r dynamicRepository) ReadAuthorized(ctx context.Context, scope tenantauth.Scope, requirements []tenantauth.Requirement, f tenantstore.AuthorizedRead) error {
	return r.hub.config.TenantRepository.(tenantstore.RequestRepository).ReadAuthorized(ctx, scope, requirements, f)
}
func (r dynamicRepository) WriteAuthorized(ctx context.Context, scope tenantauth.Scope, requirements []tenantauth.Requirement, f tenantstore.AuthorizedWrite) error {
	return r.hub.config.TenantRepository.(tenantstore.RequestRepository).WriteAuthorized(ctx, scope, requirements, f)
}

func (h *testHub) ensureRoot() error {
	h.rootMu.Lock()
	defer h.rootMu.Unlock()
	if h.service != nil {
		return nil
	}
	service, err := hubserver.Open(h.owner.Context(), h.config)
	if err != nil {
		h.owner.Errorf("open test Hub: %v", err)
		return err
	}
	h.service = service
	h.owner.Cleanup(func() {
		if err := service.Close(); err != nil {
			h.owner.Error(err)
		}
	})
	return nil
}

func activityRepository(t *testing.T, admin *sql.DB, clock func() time.Time) *postgres.Repository {
	t.Helper()
	cfg := hubtest.PostgresConfig(t, admin, hubtest.WithClock(clock))
	native, err := cfg.Connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	native.MaxConns = int32(cfg.Pool.MaxConnections)
	pool, err := pgxpool.NewWithConfig(t.Context(), native)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	repository, err := postgres.NewRepository(t.Context(), pool, cfg.Pool, cfg.LeaseClock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	return repository
}
