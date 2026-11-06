package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
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

func TestPostgresHostedEntitlementRoutesUseCurrentTenant(t *testing.T) {
	repository := projectRecordPostgres(t)
	details := &memoryRunDetails{}
	now := time.Date(2026, time.November, 4, 0, 0, 0, 0, time.UTC)
	organizations := []string{"org_first", "org_second"}
	services := make(map[string]*Service, len(organizations))
	tokens := map[string]string{"org_first": strings.Repeat("a", 32), "org_second": strings.Repeat("b", 32)}

	for _, organization := range organizations {
		plans := pilotHostedPlans()
		database := &database{postgres: repository, billingRepository: repository, billingAuditStore: details, hostedOrganization: tracker.OrganizationID(organization), now: func() time.Time { return now }, logger: discardLogger()}
		hosted := &HostedConfig{OrganizationID: organization, EntitlementAdministrator: "operator_example", EntitlementAdminToken: []byte(tokens[organization]), Plans: &plans}
		service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: database, config: Config{Logger: discardLogger(), now: func() time.Time { return now }, RunDetails: details, Hosted: hosted}}
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: tracker.OrganizationID(organization), Name: "Example organization", CreatedAt: now}); err != nil {
				return err
			}
			return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: tracker.OrganizationID(organization), ProviderID: "provider_" + organization})
		}); err != nil {
			t.Fatalf("create tenant %s: %v", organization, err)
		}
		if err := database.configureHostedPlans(t.Context(), hosted); err != nil {
			t.Fatalf("configure plans for %s: %v", organization, err)
		}
		service.registerRoutes(service.echo)
		services[organization] = service
	}

	request := func(t *testing.T, service *Service, token, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		response := httptest.NewRecorder()
		service.echo.ServeHTTP(response, request)
		return response
	}

	for _, organization := range organizations {
		service := services[organization]
		path := "/api/v2/organizations/" + organization + "/entitlements"
		if response := request(t, service, "wrong-admin-token", http.MethodGet, path, ""); response.Code != http.StatusForbidden {
			t.Fatalf("unauthorized GET for %s status=%d body=%s", organization, response.Code, response.Body)
		}
		otherOrganization := organizations[1]
		if organization == otherOrganization {
			otherOrganization = organizations[0]
		}
		otherPath := "/api/v2/organizations/" + otherOrganization + "/entitlements"
		if response := request(t, service, tokens[otherOrganization], http.MethodGet, otherPath, ""); response.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant GET for %s through %s status=%d body=%s", otherOrganization, organization, response.Code, response.Body)
		}
		if response := request(t, service, tokens[otherOrganization], http.MethodGet, path, ""); response.Code != http.StatusForbidden {
			t.Fatalf("other tenant admin GET for %s status=%d body=%s", organization, response.Code, response.Body)
		}
		response := request(t, service, tokens[organization], http.MethodGet, path, "")
		if response.Code != http.StatusOK {
			t.Fatalf("authorized GET for %s status=%d body=%s", organization, response.Code, response.Body)
		}
		var report hostedEntitlementReport
		if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode GET for %s: %v", organization, err)
		}
		if report.OrganizationID != organization || report.Base.ID != "pilot_free" || len(report.Grants) != 0 {
			t.Fatalf("unexpected initial report for %s: %#v", organization, report)
		}

		body := `{"idempotency_key":"grant-` + organization + `","action":"grant","expected_revision":1,"plan":{"id":"comp_team","version":1},"grant_id":"grant-` + organization + `","scope":["collaboration_bytes"],"starts_at":"2026-11-03T00:00:00Z","reason":"example grant"}`
		if response := request(t, service, "wrong-admin-token", http.MethodPost, path, body); response.Code != http.StatusForbidden {
			t.Fatalf("unauthorized POST for %s status=%d body=%s", organization, response.Code, response.Body)
		}
		if response := request(t, service, tokens[otherOrganization], http.MethodPost, path, body); response.Code != http.StatusForbidden {
			t.Fatalf("other tenant admin POST for %s status=%d body=%s", organization, response.Code, response.Body)
		}
		if response := request(t, service, tokens[organization], http.MethodPost, path, body); response.Code != http.StatusNoContent {
			t.Fatalf("authorized POST for %s status=%d body=%s", organization, response.Code, response.Body)
		}
	}

	for _, organization := range organizations {
		response := request(t, services[organization], tokens[organization], http.MethodGet, "/api/v2/organizations/"+organization+"/entitlements", "")
		if response.Code != http.StatusOK {
			t.Fatalf("read updated report for %s status=%d body=%s", organization, response.Code, response.Body)
		}
		var report hostedEntitlementReport
		if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
			t.Fatalf("decode updated report for %s: %v", organization, err)
		}
		if report.OrganizationID != organization || report.Revision != 2 || len(report.Grants) != 1 || report.Grants[0].ID != "grant-"+organization || len(report.Grants[0].Scope) != 1 || report.Grants[0].Scope[0] != "collaboration_bytes" || !report.Grants[0].StartsAt.Equal(time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC)) || report.Grants[0].ExpiresAt != nil {
			t.Fatalf("unexpected updated report for %s: %#v", organization, report)
		}
		if !slices.ContainsFunc(report.Plans, func(plan HostedPlan) bool {
			return plan.PlanReference == (PlanReference{ID: "comp_team", Version: 1}) && plan.Allowances["collaboration_bytes"] == 128<<20
		}) {
			t.Fatalf("updated report for %s omitted the effective grant plan", organization)
		}
		entitlement, err := services[organization].database.hostedEntitlement(t.Context(), nil, now)
		if err != nil {
			t.Fatalf("read effective entitlement for %s: %v", organization, err)
		}
		if entitlement.OrganizationID != organization || entitlement.Allowances["collaboration_bytes"] != 128<<20 {
			t.Fatalf("unexpected effective allowance for %s: %#v", organization, entitlement.Allowances)
		}
	}
}

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
