package hubserver

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresMemberMutationRoutesPreserveCurrentAuthority(t *testing.T) {
	repository := projectRecordPostgres(t)
	provider := newHostedSecurityProvider()
	details := &memoryRunDetails{}
	plans := pilotHostedPlans()
	service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{postgres: repository, billingRepository: repository, billingAuditStore: details, hostedOrganization: "org_security", now: time.Now, logger: discardLogger()}, config: Config{Logger: discardLogger(), now: time.Now, generateToken: apikey.GenerateToken, AccessRepository: repository, TenantRepository: repository, RunDetails: details, Hosted: &HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", PublicURL: "http://127.0.0.1:7777", Provider: provider, Plans: &plans}}}
	sessions, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: 24 * time.Hour, PublicURL: service.config.Hosted.PublicURL}, service)
	if err != nil {
		t.Fatal(err)
	}
	service.hostedSessions = sessions
	service.registerRoutes(service.echo)
	f := hostedSecurityFixture{service: service, provider: provider}
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"}); err != nil {
			return err
		}
		for _, id := range []tracker.ProjectID{"prj_first", "prj_second"} {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: id, Name: string(id), Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.database.configureHostedPlans(t.Context(), service.config.Hosted); err != nil {
		t.Fatal(err)
	}
	owner := f.user(t, "member_operator", "owner", "operator@example.test", "", "")
	viewer := f.user(t, "member_viewer", "viewer", "viewer@example.test", "", "")
	users := []hostedSecurityUser{
		f.user(t, "member_first", "member", "first@example.test", "", ""),
		f.user(t, "member_second", "member", "second@example.test", "", ""),
	}
	readMember := func(user hostedSecurityUser) accessstore.Member {
		t.Helper()
		var member accessstore.Member
		if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			member, err = r.AccessMember(t.Context(), user.identity.Subject)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return member
	}
	if service.database.db != nil || service.database.reader != nil || service.database.authReader != nil || service.database.claimReader != nil {
		t.Fatal("member fixture retained a legacy database handle")
	}
	base := "/api/v2/organizations/org_security/members"
	for _, user := range users {
		member := readMember(user)
		for _, request := range []struct {
			method, suffix string
			body           any
		}{
			{http.MethodPost, "/invitations", map[string]any{"email": user.identity.Email, "role": "member", "idempotency_key": "denied"}},
			{http.MethodPut, "/" + member.MembershipID + "/role", map[string]any{"role": "admin"}},
			{http.MethodDelete, "/" + member.MembershipID, nil},
		} {
			response := f.request(t, viewer, request.method, base+request.suffix, request.body)
			requireNativeStatus(t, response, http.StatusForbidden)
		}
		service.config.AccessRepository = memberScopedReadRepository{Repository: repository, user: user.identity.Subject}
		response := f.request(t, owner, http.MethodPut, base+"/"+member.MembershipID+"/grants", map[string]any{"project_id": "prj_first", "write": true, "runner": true})
		requireNativeStatus(t, response, http.StatusOK)
		response = f.request(t, owner, http.MethodPut, base+"/"+member.MembershipID+"/role", map[string]any{"role": "viewer"})
		requireNativeStatus(t, response, http.StatusOK)
		var view hostedMemberView
		decodeHubResponse(t, response, &view)
		if view.Role != "viewer" || readMember(user).Role != "viewer" {
			t.Fatal("role response or durable current role was not updated")
		}
		service.config.AccessRepository = repository
		if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
			grant, err := r.AccessProjectGrant(t.Context(), user.identity.Subject, "prj_first")
			if err == nil && (!grant.CanWrite || !grant.ManageRunner) {
				t.Error("role change replaced project grants")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index, address := range []string{"invited-first@example.test", "invited-second@example.test"} {
		request := map[string]any{"email": address, "role": "member", "idempotency_key": address, "grants": []hostedMemberGrant{{ProjectID: "prj_first", Write: true}}}
		response := f.request(t, owner, http.MethodPost, base+"/invitations", request)
		requireNativeStatus(t, response, http.StatusCreated)
		var created, replayed hostedInvitationView
		decodeHubResponse(t, response, &created)
		response = f.request(t, owner, http.MethodPost, base+"/invitations", request)
		requireNativeStatus(t, response, http.StatusCreated)
		decodeHubResponse(t, response, &replayed)
		if created.ID == "" || replayed.ID != created.ID {
			t.Fatal("invitation replay created a second provider invitation")
		}
		provider.mu.Lock()
		count := len(provider.invitations)
		provider.mu.Unlock()
		if count != index+1 {
			t.Fatalf("provider invitations=%d, want %d", count, index+1)
		}
		response = f.request(t, owner, http.MethodPut, base+"/invitations/"+created.ID, map[string]any{"grants": []hostedMemberGrant{{ProjectID: "prj_second"}}})
		requireNativeStatus(t, response, http.StatusNoContent)
		response = f.request(t, owner, http.MethodPost, base+"/invitations/"+created.ID+"/resend", map[string]any{})
		requireNativeStatus(t, response, http.StatusNoContent)
		response = f.request(t, owner, http.MethodDelete, base+"/invitations/"+created.ID, nil)
		requireNativeStatus(t, response, http.StatusNoContent)
	}
	ownerMember := readMember(owner)
	requireNativeStatus(t, f.request(t, owner, http.MethodPut, base+"/"+ownerMember.MembershipID+"/role", map[string]string{"role": "member"}), http.StatusForbidden)
	requireNativeStatus(t, f.request(t, owner, http.MethodDelete, base+"/"+ownerMember.MembershipID, nil), http.StatusForbidden)
	for _, user := range users {
		member := readMember(user)
		requireNativeStatus(t, f.request(t, owner, http.MethodDelete, base+"/"+member.MembershipID, nil), http.StatusNoContent)
		if readMember(user).Active {
			t.Fatal("removed member retained current access")
		}
	}
	target := f.user(t, "member_post_provider", "member", "post-provider@example.test", "", "")
	targetMember := readMember(target)
	service.config.Hosted.Provider = memberRoleHookProvider{HostedProvider: provider, afterRole: func() {
		if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessMember(t.Context(), owner.identity.Subject, now.Add(time.Second))
		}); err != nil {
			t.Fatal(err)
		}
	}}
	uncertain := f.request(t, owner, http.MethodPut, base+"/"+targetMember.MembershipID+"/role", map[string]string{"role": "viewer"})
	requireNativeStatus(t, uncertain, http.StatusConflict)
	var failure apiErrorResponse
	decodeHubResponse(t, uncertain, &failure)
	if failure.Code != "idempotency_in_progress" || failure.Message != mutation.ErrUncertain.Error() {
		t.Fatalf("post-provider uncertainty = %+v", failure)
	}
	if readMember(target).Role != "member" {
		t.Fatal("revoked actor changed the durable target role")
	}
	provider.mu.Lock()
	providerRole := provider.members[targetMember.MembershipID].Role.Slug
	provider.mu.Unlock()
	if providerRole != "viewer" {
		t.Fatal("uncertain response did not follow committed provider mutation")
	}
	service.config.Hosted.Provider = provider
	response := f.request(t, owner, http.MethodPost, base+"/invitations", map[string]any{"email": "after-revocation@example.test", "role": "member", "idempotency_key": "revoked"})
	requireNativeStatus(t, response, http.StatusForbidden)
}

type memberScopedReadRepository struct {
	tenantstore.Repository
	user string
}

func (r memberScopedReadRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error { return read(memberScopedReader{Reader: reader, user: r.user}) })
}

type memberScopedReader struct {
	tenantstore.Reader
	user string
}

func (r memberScopedReader) AccessMembers(context.Context, string, int) ([]accessstore.Member, error) {
	return nil, errors.New("single member mutation read the full organization")
}

func (r memberScopedReader) AccessProjectGrants(ctx context.Context, user string, after tracker.ProjectID, limit int) ([]accessstore.ProjectGrant, error) {
	if user != r.user {
		return nil, errors.New("single member mutation read another member's grants")
	}
	return r.Reader.AccessProjectGrants(ctx, user, after, limit)
}

type memberRoleHookProvider struct {
	auth.HostedProvider
	afterRole func()
}

func (p memberRoleHookProvider) SetMembershipRole(ctx context.Context, id, role string) error {
	if err := p.HostedProvider.SetMembershipRole(ctx, id, role); err != nil {
		return err
	}
	p.afterRole()
	return nil
}
