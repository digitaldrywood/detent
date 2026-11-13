package runnerruntime_test

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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

const testHubAdminToken = "example-runner-runtime-admin"

func TestMain(m *testing.M) { hubtest.Main(m) }

type Config = hubserver.Config

type testHub struct {
	*hubserver.Service
	config Config
}

func (h *testHub) runDetailStore() attachment.DetailStore { return h.config.RunDetails }

type runnerPGFixture struct {
	organization tracker.OrganizationID
	admin        *sql.DB
	bindings     []runnerauth.Binding
	service      *testHub
	server       *httptest.Server
	policy       policystore.Revision
	leases       []tracker.NativeLease
	tokens       []string
	data         []tracker.NativeRunData
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

func startupConfig(t *testing.T, admin *sql.DB) Config {
	t.Helper()
	connection := hubtest.PostgresConfig(t, admin)
	return Config{DatabaseBackend: hubserver.DatabasePostgres, Postgres: &connection, Hosted: &hostedplans.HostedConfig{
		OrganizationID:       "org_startup",
		WorkOSOrganizationID: "provider_org_startup",
		PublicURL:            "https://hub.example.test",
		Provider:             newHostedSecurityProvider(),
	}, RunDetails: &memoryRunDetails{}}
}

func seedHostedStartup(t *testing.T, admin *sql.DB, cfg Config) {
	t.Helper()
	repository := hubtest.RepositoryOn(t, admin)
	organization := tracker.OrganizationID(cfg.Hosted.OrganizationID)
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", Slug: "example-" + strings.TrimPrefix(string(organization), "org_"), CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: cfg.Hosted.WorkOSOrganizationID, BootstrapSubject: cfg.Hosted.BootstrapSubject, Deployment: "origin", PublicURL: cfg.Hosted.PublicURL})
	}); err != nil {
		t.Fatal(err)
	}
}

func newRunnerPGFixture(t *testing.T, customize ...func(*policy.Descriptor)) runnerPGFixture {
	t.Helper()
	return newRunnerPGFixtureWithDetails(t, nil, customize...)
}

func newRunnerPGFixtureWithDetails(t *testing.T, decorate func(attachment.DetailStore) attachment.DetailStore, customize ...func(*policy.Descriptor)) runnerPGFixture {
	t.Helper()
	admin := hubtest.Database(t)
	cfg := startupConfig(t, admin)
	if decorate != nil {
		cfg.RunDetails = decorate(cfg.RunDetails)
	}
	return seedRunnerPGFixture(t, admin, openStartupHub(t, admin, cfg), "org_startup", customize...)
}

func openStartupHub(t *testing.T, admin *sql.DB, cfg Config) *testHub {
	t.Helper()
	seedHostedStartup(t, admin, cfg)
	cfg.SecretKeys = secretKeys(t)
	cfg.InitialAdminToken = []byte(testHubAdminToken)
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	service, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	cfg.TenantRepository = hubtest.RepositoryOn(t, admin)
	return &testHub{Service: service, config: cfg}
}

func tokenFingerprint(hash string) string {
	hash = strings.TrimSpace(hash)
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}

func nativeFixtureStates() []tracker.NativeState { return hubtest.States() }

func hubTestPolicy() policy.Descriptor { return hubtest.PolicyDescriptor() }

func seedRunnerPGFixture(t *testing.T, admin *sql.DB, service *testHub, organization tracker.OrganizationID, customize ...func(*policy.Descriptor)) runnerPGFixture {
	t.Helper()
	var err error
	f := runnerPGFixture{organization: organization, admin: admin, service: service, server: httptest.NewServer(service.Handler())}
	t.Cleanup(f.server.Close)
	now := time.Now().UTC().Truncate(time.Microsecond)
	expires := now.Add(time.Hour)
	descriptor := hubTestPolicy()
	for _, edit := range customize {
		edit(&descriptor)
	}
	policy := descriptor.WithID()
	f.policy, err = eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), organization, policy)
	if err != nil {
		t.Fatal(err)
	}
	f.policy.ProjectID = "prj_runner"
	f.policy.ApprovedAt, f.policy.ApprovedBy = now, "test_hub_admin"
	err = service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: "hub_" + strings.Repeat("a", 32), CursorKey: bytes.Repeat([]byte{0x5a}, 32)}); err != nil {
			return err
		}
		if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_runner", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
			return err
		}
		if err := w.ApproveProjectPolicy(t.Context(), f.policy, ""); err != nil {
			return err
		}
		for index := 1; index <= 2; index++ {
			id := strconv.Itoa(index)
			token, err := apikey.GenerateToken()
			if err != nil {
				return err
			}
			binding := runnerauth.NewBinding()
			f.bindings = append(f.bindings, binding)
			runnerID, machineID, credentialID := binding.RunnerID, string(binding.MachineID), "credential_"+id
			f.tokens = append(f.tokens, token)
			hash := apikey.HashToken(token)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: credentialID, Name: "Example runner " + id, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: credentialID, ProjectID: "prj_runner"}); err != nil {
				return err
			}
			operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: organization, ID: "enrollment_" + id, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenHash: apikey.HashToken("example_enrollment_" + id), Scope: "projects", ProjectIDs: []tracker.ProjectID{"prj_runner"}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "test_hub_admin"}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), "enrollment_"+id, runnerID, tracker.MachineID(machineID), now); err != nil {
				return err
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: machineID, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: organization, RunnerID: runnerID, MachineID: tracker.MachineID(machineID), TokenID: credentialID, EnrollmentID: "enrollment_" + id, Scope: "projects", State: "active", DisplayName: "Example runner " + id, Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{"prj_runner"}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
				return err
			}
			state := "Todo"
			item, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: "prj_runner", NativeID: tracker.NativeWorkItemID("wi_" + id), Number: int64(index), Title: "Example", WorkflowStateName: &state})
			if err != nil {
				return err
			}
			itemRaw, err := json.Marshal(tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: "prj_runner", WorkItemID: item.NativeID, Number: int(item.Number), Revision: tracker.Revision(item.Revision)}, Title: "Example", State: "Todo"})
			if err != nil {
				return err
			}
			itemVersion, err := eventstore.NewContent(service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: "prj_runner", WorkItemID: item.NativeID, RecordID: string(item.NativeID), Revision: item.Revision}, itemRaw)
			if err != nil {
				return err
			}
			if _, err := w.CreateCollaborationVersion(t.Context(), itemVersion); err != nil {
				return err
			}
			lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machineID, SessionID: "execution_" + id, LeaseID: "lease_" + id, TTL: time.Hour})
			if err != nil {
				return err
			}
			if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: organization, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
				return err
			}
			if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: organization, ProjectID: "prj_runner", LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: policy.ID, BoundAt: now}); err != nil {
				return err
			}
			f.leases = append(f.leases, tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: item.NativeID, MachineID: tracker.MachineID(machineID), SessionID: lease.SessionID, FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: policy.ID, ExpiresAt: lease.ExpiresAt})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for index, lease := range f.leases {
		data := tracker.NativeRunData{AttemptID: hubapi.NewID("attempt"), RunID: hubapi.NewID("run"), LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: policy.ID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "example-backend", Model: "example-model"}}
		f.data = append(f.data, data)
		event := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start_" + data.AttemptID}, Type: "run.started", SchemaVersion: 1, Data: data}
		path := "/api/v2/organizations/" + string(organization) + "/projects/prj_runner/work-items/" + string(lease.WorkItemID) + "/events"
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, f.tokens[index], event), http.StatusOK)
	}
	return f
}

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
func (p *hostedSecurityProvider) identity(subject, email, organization, support string) auth.Identity {
	now := time.Now().UTC()
	identity := auth.Identity{Subject: subject, Email: email, Hosted: &auth.HostedIdentity{Subject: subject, OrganizationID: organization, SessionID: hubapi.NewID("session"), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour), SupportActor: support}}
	p.mu.Lock()
	p.sessions[identity.Hosted.SessionID] = *identity.Hosted
	p.mu.Unlock()
	return identity
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
func (f runnerPGFixture) client(t *testing.T, index int) (*api.NativeClient, context.Context) {
	t.Helper()
	client, err := api.New(api.Config{URL: f.server.URL, TokenSource: func() string { return f.tokens[index] }, HTTPClient: &http.Client{Transport: &http.Transport{}}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(f.organization, "prj_runner")
	if err != nil {
		t.Fatal(err)
	}
	return native, api.WithMutationAuthority(t.Context(), native.Scope(), f.leases[index], nil)
}

type nativeMachineHeartbeatRequest struct {
	HostMetrics           json.RawMessage                     `json:"host_metrics,omitempty"`
	ChangeCursor          *string                             `json:"change_cursor,omitempty"`
	Admission             *tracker.NativeAdmissionObservation `json:"admission,omitempty"`
	SpriteName            string                              `json:"sprite_name,omitempty"`
	Update                *runnerauth.UpdateObservation       `json:"update,omitempty"`
	CapacityConfig        *runnerauth.CapacityConfig          `json:"capacity_configuration,omitempty"`
	ProjectConfiguration  *runnerauth.ProjectConfiguration    `json:"project_configuration,omitempty"`
	LocalChecks           *runnerauth.LocalChecks             `json:"local_checks,omitempty"`
	Problems              []runnerauth.Problem                `json:"problems"`
	ProtocolMajor         int                                 `json:"protocol_major,omitempty"`
	SettingsRejected      bool                                `json:"settings_rejected,omitempty"`
	BackendIsolation      isolation.Report                    `json:"backend_isolation,omitempty"`
	ProviderReports       []providercapacity.Report           `json:"provider_reports,omitempty"`
	DisplayName           string                              `json:"display_name"`
	Capacity              int                                 `json:"capacity"`
	Version               string                              `json:"version"`
	OS                    string                              `json:"os,omitempty"`
	Architecture          string                              `json:"architecture,omitempty"`
	WorkspaceCapabilities *workspacesession.Capabilities      `json:"workspace_capabilities,omitempty"`
	WorkspaceIsolation    string                              `json:"workspace_isolation,omitempty"`
	Skills                *[]skills.ProviderSkill             `json:"skills,omitempty"`
	CheckoutRepository    *string                             `json:"checkout_repository,omitempty"`
}

func changeTestInput() tracker.ChangeVersionInput {
	return tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("a", 40), Repository: "https://github.com/example/repo", Code: changeTestArtifact("code"), Artifacts: []tracker.ChangeArtifact{changeTestArtifact("manifest")}, PolicyID: hubTestPolicy().ID}
}
func changeTestArtifact(kind string) tracker.ChangeArtifact {
	return tracker.ChangeArtifact{Kind: kind, URI: "s3://customer/change/" + kind, SHA256: policy.Digest([]byte(kind)), Availability: "available"}
}
func changeTestSource(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) []byte {
	t.Helper()
	bundle, _ := changeTestSourceCheckout(t, input, content...)
	return bundle
}
func changeTestSourceCheckout(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) ([]byte, string) {
	t.Helper()
	directory := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Source Fixture", "-c", "user.email=source@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("source fixture git %v: %s: %v", args, output, err)
		}
		return string(output)
	}
	git("init", "-b", "main", "--object-format=sha1", "--template=")
	path := filepath.Join(directory, "source.txt")
	if err := os.WriteFile(path, []byte("base source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-m", "base")
	base := strings.TrimSpace(git("rev-parse", "HEAD"))
	data := []byte(input.HeadSHA + "\n")
	if len(content) > 0 && content[0] != nil {
		data = content[0]
		if err := os.WriteFile(path, append(bytes.Clone(data), []byte("draft source\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "source.txt")
		git("commit", "-m", "source draft")
	}
	if len(content) == 0 || content[0] != nil {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "source.txt")
	}
	git("commit", "--allow-empty", "-m", "reviewed source")
	head := strings.TrimSpace(git("rev-parse", "HEAD"))
	bundlePath := filepath.Join(directory, "source.bundle")
	git("bundle", "create", bundlePath, "HEAD", "^"+base)
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	diff := git("diff", "--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", base, head, "--")
	input.BaseSHA, input.HeadSHA, input.MergeBaseSHA = base, head, base
	input.Source = &tracker.ChangeSource{
		Format: "git-bundle", BaseSHA: base, HeadSHA: head,
		BundleSHA256: tracker.ChangeSourceDigest(bundle),
		DiffSHA256:   tracker.ChangeSourceDigest([]byte(diff)),
		Bytes:        int64(len(bundle)),
	}
	return bundle, directory
}
