package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

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

type hostedSecurityUser struct {
	identity auth.Identity
	token    string
}

type hostedSecurityFixture struct {
	service  *Service
	provider *hostedSecurityProvider
	project  tracker.ProjectID
	base     string
}

func TestHostedSchemaFixtureTenantIsolation(t *testing.T) {
	t.Parallel()
	for _, organization := range []string{"org_first", "org_second", "org_third"} {
		t.Run(organization, func(t *testing.T) {
			t.Parallel()
			service := openHostedPostgresService(t, Config{
				Hosted: &hostedplans.HostedConfig{
					OrganizationID:       organization,
					WorkOSOrganizationID: "provider_" + organization,
					PublicURL:            "https://" + organization + ".example.test",
					Provider:             newHostedSecurityProvider(),
				},
			})
			version, err := postgres.LatestSchemaVersion()
			if err != nil {
				t.Fatal(err)
			}
			admin := hostedPostgresAdmin(t, service)
			var applied int64
			if err := admin.QueryRowContext(t.Context(), "SELECT max(version_id) FROM goose_db_version WHERE is_applied").Scan(&applied); err != nil {
				t.Fatal(err)
			}
			if service.database.schemaVersion != version || applied != version {
				t.Fatalf("schema version = %d, want %d", applied, version)
			}
			var boundOrganization string
			if err := service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(r tenantstore.Reader) error {
				tenant, err := r.AccessTenant(t.Context())
				boundOrganization = string(tenant.OrganizationID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if boundOrganization != organization {
				t.Fatalf("tenant binding = %q, want %q", boundOrganization, organization)
			}
			var sessions int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_sessions WHERE organization_id=$1", organization).Scan(&sessions); err != nil {
				t.Fatal(err)
			}
			if sessions != 0 {
				t.Fatalf("new fixture contains %d sessions", sessions)
			}
			now := service.config.now().UTC().Truncate(time.Microsecond)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
				return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: tracker.OrganizationID(organization), Hash: apikey.HashToken("same-token"), Email: "fixture@example.test", Subject: "fixture_" + organization, ProviderOrganizationID: "provider_" + organization, SessionID: "session_" + organization, CreatedAt: now, ExpiresAt: now.Add(time.Hour), IdentityCreatedAt: now, IdentityExpiresAt: now.Add(time.Hour)})
			}); err != nil {
				t.Fatal(err)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.AccessSession(t.Context(), apikey.HashToken("same-token"))
				if err == nil && (row.OrganizationID != tracker.OrganizationID(organization) || row.SessionID != "session_"+organization) {
					return errors.New("session crossed tenant binding")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), "CREATE TABLE fixture_probe (id bigint PRIMARY KEY)"); err != nil {
				t.Fatalf("fixture schema was shared: %v", err)
			}
			if err := service.database.health(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("seeded security copies", func(t *testing.T) {
		for range 2 {
			f := newHostedSecurityFixture(t)
			var sessions int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_sessions WHERE organization_id=$1", f.service.accessOrganization()).Scan(&sessions); err != nil {
				t.Fatal(err)
			}
			if sessions != 0 {
				t.Fatalf("security template contains %d sessions", sessions)
			}
			// The same user and issue must be fresh in each seeded copy.
			owner := f.user(t, "owner", "owner", "owner@example.test", "write", "")
			response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "isolation"}, Title: "isolated", State: "Todo"})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			if issue.Number != 1 {
				t.Fatalf("security fixture issue number = %d, want 1", issue.Number)
			}
		}
	})
}

func newHostedSecurityFixture(t *testing.T, configure ...func(*Config)) hostedSecurityFixture {
	t.Helper()
	provider := newHostedSecurityProvider()
	legacyPlans := hostedplans.PilotPlans()
	cfg := Config{
		Hosted: &hostedplans.HostedConfig{
			Plans:                &legacyPlans,
			OrganizationID:       "org_security",
			WorkOSOrganizationID: "org_provider",
			BootstrapSubject:     "user_owner",
			PublicURL:            "http://127.0.0.1:7777",
			StaffEmails:          []string{"staff@example.test", "support@example.test"},
			SupportActors:        []string{"support@example.test"},
			Provider:             provider,
		},
	}
	for _, apply := range configure {
		apply(&cfg)
	}
	service := openHostedPostgresService(t, cfg)
	project := tracker.ProjectID("prj_security")
	seedHostedPostgresProject(t, service, project)
	return hostedSecurityFixture{service: service, provider: provider, project: project, base: "/api/v2/organizations/org_security/projects/" + string(project)}
}

func seedHostedSecurityProject(ctx context.Context, repository tenantstore.Repository, organization tracker.OrganizationID) error {
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}
	return repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.CreateProjectRecord(ctx, projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_security", Name: "private-project-sentinel", Profile: "native", CreatedAt: time.Now().UTC()}, States: states})
		return err
	})
}

func (f hostedSecurityFixture) user(t *testing.T, name, role, email, grant, supportActor string) hostedSecurityUser {
	t.Helper()
	now := time.Now().UTC()
	identity := auth.Identity{Subject: "user_" + name, Email: email, EmailVerified: true, Hosted: &auth.HostedIdentity{
		Subject: "user_" + name, OrganizationID: "org_provider", SessionID: "session_" + name,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), SupportActor: supportActor,
	}}
	if supportActor != "" {
		identity.Hosted.SupportReason = "customer-request"
	}
	f.provider.mu.Lock()
	f.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
	f.provider.mu.Unlock()
	membership, err := f.provider.CreateMembership(t.Context(), identity.Subject, identity.Hosted.OrganizationID, role)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.addHostedMember(t.Context(), identity, membership); err != nil {
		t.Fatal(err)
	}
	token, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	user := hostedSecurityUser{identity: identity, token: token}
	if grant != "" {
		f.grant(t, user, grant == "write", false)
	}
	return user
}

func (f hostedSecurityFixture) withoutProjectAccess(t *testing.T, user hostedSecurityUser) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.DeleteAccessProjectGrant(t.Context(), user.identity.Subject, f.project); err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if err := w.DeleteAccessTokenGrant(t.Context(), member.PrincipalID, f.project); err != nil && !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (f hostedSecurityFixture) grant(t *testing.T, user hostedSecurityUser, write, runner bool) {
	t.Helper()
	f.grantContext(t.Context(), t, user, write, runner)
}

func (f hostedSecurityFixture) grantContext(ctx context.Context, t *testing.T, user hostedSecurityUser, write, runner bool) {
	t.Helper()
	if err := f.service.config.TenantRepository.WriteTenant(ctx, "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(ctx, user.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(ctx, accessstore.ProjectGrant{OrganizationID: "org_security", UserID: user.identity.Subject, ProjectID: f.project, CanWrite: write, ManageRunner: runner}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: "org_security", TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
}

func (f hostedSecurityFixture) seedIssue(t *testing.T, number int) tracker.NativeWorkItemID {
	t.Helper()
	organization := f.service.accessOrganization()
	now := f.service.config.now()
	issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: f.project, WorkItemID: tracker.NativeWorkItemID(hubapi.NewID("wi")), Number: number, Revision: 1, Profile: "native"}, State: "Todo", Title: "private-issue-sentinel", Body: "private-body-sentinel", CreatedAt: now, UpdatedAt: now, LastActivityAt: now}
	fields, err := f.service.prepareWorkItemFields(t.Context(), organization, hubrecords.WorkItemRecordPlan{Issue: issue, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(f.service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: f.project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: 1}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: organization, ProjectID: f.project, NativeID: issue.WorkItemID, Number: int64(number), Revision: 1, Title: issue.Title, BodyObjectKey: &fields.Body, WorkflowStateName: &issue.State, CreatedAt: now, UpdatedAt: now, NativeCreatedAt: now, NativeUpdatedAt: now, LastActivityAt: now}); err != nil {
			return err
		}
		project, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		if err := hubrecords.WriteClaimFactsAndQueue(t.Context(), w, hubrecords.WorkItemRecordPlan{Project: project, Issue: issue, Create: true}, nil); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return issue.WorkItemID
}

func (f hostedSecurityFixture) request(t *testing.T, user hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	contentType := ""
	switch value := body.(type) {
	case nil:
	case url.Values:
		reader = strings.NewReader(value.Encode())
		contentType = "application/x-www-form-urlencoded"
	default:
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
		contentType = "application/json"
	}
	request := httptest.NewRequest(method, path, reader)
	if user.token != "" {
		request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
		request.Header.Set("X-CSRF-Token", hostedCSRF(user.token))
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func TestHostedSecurityRoleProjectMatrix(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"owner", "admin", "member", "viewer"} {
		for _, grant := range []string{"", "read", "write"} {
			t.Run(role+"/"+grant, func(t *testing.T) {
				t.Parallel()
				f := newHostedSecurityFixture(t)
				user := f.user(t, role, role, role+"@example.test", grant, "")
				if grant == "" {
					f.withoutProjectAccess(t, user)
				}
				readStatus := http.StatusNotFound
				if grant != "" {
					readStatus = http.StatusOK
				}
				requireNativeStatus(t, f.request(t, user, http.MethodGet, f.base, nil), readStatus)
				writeStatus := http.StatusNotFound
				if grant != "" {
					writeStatus = http.StatusForbidden
				}
				if grant == "write" && role != "viewer" {
					writeStatus = http.StatusOK
				}
				command := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create"}, Title: "new issue", Body: "private-body-sentinel" + issueContractTestSections, State: "Todo"}
				requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/work-items", command), writeStatus)
				policyStatus, revokeStatus := http.StatusNotFound, http.StatusNotFound
				if grant == "read" || grant == "write" && role == "viewer" {
					policyStatus, revokeStatus = http.StatusForbidden, http.StatusForbidden
				}
				if grant == "write" && (role == "owner" || role == "admin") {
					policyStatus, revokeStatus = http.StatusOK, http.StatusGone
				}
				descriptor := hubTestPolicy()
				for _, route := range []string{"/policy", "/onboarding/policy"} {
					requireNativeStatus(t, f.request(t, user, http.MethodPut, f.base+route, policy.Change{Policy: descriptor}), policyStatus)
				}
				requireNativeStatus(t, f.request(t, user, http.MethodDelete, f.base+"/policy", map[string]string{"expected_policy_id": descriptor.ID}), revokeStatus)
				requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/claims", map[string]any{}), http.StatusForbidden)
				runnerStatus := http.StatusNotFound
				if role == "owner" || role == "admin" {
					runnerStatus = http.StatusOK
				}
				requireNativeStatus(t, f.request(t, user, http.MethodGet, "/api/v2/organizations/org_security/runners", nil), runnerStatus)
			})
		}
	}
}

func TestHostedSecurityStaffMetadataBoundary(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	f.seedIssue(t, 1)
	staff := f.user(t, "staff", "owner", "staff@example.test", "write", "")
	customer := f.user(t, "customer", "owner", "customer@example.test", "write", "")
	bootstrapToken := hubapi.NewID("token")
	seeded := f.service.config.now().UTC()
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_security", ID: bootstrapTokenID, Name: "Bootstrap administrator", Hash: apikey.HashToken(bootstrapToken), Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: seeded, UpdatedAt: seeded})
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		path   string
		user   hostedSecurityUser
		bearer string
		want   int
	}{
		{name: "staff report", path: "/api/cloud/metadata", user: staff, want: http.StatusOK},
		{name: "reporting bearer", path: "/api/cloud/metadata", bearer: bootstrapToken, want: http.StatusOK},
		{name: "ordinary admin bearer", path: "/api/cloud/metadata", bearer: testHubAdminToken, want: http.StatusForbidden},
		{name: "customer report", path: "/api/cloud/metadata", user: customer, want: http.StatusForbidden},
		{name: "owner billing report", path: "/api/cloud/billing", user: customer, want: http.StatusOK},
		{name: "staff native content", path: f.base + "/work-items", user: staff, want: http.StatusForbidden},
		{name: "staff organization members", path: "/api/v2/organizations/org_security/members", user: staff, want: http.StatusForbidden},
		{name: "staff fleet", path: "/api/v2/organizations/org_security/fleet", user: staff, want: http.StatusForbidden},
		{name: "bootstrap native content", path: f.base + "/work-items", bearer: testHubAdminToken, want: http.StatusNotFound},
		{name: "staff legacy content", path: "/api/v1/work-items", user: staff, want: http.StatusNotFound},
		{name: "health", path: "/health", bearer: testHubAdminToken, want: http.StatusOK},
		{name: "staff global tokens", path: "/api/v2/organizations", user: staff, want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			if test.user.token != "" {
				request.AddCookie(&http.Cookie{Name: hostedCookie, Value: test.user.token})
			}
			if test.bearer != "" {
				request.Header.Set("Authorization", "Bearer "+test.bearer)
			}
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, test.want)
			if test.want == http.StatusOK && (test.path == "/api/cloud/metadata" || test.path == "/api/cloud/billing") {
				var report hostedMetadataResponse
				decodeHubResponse(t, response, &report)
				if report.OrganizationID != "org_security" || report.ProviderOrganizationID != "org_provider" || !report.Healthy || report.PlanID == "" || report.Entitlement.OrganizationID != "org_security" {
					t.Fatalf("tenant metadata = %+v", report)
				}
				if report.Members == nil || *report.Members != 2 || report.Projects == nil || *report.Projects != 1 || report.Runners == nil || *report.Runners != 0 || report.LastActivity == nil || report.LastActivity.IsZero() {
					t.Fatalf("tenant metadata populations = %+v", report)
				}
				if report.CostDrivers.ActiveIssues == nil || *report.CostDrivers.ActiveIssues != 1 || report.CostDrivers.ArchivedIssues == nil || *report.CostDrivers.ArchivedIssues != 0 {
					t.Fatalf("tenant issue populations = %+v", report.CostDrivers)
				}
				if report.DatabaseBytes != nil || report.CostDrivers.DatabaseBytes != nil || report.CostDrivers.WALBytes != nil || !slices.Contains(report.UnavailableUsage, "database_bytes") || !slices.Contains(report.UnavailableUsage, "wal_bytes") {
					t.Fatalf("tenant physical storage must be unavailable: %+v", report)
				}
				if (report.Events == nil) != slices.Contains(report.UnavailableUsage, "events") {
					t.Fatalf("event population availability = %+v", report)
				}
				var fields map[string]json.RawMessage
				decodeHubResponse(t, response, &fields)
				for name := range fields {
					if !slices.Contains([]string{"organization_id", "provider_organization_id", "members", "projects", "runners", "events", "last_activity", "database_bytes", "healthy", "unavailable_usage", "entitlement", "plan_id", "storage_quota_bytes", "event_quota", "cost_drivers"}, name) {
						t.Fatalf("unexpected metadata field %q", name)
					}
				}
				var drivers map[string]json.RawMessage
				if err := json.Unmarshal(fields["cost_drivers"], &drivers); err != nil {
					t.Fatal(err)
				}
				for name := range drivers {
					if !slices.Contains([]string{"active_issues", "archived_issues", "database_bytes", "wal_bytes", "requests", "request_bytes", "response_bytes", "artifact_retained_bytes", "artifact_reserved_bytes", "relay_bytes", "window_ends_at", "operator_ai", "operator_ai_cost_usd"}, name) {
						t.Fatalf("unexpected metadata cost field %q", name)
					}
				}
				for _, field := range []json.RawMessage{fields["database_bytes"], drivers["database_bytes"], drivers["wal_bytes"]} {
					if string(field) != "null" {
						t.Fatalf("unknown physical storage = %s, want null", field)
					}
				}
			}
			for _, sentinel := range []string{"private-project-sentinel", "private-issue-sentinel", "private-body-sentinel", testHubAdminToken} {
				if strings.Contains(response.Body.String(), sentinel) {
					t.Fatalf("response exposed %q", sentinel)
				}
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("hosted response permits caching")
			}
		})
	}
	requireNativeStatus(t, f.request(t, staff, http.MethodPost, "/support/start", url.Values{}), http.StatusForbidden)
}

func TestHostedSecurityRunnerPermissionsAreSeparate(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, role      string
		runner, missing bool
		want            int
	}{
		{name: "owner without runner grant", role: "owner", want: http.StatusOK},
		{name: "admin without runner grant", role: "admin", want: http.StatusOK},
		{name: "owner with grantless project", role: "owner", missing: true, want: http.StatusOK},
		{name: "admin with grantless project", role: "admin", missing: true, want: http.StatusOK},
		{name: "member with runner grant", role: "member", runner: true, want: http.StatusOK},
		{name: "member with grantless project", role: "member", runner: true, missing: true, want: http.StatusNotFound},
		{name: "viewer with runner grant", role: "viewer", runner: true, want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, ctx := newHostedKeyMCPFixture(t, "dedicated", test.role)
			user := f.user
			f.grant(t, user, false, test.runner)
			if test.missing {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: f.service.accessOrganization(), ID: "prj_grantless", Name: "owner/repository", Profile: "github_compatible", CreatedAt: f.service.config.now()}, States: nativeFixtureStates()}); err != nil {
						return err
					}
					return w.(accessstore.ProjectAccessCleaner).DeleteAccessProjectGrants(t.Context(), "prj_grantless")
				}); err != nil {
					t.Fatal(err)
				}
			}
			response := f.request(t, user, http.MethodGet, "/api/v2/organizations/org_security/runners", nil)
			requireNativeStatus(t, response, test.want)
			if test.role == "member" && test.missing && !strings.Contains(response.Body.String(), "owner/repository") {
				t.Fatalf("denial does not name missing project: %s", response.Body.String())
			}
			credential, err := currentHubOperator(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := f.service.appAllRunnerGrants(t.Context(), credential); err != nil || got != (test.want == http.StatusOK) {
				t.Fatalf("Cloud runner controls available=%v", got)
			}
			catalogCtx := f.service.withOperatorCatalog(ctx, credential, "org_security")
			_, catalogErr := f.service.authorizeCatalog(catalogCtx, hubFleetRequirement(operatortool.ListRunnerRouting))
			_, err = (hubFleetExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.ListRunnerRouting, Arguments: json.RawMessage(`{}`)})
			enrollment := runnerauth.EnrollmentRequest{ProjectIDs: []tracker.ProjectID{f.project}, Operations: []string{runnerauth.Read}, TTLSeconds: 900}
			arguments, encodeErr := json.Marshal(hubFleetRequest{RequestID: "role-enrollment", Enrollment: enrollment})
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			_, enrollmentErr := (hubFleetExecutor{f.service}).Execute(ctx, operatortool.Call{Name: operatortool.CreateRunnerEnrollment, Arguments: arguments})
			for _, denial := range []error{catalogErr, err, enrollmentErr} {
				if test.want == http.StatusOK && denial != nil || test.want != http.StatusOK && !errors.Is(denial, operatortool.ErrAccessDenied) {
					t.Fatalf("operator runner access=%v", denial)
				}
				if test.role == "member" && test.missing && !strings.Contains(denial.Error(), "owner/repository") {
					t.Fatalf("operator denial does not name missing project: %v", denial)
				}
			}
			response = f.request(t, user, http.MethodPost, "/api/v2/organizations/org_security/runner-enrollments", enrollment)
			want := test.want
			if want == http.StatusOK {
				want = http.StatusCreated
			}
			requireNativeStatus(t, response, want)
			requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/work-items/wi_unknown/change-requests/change_unknown/versions/version_unknown/checks", map[string]any{}), http.StatusNotFound)
		})
	}
}

func TestHostedSecuritySupportSessionConstraints(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		role   string
		grant  string
		change func(*testing.T, hostedSecurityFixture, hostedSecurityUser)
		path   string
		want   int
	}{
		{name: "effective project read", role: "member", grant: "read", want: http.StatusOK},
		{name: "effective project write", role: "member", grant: "write", want: http.StatusOK},
		{name: "no effective project grant", role: "owner", want: http.StatusNotFound},
		{name: "viewer read", role: "viewer", grant: "write", want: http.StatusOK},
		{name: "wrong organization", role: "member", grant: "write", path: "/api/v2/organizations/org_other/projects/prj_security", want: http.StatusNotFound},
		{name: "expired provider session", role: "member", grant: "write", change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
			f.provider.mu.Lock()
			identity := f.provider.sessions[u.identity.Hosted.SessionID]
			identity.ExpiresAt = time.Now().Add(-time.Minute)
			f.provider.sessions[identity.SessionID] = identity
			f.provider.mu.Unlock()
		}, want: http.StatusUnauthorized},
		{name: "revoked provider session", role: "member", grant: "write", change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
			if err := f.provider.RevokeSession(t.Context(), u.identity.Hosted.SessionID); err != nil {
				t.Fatal(err)
			}
		}, want: http.StatusUnauthorized},
		{name: "removed membership", role: "member", grant: "write", change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
			if err := f.provider.RevokeMembership(t.Context(), "membership_"+u.identity.Subject); err != nil {
				t.Fatal(err)
			}
		}, want: http.StatusForbidden},
		{name: "revoked local session", role: "member", grant: "write", change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
				return w.RevokeAccessSession(t.Context(), apikey.HashToken(u.token), f.service.config.now().UTC())
			}); err != nil {
				t.Fatal(err)
			}
		}, want: http.StatusUnauthorized},
		{name: "changed support actor", role: "member", grant: "write", change: func(t *testing.T, f hostedSecurityFixture, u hostedSecurityUser) {
			f.provider.mu.Lock()
			identity := f.provider.sessions[u.identity.Hosted.SessionID]
			identity.SupportActor = "staff@example.test"
			f.provider.sessions[identity.SessionID] = identity
			f.provider.mu.Unlock()
		}, want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			user := f.user(t, "customer", test.role, "customer@example.test", test.grant, "support@example.test")
			if test.grant == "" {
				f.withoutProjectAccess(t, user)
			}
			if test.change != nil {
				test.change(t, f, user)
			}
			path := test.path
			if path == "" {
				path = f.base
			}
			requireNativeStatus(t, f.request(t, user, http.MethodGet, path, nil), test.want)
			if test.want == http.StatusOK {
				command := tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "support-write"}, Title: "private-body-sentinel", State: "Todo"}
				wantWrite := http.StatusForbidden
				if test.role != "viewer" && test.grant == "write" {
					wantWrite = http.StatusOK
				}
				requireNativeStatus(t, f.request(t, user, http.MethodPost, f.base+"/work-items", command), wantWrite)
				requireNativeStatus(t, f.request(t, user, http.MethodGet, "/api/cloud/metadata", nil), http.StatusForbidden)
				if wantWrite != http.StatusOK {
					var count int
					if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND session_id=$2", f.service.accessOrganization(), user.identity.Hosted.SessionID).Scan(&count); err != nil || count != 1 {
						t.Fatalf("support read audit rows=%d, want 1: %v", count, err)
					}
				}
				var auditID int64
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT id FROM hosted_audit WHERE organization_id=$1 AND session_id=$2 ORDER BY id DESC LIMIT 1", f.service.accessOrganization(), user.identity.Hosted.SessionID).Scan(&auditID); err != nil {
					t.Fatal(err)
				}
				var audit accessstore.Audit
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					audit, err = r.AccessAudit(t.Context(), auditID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				actor, effective, organization, reason, raw := audit.ActualActor, audit.EffectiveUser, string(audit.OrganizationID), audit.Reason, audit.Event+audit.Route+string(audit.ProjectID)
				if actor != "support@example.test" || effective != user.identity.Subject || organization != "org_security" || reason != "customer-request" || strings.Contains(raw, "private-body-sentinel") || strings.Contains(raw, user.token) {
					t.Fatalf("audit actor=%q effective=%q organization=%q reason=%q", actor, effective, organization, reason)
				}
			}
		})
	}
}

func TestHostedSecurityReplayAndCursorRevocation(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	writer := f.user(t, "writer", "member", "writer@example.test", "write", "")
	reader := f.user(t, "reader", "viewer", "reader@example.test", "read", "")
	f.seedIssue(t, 1)
	f.seedIssue(t, 2)
	command := tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "cached-command"}, Title: "Receipt reconnect", State: "Todo"}
	created := f.request(t, writer, http.MethodPost, f.base+"/work-items", command)
	requireNativeStatus(t, created, http.StatusOK)
	// Simulate a pre-upgrade receipt: its session suffix must not let a
	// reconnect create a duplicate or evade changed-payload conflicts.
	var original eventstore.Receipt
	operation := http.MethodPost + " " + f.base + "/work-items"
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		member, err := r.AccessMember(t.Context(), writer.identity.Subject)
		if err != nil {
			return err
		}
		original, err = r.Receipt(t.Context(), member.PrincipalID, operation, command.IdempotencyKey)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tx, err := hostedPostgresAdmin(t, f.service).BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "SET LOCAL session_replication_role=replica"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"native_commands", "native_command_states"} {
		result, err := tx.ExecContext(t.Context(), "UPDATE "+table+" SET operation=operation || ' ' || $1 WHERE organization_id=$2 AND actor_id=$3 AND operation=$4 AND command_key=$5", writer.identity.Hosted.SessionID, original.OrganizationID, original.ActorID, operation, command.IdempotencyKey)
		if err != nil {
			t.Fatal(err)
		}
		if count, err := result.RowsAffected(); err != nil || count != 1 {
			t.Fatalf("legacy receipt %s rows=%d error=%v", table, count, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
		receipt, err := r.ReplayReceipt(t.Context(), original.ActorID, operation, command.IdempotencyKey)
		if err == nil && (receipt.Operation != operation+" "+writer.identity.Hosted.SessionID || receipt.RequestHash != original.RequestHash || receipt.PayloadSHA256 != original.PayloadSHA256) {
			return errors.New("legacy receipt fixture lost immutable identity")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.maintainBoardRecords(t.Context()); err != nil {
		t.Fatalf("refresh board projection: %v", err)
	}
	response := f.request(t, writer, http.MethodGet, f.base+"/work-items?limit=1", nil)
	requireNativeStatus(t, response, http.StatusOK)
	var page tracker.Page[tracker.NativeIssue]
	decodeHubResponse(t, response, &page)
	if page.NextCursor == "" {
		t.Fatal("missing cursor")
	}
	requireNativeStatus(t, f.request(t, reader, http.MethodGet, f.base+"/work-items?limit=1&cursor="+url.QueryEscape(page.NextCursor), nil), http.StatusUnprocessableEntity)
	secondIdentity := writer.identity
	secondHosted := *writer.identity.Hosted
	secondHosted.SessionID = "session_writer_second"
	secondIdentity.Hosted = &secondHosted
	f.provider.mu.Lock()
	f.provider.sessions[secondHosted.SessionID] = secondHosted
	f.provider.mu.Unlock()
	secondToken, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), secondIdentity)
	if err != nil {
		t.Fatal(err)
	}
	secondSession := hostedSecurityUser{identity: secondIdentity, token: secondToken}
	replay := f.request(t, secondSession, http.MethodPost, f.base+"/work-items", command)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != created.Body.String() {
		t.Fatal("reconnect did not replay legacy receipt")
	}
	changed := command
	changed.Title = "changed"
	requireNativeStatus(t, f.request(t, secondSession, http.MethodPost, f.base+"/work-items", changed), http.StatusConflict)
	other := f.user(t, "other-writer", "member", "other-writer@example.test", "write", "")
	distinct := createIndependentNativeIssue(t, command, func(request tracker.CreateIssue) *httptest.ResponseRecorder {
		return f.request(t, other, http.MethodPost, f.base+"/work-items", request)
	})
	requireNativeStatus(t, distinct, http.StatusOK)
	var first, second tracker.NativeIssue
	decodeHubResponse(t, created, &first)
	decodeHubResponse(t, distinct, &second)
	if first.WorkItemID == second.WorkItemID {
		t.Fatal("distinct actors shared a receipt")
	}

	requireNativeStatus(t, f.request(t, secondSession, http.MethodGet, f.base+"/work-items?limit=1&cursor="+url.QueryEscape(page.NextCursor), nil), http.StatusUnprocessableEntity)
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), writer.identity.Subject, f.project)
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path string
		body         any
	}{
		{method: http.MethodPost, path: f.base + "/work-items", body: command},
		{method: http.MethodGet, path: f.base + "/work-items?limit=1&cursor=" + url.QueryEscape(page.NextCursor)},
	} {
		response := f.request(t, writer, test.method, test.path, test.body)
		requireNativeStatus(t, response, http.StatusNotFound)
		if strings.Contains(response.Body.String(), "cached-private-sentinel") {
			t.Fatal("revoked grant exposed cached response")
		}
	}
}

func TestHostedSecurityCookieMutationsRequireCSRF(t *testing.T) {
	t.Parallel()
	for _, authorization := range []string{"", "Bearer invalid"} {
		t.Run(authorization, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			user := f.user(t, "owner", "owner", "owner@example.test", "", "")
			request := httptest.NewRequest(http.MethodPost, "/projects", strings.NewReader(url.Values{"name": {"Rejected"}, "grant_access": {"true"}}.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Authorization", authorization)
			request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
			response := httptest.NewRecorder()
			f.service.Handler().ServeHTTP(response, request)
			requireNativeStatus(t, response, http.StatusForbidden)
		})
	}
}

func TestHostedSecurityAdministrationRejectsBearerCookieMix(t *testing.T) {
	t.Parallel()
	for _, authorization := range []string{"Bearer invalid", "Bearer " + testHubAdminToken} {
		for _, route := range []struct {
			method string
			path   string
			body   string
		}{
			{method: http.MethodPost, path: "/api/v2/organizations/org_security/projects", body: `{"idempotency_key":"mixed-auth","name":"Unauthorized project","states":[{"name":"Todo","dispatchable":true,"transitions":["Done"]},{"name":"Done","terminal":true,"transitions":["Todo"]}]}`},
			{method: http.MethodPost, path: "/api/v2/organizations/org_security/runner-enrollments", body: `{}`},
			{method: http.MethodGet, path: "/api/v2/organizations/org_security/runners"},
			{method: http.MethodPut, path: "/api/v2/organizations/org_security/runners/runner_unknown/routing", body: `{}`},
		} {
			t.Run(authorization+"/"+route.method+route.path, func(t *testing.T) {
				t.Parallel()
				f := newHostedSecurityFixture(t)
				user := f.user(t, "owner", "owner", "owner@example.test", "write", "")
				f.grant(t, user, true, true)
				request := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", authorization)
				request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				requireNativeStatus(t, response, http.StatusNotFound)
				var count int
				if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM projects WHERE organization_id=$1", f.service.accessOrganization()).Scan(&count); err != nil || count != 1 {
					t.Fatalf("mixed authentication changed projects: count=%d, error=%v", count, err)
				}
			})
		}
	}
}

func TestHostedSecurityReplacementMembershipRequiresNewGrants(t *testing.T) {
	t.Parallel()
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%v", replacement), func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			user := f.user(t, "member", "member", "member@example.test", "write", "")
			f.grant(t, user, true, true)
			f.provider.mu.Lock()
			membership := f.provider.members["membership_"+user.identity.Subject]
			if replacement {
				delete(f.provider.members, membership.ID)
				membership.ID += "_replacement"
				f.provider.members[membership.ID] = membership
			}
			f.provider.mu.Unlock()
			if err := f.service.addHostedMember(t.Context(), user.identity, membership); err != nil {
				t.Fatal(err)
			}
			want := http.StatusOK
			if replacement {
				want = http.StatusNotFound
			}
			requireNativeStatus(t, f.request(t, user, http.MethodGet, f.base, nil), want)
			requireNativeStatus(t, f.request(t, user, http.MethodGet, "/api/v2/organizations/org_security/runners", nil), want)
			if replacement {
				f.grant(t, user, false, false)
				requireNativeStatus(t, f.request(t, user, http.MethodGet, f.base, nil), http.StatusOK)
				requireNativeStatus(t, f.request(t, user, http.MethodGet, "/api/v2/organizations/org_security/runners", nil), http.StatusNotFound)
			}
		})
	}
}

func TestHostedSecuritySSEAudit(t *testing.T) {
	t.Parallel()
	for _, actor := range []string{"", "support@example.test"} {
		t.Run(actor, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			f.seedIssue(t, 1)
			user := f.user(t, "viewer", "viewer", "viewer@example.test", "read", actor)
			before := dashboardPostgresHead(t, f)
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
				return dashboardPostgresLane(t.Context(), w, f.project)
			}); err != nil {
				t.Fatal(err)
			}
			reader := openHostedTestStream(t, f, user, "?since="+strconv.FormatInt(before, 10))
			kind, _ := readHostedEvent(t, reader)
			if kind != "lane" {
				t.Fatalf("initial event = %q", kind)
			}
			var count, attributed int
			err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*),count(*) FILTER(WHERE actual_actor=$1 AND effective_user=$2 AND reason=$3) FROM hosted_audit WHERE session_id=$4 AND route=$5 AND organization_id=$6", actor, user.identity.Subject, user.identity.Hosted.SupportReason, user.identity.Hosted.SessionID, "GET /projects/:project/events", f.service.accessOrganization()).Scan(&count, &attributed)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if actor != "" {
				want = 1
			}
			if count != want || attributed != want {
				t.Fatalf("stream read audit rows=%d attributed=%d, want %d", count, attributed, want)
			}
		})
	}
}

type hostedEventRecorder struct {
	*httptest.ResponseRecorder
	flush func()
}

func (r hostedEventRecorder) Flush() {
	r.ResponseRecorder.Flush()
	r.flush()
}

func TestHostedSecuritySSERevocation(t *testing.T) {
	t.Parallel()
	for _, revocation := range []string{"provider session", "membership", "project grant", "shutdown"} {
		t.Run(revocation, func(t *testing.T) {
			t.Parallel()
			clock := &leaseTestClock{value: time.Now().UTC()}
			f, _, delivery := controlledHostedDelivery(t, dashboardDeliveryConfig{}, func(cfg *Config) {
				cfg.now = clock.Now
			})
			owner := f.user(t, "member", "member", "member@example.test", "write", "")
			user := f.user(t, "viewer", "viewer", "viewer@example.test", "read", "")
			item := f.seedIssue(t, 1)
			before := dashboardPostgresHead(t, f)
			created := f.request(t, owner, http.MethodPost, f.base+"/workspaces", workspaceRequest{
				Mutation: tracker.Mutation{IdempotencyKey: "revocation-workspace"}, WorkItemID: string(item), Requires: []string{"files"},
			})
			requireNativeStatus(t, created, http.StatusCreated)
			var workspace workspacesession.Session
			decodeHubResponse(t, created, &workspace)
			if revocation == "shutdown" {
				delivery.start()
				ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
				defer cancel()
				request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/projects/"+string(f.project)+"/events?workspace="+workspace.ID, nil)
				request.AddCookie(&http.Cookie{Name: hostedCookie, Value: user.token})
				var shutdownErr error
				frames := 0
				response := hostedEventRecorder{ResponseRecorder: httptest.NewRecorder(), flush: func() {
					frames++
					if frames == 1 {
						shutdownErr = f.service.Shutdown(ctx)
					} else {
						cancel()
					}
				}}
				f.service.Handler().ServeHTTP(response, request)
				if response.Code != http.StatusOK || frames != 1 || shutdownErr != nil || ctx.Err() != nil {
					t.Fatalf("shutdown stream: status=%d frames=%d shutdown=%v context=%v", response.Code, frames, shutdownErr, ctx.Err())
				}
				return
			}
			reader := openHostedTestStream(t, f, user, "?workspace="+workspace.ID+"&since="+strconv.FormatInt(before, 10))
			pumpHostedDelivery(t, delivery)
			kind := ""
			for kind != "workspace.requested" {
				kind, _ = readHostedEvent(t, reader)
			}
			if kind != "workspace.requested" {
				t.Fatalf("workspace event = %q", kind)
			}
			switch revocation {
			case "provider session":
				if err := f.provider.RevokeSession(t.Context(), user.identity.Hosted.SessionID); err != nil {
					t.Fatal(err)
				}
			case "membership":
				if err := f.provider.RevokeMembership(t.Context(), "membership_"+user.identity.Subject); err != nil {
					t.Fatal(err)
				}
			case "project grant":
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
					return w.DeleteAccessProjectGrant(t.Context(), user.identity.Subject, f.project)
				}); err != nil {
					t.Fatal(err)
				}
			}
			clock.Advance(21 * time.Second)
			pumpHostedDelivery(t, delivery)
			frame, err := readSSEFrame(reader)
			for err == nil && frame.Event == "checkpoint" {
				frame, err = readSSEFrame(reader)
			}
			if err != nil || frame.Event != "reset" {
				t.Fatalf("revocation reset=%+v: %v", frame, err)
			}
			frame, err = readSSEFrame(reader)
			if !errors.Is(err, io.EOF) {
				t.Fatalf("revoked stream produced %+v, error=%v", frame, err)
			}
		})
	}
}

func TestHostedSecuritySSEDeniesUnscopedProject(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "viewer", "viewer", "viewer@example.test", "", "")
	response := f.request(t, user, http.MethodGet, "/projects/"+string(f.project)+"/events", nil)
	if response.Code != http.StatusForbidden && response.Code != http.StatusNotFound {
		t.Fatalf("ungranted project stream returned %d", response.Code)
	}
	if strings.Contains(response.Body.String(), "event:") {
		t.Fatal("ungranted project emitted an event")
	}
}

func TestHostedSecurityLoginTransactions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, start, email string
		wrongState         bool
		want               int
	}{
		{name: "customer provider organization", start: "/auth/oidc/start", email: "owner@example.test", want: http.StatusSeeOther},
		{name: "staff without organization", start: "/auth/oidc/start?staff=1", email: "staff@example.test", want: http.StatusSeeOther},
		{name: "wrong callback state", start: "/auth/oidc/start", email: "owner@example.test", wrongState: true, want: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			user := f.user(t, "owner", "owner", test.email, "", "")
			f.provider.mu.Lock()
			f.provider.codes["code"] = user.identity
			f.provider.mu.Unlock()
			start := f.request(t, hostedSecurityUser{}, http.MethodGet, test.start, nil)
			requireNativeStatus(t, start, http.StatusSeeOther)
			location, err := url.Parse(start.Header().Get("Location"))
			if err != nil {
				t.Fatal(err)
			}
			state := location.Query().Get("state")
			if state == "" {
				t.Fatal("missing login state")
			}
			if test.wrongState {
				state = "wrong-state"
			}
			var cookie *http.Cookie
			for _, current := range start.Result().Cookies() {
				if current.Name == hostedTransactionCookie {
					cookie = current
				}
			}
			if cookie == nil {
				t.Fatal("missing transaction cookie")
			}
			path := "/auth/oidc/callback?" + url.Values{"state": {state}, "code": {"code"}}.Encode()
			for attempt := range 2 {
				request := httptest.NewRequest(http.MethodGet, path, nil)
				request.AddCookie(cookie)
				response := httptest.NewRecorder()
				f.service.Handler().ServeHTTP(response, request)
				want := test.want
				if attempt == 1 {
					want = http.StatusUnauthorized
				}
				requireNativeStatus(t, response, want)
			}
			f.provider.mu.Lock()
			exchanges := f.provider.exchanges
			f.provider.mu.Unlock()
			wantExchanges := 1
			if test.wrongState {
				wantExchanges = 0
			}
			if exchanges != wantExchanges {
				t.Fatalf("provider exchanges = %d, want %d", exchanges, wantExchanges)
			}
		})
	}
}

var _ auth.HostedProvider = (*hostedSecurityProvider)(nil)

func TestHostedSecurityRoleRevocationAppliesImmediately(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	user := f.user(t, "member", "member", "member@example.test", "write", "")
	command := tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "before-role-change"}, Title: "private-mutation-sentinel", State: "Todo"}
	response := f.request(t, user, http.MethodPost, f.base+"/work-items", command)
	requireNativeStatus(t, response, http.StatusOK)
	replay := f.request(t, user, http.MethodPost, f.base+"/work-items", command)
	requireNativeStatus(t, replay, http.StatusOK)
	if response.Body.String() != replay.Body.String() {
		t.Fatal("idempotent replay changed the mutation response")
	}
	if err := f.provider.SetMembershipRole(t.Context(), "membership_"+user.identity.Subject, "viewer"); err != nil {
		t.Fatal(err)
	}
	requireNativeStatus(t, f.request(t, user, http.MethodGet, f.base, nil), http.StatusOK)
	response = f.request(t, user, http.MethodPost, f.base+"/work-items", command)
	requireNativeStatus(t, response, http.StatusForbidden)
	if strings.Contains(response.Body.String(), "private-mutation-sentinel") {
		t.Fatal("role downgrade exposed cached mutation")
	}
}

func TestHostedSecuritySupportLoginAndExit(t *testing.T) {
	t.Parallel()
	f := newHostedSecurityFixture(t)
	staff := f.user(t, "support", "owner", "support@example.test", "", "")
	customer := f.user(t, "customer", "viewer", "customer@example.test", "read", "support@example.test")
	start := f.request(t, staff, http.MethodPost, "/support/start", url.Values{})
	requireNativeStatus(t, start, http.StatusOK)
	var transaction *http.Cookie
	for _, cookie := range start.Result().Cookies() {
		if cookie.Name == hostedTransactionCookie {
			transaction = cookie
		}
	}
	if transaction == nil {
		t.Fatal("support login transaction cookie was not set")
	}
	f.provider.mu.Lock()
	f.provider.codes["support-code"] = customer.identity
	f.provider.mu.Unlock()
	request := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback?code=support-code", nil)
	request.AddCookie(transaction)
	request.AddCookie(&http.Cookie{Name: hostedCookie, Value: staff.token})
	callback := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(callback, request)
	requireNativeStatus(t, callback, http.StatusSeeOther)
	var sessionCookie *http.Cookie
	for _, cookie := range callback.Result().Cookies() {
		if cookie.Name == hostedCookie {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil {
		t.Fatal("support session cookie was not set")
	}
	support := hostedSecurityUser{identity: customer.identity, token: sessionCookie.Value}
	page := f.request(t, support, http.MethodGet, "/organization", nil)
	requireNativeStatus(t, page, http.StatusOK)
	if !strings.Contains(page.Body.String(), "support@example.test") || !strings.Contains(page.Body.String(), "/logout") {
		t.Fatal("support indicator or exit flow is missing")
	}
	requireNativeStatus(t, f.request(t, support, http.MethodPost, "/logout", url.Values{}), http.StatusSeeOther)
	requireNativeStatus(t, f.request(t, support, http.MethodGet, f.base, nil), http.StatusUnauthorized)
	if _, err := f.provider.CurrentSession(t.Context(), *customer.identity.Hosted); !errors.Is(err, auth.ErrHostedIdentity) {
		t.Fatalf("provider support session was not revoked: %v", err)
	}
	var starts, ends int
	err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FILTER(WHERE event='session_started'),count(*) FILTER(WHERE event='session_ended') FROM hosted_audit WHERE session_id=$1 AND actual_actor=$2 AND effective_user=$3 AND organization_id=$4", customer.identity.Hosted.SessionID, "support@example.test", customer.identity.Subject, f.service.accessOrganization()).Scan(&starts, &ends)
	if err != nil || starts != 1 || ends != 1 {
		t.Fatalf("support lifecycle audit starts=%d ends=%d error=%v", starts, ends, err)
	}
}

func TestHostedSecurityLogsExcludeCustomerContent(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	provider := newHostedSecurityProvider()
	service := openHostedPostgresService(t, Config{
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Hosted: &hostedplans.HostedConfig{OrganizationID: "org_logs", WorkOSOrganizationID: "org_provider", PublicURL: "https://logs.example.test", Provider: provider},
	})
	const sentinel = "private-content-bearer-sentinel"
	service.echo.GET("/security/error", func(c echo.Context) error {
		failure := fmt.Errorf("load work item: token=%s: %w", sentinel, errors.New("bearer "+sentinel))
		service.config.Logger.Error("security probe failed", "title", sentinel, "body", sentinel, "api_token", sentinel, "error", failure)
		return service.nativeAPIError(c, failure)
	})
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/security/error", nil))
	requireNativeStatus(t, response, http.StatusInternalServerError)
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if logs.Len() == 0 {
		t.Fatal("test produced no log records")
	}
	if !strings.Contains(logs.String(), "security probe failed") || !strings.Contains(logs.String(), "load work item") {
		t.Fatalf("hosted logs dropped the error diagnostics: %s", logs.String())
	}
	if strings.Contains(logs.String(), sentinel) || strings.Contains(response.Body.String(), sentinel) {
		t.Fatal("hosted logs or errors exposed customer content")
	}
}

func TestHostedProjectCreationRechecksTheCreatorsRole(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		role    string
		created bool
	}{
		{role: "owner", created: true},
		{role: "admin", created: true},
		{role: "member", created: false},
	} {
		t.Run(test.role, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			user := f.user(t, "creator-"+test.role, test.role, test.role+"-creator@example.test", "write", "")
			f.grant(t, user, true, true)
			for _, role := range []string{"owner", "admin", "member", "viewer"} {
				f.user(t, "other-"+role, role, role+"-other@example.test", "write", "")
			}
			inactive := f.user(t, "inactive-admin", "admin", "inactive@example.test", "write", "")
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), inactive.identity.Subject)
				if err != nil {
					return err
				}
				member.Active, member.UpdatedAt = false, f.service.config.now().UTC()
				return w.UpdateAccessMember(t.Context(), member)
			}); err != nil {
				t.Fatal(err)
			}
			// The request authenticated while the user was an owner; the
			// provider's membership is what the transaction must trust.
			var principal, principalHash, membership string
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
				member, err := r.AccessMember(t.Context(), user.identity.Subject)
				if err != nil {
					return err
				}
				principal, membership = member.PrincipalID, member.MembershipID
				stored, err := r.AccessCredential(t.Context(), member.PrincipalID)
				principalHash = stored.Hash
				return err
			}); err != nil {
				t.Fatal(err)
			}
			credential := hubapi.APICredential{ID: principal, Hash: principalHash, HostedPrincipal: principal, HostedMembership: membership, Scope: hubapi.APIScopeOperator, NativeOnly: true, Hosted: user.identity.Hosted, SessionHash: apikey.HashToken(user.token), HostedRole: "owner", ManageRunners: true}
			project, err := f.service.createHostedProjectRecord(t.Context(), credential, "Project for "+test.role, nil)
			if (err == nil) != test.created {
				t.Fatalf("createHostedProjectRecord() = %q, %v; want created %v", project, err, test.created)
			}
			if !test.created {
				return
			}
			var manageRunner bool
			err = f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
				grant, err := r.AccessProjectGrant(t.Context(), user.identity.Subject, tracker.ProjectID(project))
				manageRunner = grant.ManageRunner
				return err
			})
			if err != nil || !manageRunner {
				t.Fatalf("creator runner management = %v, %v", manageRunner, err)
			}
			for _, role := range []string{"owner", "admin", "member", "viewer"} {
				var grants int
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
					member, err := r.AccessMember(t.Context(), "user_other-"+role)
					if err != nil {
						return err
					}
					grant, err := r.AccessProjectGrant(t.Context(), member.UserID, tracker.ProjectID(project))
					if errors.Is(err, corestore.ErrNotFound) {
						return nil
					}
					if err != nil {
						return err
					}
					_, err = r.AccessTokenGrant(t.Context(), member.PrincipalID, tracker.ProjectID(project))
					if errors.Is(err, corestore.ErrNotFound) {
						return nil
					}
					if err == nil && grant.CanWrite && grant.ManageRunner {
						grants = 1
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				want := 0
				if role == "owner" || role == "admin" {
					want = 1
				}
				if grants != want {
					t.Fatalf("%s grants = %d, want %d", role, grants, want)
				}
			}
			var inactiveGrants, audits int
			err = f.service.config.TenantRepository.ReadTenant(t.Context(), f.service.accessOrganization(), corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.AccessProjectGrant(t.Context(), inactive.identity.Subject, tracker.ProjectID(project))
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				if err == nil {
					inactiveGrants = 1
				}
				return err
			})
			if err != nil || inactiveGrants != 0 {
				t.Fatalf("inactive grants = %d, %v", inactiveGrants, err)
			}
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND project_id=$2 AND actual_actor=$3 AND event='project_created'", f.service.accessOrganization(), project, user.identity.Subject).Scan(&audits); err != nil || audits != 1 {
				t.Fatalf("creation audits = %d, %v", audits, err)
			}
		})
	}
}

func (*hostedSecurityProvider) HasUser(context.Context, string) (bool, error) { return true, nil }

func TestHostedRepeatedReadsAreNotAudited(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		support string
	}{
		{name: "member reads"},
		{name: "support reads", support: "support@example.test"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			user := f.user(t, "customer", "member", "customer@example.test", "read", test.support)
			for range 3 {
				requireNativeStatus(t, f.request(t, user, http.MethodGet, f.base, nil), http.StatusOK)
			}
			var count, attributed int
			if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*),count(*) FILTER(WHERE actual_actor=$1 AND effective_user=$2 AND reason=$3) FROM hosted_audit WHERE session_id=$4 AND event='action' AND route LIKE 'GET %' AND organization_id=$5", test.support, user.identity.Subject, user.identity.Hosted.SupportReason, user.identity.Hosted.SessionID, f.service.accessOrganization()).Scan(&count, &attributed); err != nil {
				t.Fatal(err)
			}
			want := 0
			if test.support != "" {
				want = 3
			}
			if count != want || attributed != want {
				t.Fatalf("read audit rows=%d attributed=%d, want %d", count, attributed, want)
			}
		})
	}
}
