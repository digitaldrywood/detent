package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/platformusers"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresTenantAccessWithoutSQLite(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	provider := newHostedSecurityProvider()
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org)}); err != nil {
				return err
			}
			_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: "prj_example", Name: "Example", Profile: "native"})
			return err
		}); err != nil {
			t.Fatalf("seed tenant %s: %v cause=%v", org, err, errors.Unwrap(err))
		}
		member := auth.Membership{ID: "membership_" + string(org), UserID: "user_example", OrganizationID: "provider_" + string(org), Status: "active"}
		member.Role.Slug = "admin"
		provider.members[member.ID] = member
		service := &Service{config: Config{AccessRepository: repository, TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(org), Provider: provider, SharedEntry: &HostedSharedEntry{}}, SecretKeys: secretTestKeys(t, "1", "1"), now: func() time.Time { return now }, generateToken: apikey.GenerateToken, newTokenID: func() string { return "key_example" }}}
		identity := &auth.HostedIdentity{Subject: member.UserID, OrganizationID: member.OrganizationID, SessionID: "session_example", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		provider.sessions[identity.SessionID] = *identity
		if err := service.storeHostedMember(t.Context(), auth.Identity{Subject: member.UserID, Email: "member@example.com", EmailVerified: true}, member, ""); err != nil {
			t.Fatal(err)
		}
		if err := service.CreateWebSession(t.Context(), auth.SessionRecord{TokenHash: "binding", Email: "member@example.com", Identity: identity, CreatedAt: now, ExpiresAt: identity.ExpiresAt}); err != nil {
			t.Fatal(err)
		}
		session, err := service.storedWebSession(t.Context(), "binding", now)
		if err != nil {
			t.Fatal(err)
		}
		credential, status, err := service.hostedSharedCredential(t.Context(), session, "binding", "admin")
		if err != nil || status != http.StatusOK {
			t.Fatalf("credential status=%d error=%v", status, err)
		}
		if !service.appAllRunnerGrants(t.Context(), credential) {
			t.Fatal("typed administrator cannot manage runners")
		}
		scope := nativeScope{organization: org, project: "prj_example", credential: credential, requireHostedAdmin: true}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: member.UserID, ProjectID: scope.project, CanWrite: true}); err != nil {
				return err
			}
			return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: scope.project})
		}); err != nil {
			t.Fatal(err)
		}
		if err := service.requireOperatorRunners(t.Context(), nil, scope); err == nil {
			t.Fatal("runner access accepted without management grant")
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: member.UserID, ProjectID: scope.project, CanWrite: true, ManageRunner: true})
		}); err != nil {
			t.Fatal(err)
		}
		if err := service.requireOperatorRunners(t.Context(), nil, scope); err != nil {
			t.Fatalf("runner management grant rejected: %v", err)
		}
		token, err := service.createAPITokenFor(t.Context(), tokenRequest{Name: "Example key", Scope: apiScopeOperator, Issuer: &credential, KeyScope: apikey.ScopeRead, ProjectAccess: hostedProjectsAll})
		if err != nil {
			t.Fatal(err)
		}
		authenticated, status, err := service.authenticateAPIToken(t.Context(), token.Token, "", "")
		if err != nil || status != http.StatusOK || authenticated.HostedMembership != member.ID {
			t.Fatalf("API key status=%d error=%v", status, err)
		}
		if err := service.revokeAPITokenFor(t.Context(), token.ID); err != nil {
			t.Fatal(err)
		}
		if _, status, err := service.authenticateAPIToken(t.Context(), token.Token, "", ""); err == nil || status != http.StatusUnauthorized {
			t.Fatalf("revoked key status=%d error=%v", status, err)
		}
		envelope, err := service.config.SecretKeys.Seal([]byte("example secret"), accessstore.SecretAAD(org, scope.project, flySpritesToken))
		if err != nil {
			t.Fatal(err)
		}
		secret := accessstore.Secret{OrganizationID: org, ProjectID: scope.project, Kind: flySpritesToken, OrganizationSlug: "example", Envelope: envelope, UpdatedAt: now}
		if err := service.accessSecretWrite(t.Context(), scope, func(w tenantstore.Writer) error {
			return w.StoreAccessSecret(t.Context(), secret, service.accessSecretAudit(scope, "set", envelope.Version))
		}); err != nil {
			t.Fatal(err)
		}
		resolved, slug, readEnvelope, err := service.resolveSpritesSecret(t.Context(), nil, scope)
		if err != nil || resolved.project != scope.project || slug != "example" || readEnvelope.Version != envelope.Version {
			t.Fatalf("secret execution read error=%v", err)
		}
		metadata, err := service.readAccessSecretStatus(t.Context(), scope)
		if err != nil || !metadata.Present {
			t.Fatalf("secret metadata=%+v error=%v", metadata, err)
		}
		if err := service.checkAccessSecretKeys(t.Context()); err != nil {
			t.Fatal(err)
		}
		service.config.SecretKeys = nil
		if err := service.checkAccessSecretKeys(t.Context()); !errors.Is(err, hubsecrets.ErrUnavailable) {
			t.Fatalf("missing key startup=%v", err)
		}
		service.config.SecretKeys = secretTestKeys(t, "1", "1")
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		claims := cloudassert.Claims{Kind: cloudassert.KindBrowser, Audience: string(org), Subject: member.UserID, ProviderOrganization: member.OrganizationID, ProviderSession: identity.SessionID, SessionCreatedAt: now, SessionExpiresAt: identity.ExpiresAt, AccessExpiresAt: identity.ExpiresAt, Email: "member@example.com", Role: "admin", Binding: "shared_binding"}
		c.Set(hostedSharedClaimsKey, claims)
		if _, _, err := service.sharedHostedSession(t.Context(), c); err != nil {
			t.Fatal(err)
		}
		if err := service.revokeHostedSharedBinding(t.Context(), claims.Binding, formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := service.sharedHostedSession(t.Context(), c); err == nil {
			t.Fatal("shared binding resurrected after revocation")
		}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			m, err := w.AccessMember(t.Context(), member.UserID)
			if err != nil {
				return err
			}
			m.Role = "viewer"
			return w.UpdateAccessMember(t.Context(), m)
		}); err != nil {
			t.Fatal(err)
		}
		if err := service.accessSecretWrite(t.Context(), scope, func(tenantstore.Writer) error { return fmt.Errorf("authorization should reject this callback") }); !errors.Is(err, accessstore.ErrDenied) {
			t.Fatalf("stale admin role=%v", err)
		}
	}
}

func TestAccessInvitationLifecycleIsTenantScoped(t *testing.T) {
	repository := projectRecordPostgres(t)
	provider := newHostedSecurityProvider()
	now := time.Now().UTC().Truncate(time.Microsecond)
	services := make(map[tracker.OrganizationID]*Service)
	credentials := make(map[tracker.OrganizationID]apiCredential)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		providerID := "provider_" + string(org)
		membershipID := "membership_admin_" + string(org)
		identity := &auth.HostedIdentity{Subject: "admin_example", OrganizationID: providerID, SessionID: "session_" + string(org), CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		member := auth.Membership{ID: membershipID, UserID: identity.Subject, OrganizationID: providerID, Status: "active"}
		member.Role.Slug = "admin"
		provider.members[membershipID] = member
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: now}); err != nil {
				return fmt.Errorf("create organization: %w", err)
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: providerID}); err != nil {
				return fmt.Errorf("create tenant: %w", err)
			}
			for _, id := range []tracker.ProjectID{"project_first", "project_second"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: id, Name: string(id), Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}}); err != nil {
					return fmt.Errorf("create project: %w", err)
				}
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "admin_key", Name: "Admin key", Hash: "hash_" + string(org), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: identity.Subject, MembershipID: membershipID, ProjectAccess: "selected", Kind: "personal"}); err != nil {
				return fmt.Errorf("create credential: %w", err)
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: identity.Subject, Email: "admin@example.test", MembershipID: membershipID, Role: "admin", Active: true, PrincipalID: "admin_key", CreatedAt: now, UpdatedAt: now}); err != nil {
				return fmt.Errorf("create member: %w", err)
			}
			for _, id := range []tracker.ProjectID{"project_first", "project_second"} {
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: identity.Subject, ProjectID: id, CanWrite: true, ManageRunner: true}); err != nil {
					return fmt.Errorf("create project grant: %w", err)
				}
				if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "admin_key", ProjectID: id}); err != nil {
					return fmt.Errorf("create token grant: %w", err)
				}
			}
			return nil
		}); err != nil {
			t.Fatalf("seed lifecycle tenant %s: %v cause=%v", org, err, errors.Unwrap(err))
		}
		services[org] = &Service{config: Config{AccessRepository: repository, TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(org), Provider: provider, SharedEntry: &HostedSharedEntry{}}, now: func() time.Time { return now }, generateToken: func() (string, error) { return "generated_example_token", nil }}}
		credentials[org] = apiCredential{ID: "admin_key", Hash: "hash_" + string(org), Scope: apiScopeOperator, NativeOnly: true, Hosted: identity, HostedRole: "admin", HostedMembership: membershipID, HostedPrincipal: "admin_key"}
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			row := accessstore.MemberReservation{OrganizationID: org, Email: "seat@example.test", ExpiresAt: now.Add(time.Hour)}
			created, err := w.ReserveAccessMemberReservation(t.Context(), row, now, 2)
			if err != nil || !created {
				return fmt.Errorf("tenant %s reservation created=%t error=%v", org, created, err)
			}
			repeated, err := w.ReserveAccessMemberReservation(t.Context(), row, now, 2)
			if err != nil || repeated {
				return fmt.Errorf("tenant %s repeated reservation created=%t error=%v", org, repeated, err)
			}
			_, err = w.ReserveAccessMemberReservation(t.Context(), accessstore.MemberReservation{OrganizationID: org, Email: "over_limit@example.test", ExpiresAt: now.Add(time.Hour)}, now, 2)
			if !errors.Is(err, accessstore.ErrSeatLimit) {
				return fmt.Errorf("member seat limit error=%v", err)
			}
			return w.DeleteAccessMemberReservation(t.Context(), row.Email)
		}); err != nil {
			t.Fatal(err)
		}
	}

	first, err := services["org_first"].createAccessInvitation(t.Context(), credentials["org_first"], "member@example.test", "member", []hostedMemberGrant{{ProjectID: "project_first", Write: true}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := services["org_second"].createAccessInvitation(t.Context(), credentials["org_second"], "member@example.test", "member", []hostedMemberGrant{{ProjectID: "project_second", Runner: true}})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID {
		t.Fatalf("provider reused invitation ID %q", first.ID)
	}
	for org, expected := range map[tracker.OrganizationID]string{"org_first": first.ID, "org_second": second.ID} {
		if err := services[org].accessRead(t.Context(), org, func(r tenantstore.Reader) error {
			row, err := r.AccessInvitation(t.Context(), expected)
			if err != nil {
				return err
			}
			if row.OrganizationID != org || row.Email != "member@example.test" {
				t.Fatalf("tenant %s invitation=%+v", org, row)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	firstList, err := services["org_first"].accessPendingInvitations(t.Context())
	if err != nil || len(firstList) != 1 || firstList[0].ID != first.ID {
		t.Fatalf("first tenant invitations=%+v error=%v", firstList, err)
	}
	secondList, err := services["org_second"].accessPendingInvitations(t.Context())
	if err != nil || len(secondList) != 1 || secondList[0].ID != second.ID {
		t.Fatalf("second tenant invitations=%+v error=%v", secondList, err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/platform/users", nil)
		c := echo.New().NewContext(request, recorder)
		parsed := platformusers.Parse("member@example.test")
		if err := services[org].hostedAccessPlatformUsers(c, t.Context(), parsed, platformusers.Query{Text: parsed.Text}, platformusers.TenantResult{Members: []platformusers.TenantMember{}, Invitations: []platformusers.TenantInvitation{}}); err != nil {
			t.Fatal(err)
		}
		var listed platformusers.TenantResult
		if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil || len(listed.Invitations) != 1 || listed.Invitations[0].Email != "member@example.test" || listed.NextCursor != "" {
			t.Fatalf("platform tenant invitation search=%+v decode_error=%v", listed, err)
		}
	}
	if err := services["org_first"].accessRead(t.Context(), "org_first", func(r tenantstore.Reader) error {
		return services["org_first"].authorizeAccessScope(t.Context(), r, nativeScope{organization: "org_first", credential: credentials["org_first"], requireHostedAdmin: true}, true)
	}); err != nil {
		t.Fatalf("first tenant admin authorization before edit: %v", err)
	}
	if err := services["org_first"].editAccessInvitation(t.Context(), credentials["org_first"], first.ID, []hostedMemberGrant{{ProjectID: "project_second", Runner: true}}); err != nil {
		t.Fatal(err)
	}
	if err := services["org_first"].accessRead(t.Context(), "org_first", func(r tenantstore.Reader) error {
		grants, err := r.AccessInvitationGrants(t.Context(), first.ID, "", 10)
		if err != nil || len(grants) != 1 || grants[0].ProjectID != "project_second" || !grants[0].ManageRunner {
			t.Fatalf("edited grants=%+v error=%v", grants, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	accepted := auth.Identity{Subject: "member_first", Email: "member@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: "member_first", OrganizationID: "provider_org_first", SessionID: "session_member", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
	if err := services["org_first"].acceptAccessInvitation(t.Context(), accepted, first.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := services["org_first"].accessRead(t.Context(), "org_first", func(r tenantstore.Reader) error {
		grant, err := r.AccessProjectGrant(t.Context(), accepted.Subject, "project_second")
		if err != nil || !grant.ManageRunner {
			t.Fatalf("accepted project grant=%+v error=%v", grant, err)
		}
		row, err := r.AccessInvitation(t.Context(), first.ID)
		if err != nil || row.AcceptedUserID != accepted.Subject {
			t.Fatalf("accepted invitation=%+v error=%v", row, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := services["org_second"].revokeAccessInvitation(t.Context(), credentials["org_second"], second.ID); err != nil {
		t.Fatal(err)
	}
	if err := services["org_second"].accessRead(t.Context(), "org_second", func(r tenantstore.Reader) error {
		_, err := r.AccessInvitation(t.Context(), second.ID)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("revoked invitation read error=%v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := services["org_first"].accessRead(t.Context(), "org_first", func(r tenantstore.Reader) error {
		row, err := r.AccessInvitation(t.Context(), first.ID)
		if err != nil || row.AcceptedUserID != accepted.Subject {
			t.Fatalf("other tenant revoke changed accepted invitation=%+v error=%v", row, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	lostResponseEmail := "lost-response@example.test"
	if err := repository.WriteTenant(t.Context(), "org_first", corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessMemberReservation(t.Context(), accessstore.MemberReservation{OrganizationID: "org_first", Email: lostResponseEmail, ExpiresAt: now.Add(time.Hour)})
	}); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	provider.inviteErrAfterCreate = errors.New("response lost after provider accepted invitation")
	provider.mu.Unlock()
	_, err = services["org_first"].createAccessInvitation(t.Context(), credentials["org_first"], lostResponseEmail, "member", nil)
	if !errors.Is(err, mutation.ErrUncertain) {
		t.Fatalf("lost provider response error=%v, want uncertain outcome", err)
	}
	provider.mu.Lock()
	lostResponseCount := len(provider.invitations)
	provider.mu.Unlock()
	if lostResponseCount != 3 {
		t.Fatalf("provider invitations=%d, want the invitation committed before response loss", lostResponseCount)
	}
	if err := services["org_first"].accessRead(t.Context(), "org_first", func(r tenantstore.Reader) error {
		reservation, err := r.AccessMemberReservation(t.Context(), lostResponseEmail)
		if err != nil {
			return err
		}
		if reservation.Email != lostResponseEmail || reservation.OrganizationID != "org_first" {
			t.Fatalf("uncertain invitation reservation=%+v", reservation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSharedAccessSessionReadsExistingBindingWithoutWriting(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	organization := tracker.OrganizationID("org_shared_session")
	identity := auth.HostedIdentity{Subject: "user_example", OrganizationID: "provider_example", SessionID: "session_example", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	row := accessstore.Session{OrganizationID: organization, Hash: "binding_example", Email: "member@example.test", Subject: identity.Subject, ProviderOrganizationID: identity.OrganizationID, SessionID: identity.SessionID, IdentityCreatedAt: identity.CreatedAt, IdentityExpiresAt: identity.ExpiresAt, CreatedAt: now, ExpiresAt: identity.ExpiresAt}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: identity.OrganizationID}); err != nil {
			return err
		}
		return w.CreateAccessSession(t.Context(), row)
	}); err != nil {
		t.Fatal(err)
	}
	counted := &countingTenantRepository{Repository: repository}
	service := &Service{config: Config{AccessRepository: counted, Hosted: &HostedConfig{OrganizationID: string(organization)}, now: func() time.Time { return now }}}
	claims := cloudassert.Claims{Kind: cloudassert.KindBrowser, Audience: string(organization), Subject: identity.Subject, ProviderOrganization: identity.OrganizationID, ProviderSession: identity.SessionID, SessionCreatedAt: identity.CreatedAt, SessionExpiresAt: identity.ExpiresAt, AccessExpiresAt: identity.ExpiresAt, Email: row.Email, Role: "admin", Binding: row.Hash}
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	if _, _, err := service.sharedAccessSession(t.Context(), c, claims); err != nil {
		t.Fatalf("existing binding authentication: %v", err)
	}
	if counted.reads != 1 || counted.writes != 0 {
		t.Fatalf("existing binding repository calls = reads %d, writes %d; want reads 1, writes 0", counted.reads, counted.writes)
	}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.RevokeAccessSession(t.Context(), row.Hash, now)
	}); err != nil {
		t.Fatal(err)
	}
	counted.reads, counted.writes = 0, 0
	if _, _, err := service.sharedAccessSession(t.Context(), c, claims); err == nil {
		t.Fatal("revoked binding authenticated")
	}
	if counted.reads != 1 || counted.writes != 0 {
		t.Fatalf("revoked binding repository calls = reads %d, writes %d; want reads 1, writes 0", counted.reads, counted.writes)
	}
}

type countingTenantRepository struct {
	tenantstore.Repository
	reads  int
	writes int
}

func (r *countingTenantRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	r.reads++
	return r.Repository.ReadTenant(ctx, organization, workload, read)
}

func (r *countingTenantRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	r.writes++
	return r.Repository.WriteTenant(ctx, organization, workload, write)
}
