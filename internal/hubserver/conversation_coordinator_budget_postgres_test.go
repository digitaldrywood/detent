package hubserver

import (
	"context"
	"crypto/sha256"
	"net/http"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type coordinatorBudgetProvider struct {
	*hostedSecurityProvider
	organization string
}

func (p coordinatorBudgetProvider) Organization(_ context.Context, id string) (auth.Organization, error) {
	return auth.Organization{ID: id, ExternalID: p.organization, Name: "Example organization"}, nil
}

func coordinatorBudgetPeer(t *testing.T, first hostedSecurityFixture) hostedSecurityFixture {
	t.Helper()
	cfg := first.service.config
	hosted := *cfg.Hosted
	cfg.Hosted = &hosted
	hosted.OrganizationID, hosted.WorkOSOrganizationID, hosted.BootstrapSubject = "org_budget_second", "provider_budget_second", "owner_budget_second"
	provider := newHostedSecurityProvider()
	hosted.Provider = coordinatorBudgetProvider{hostedSecurityProvider: provider, organization: hosted.OrganizationID}
	cfg.InitialAdminToken = []byte("example-second-budget-admin")
	organization := tracker.OrganizationID(hosted.OrganizationID)
	now := cfg.Clock()
	key := sha256.Sum256([]byte(hosted.OrganizationID))
	envelope, err := cfg.SecretKeys.Seal(key[:], accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
	if err != nil {
		t.Fatal(err)
	}
	deployment, generation := hosted.Deployment()
	if err := first.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example second organization", Slug: "budget-second", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.AddAccessOrganizationSlug(t.Context(), "budget-second"); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: hosted.WorkOSOrganizationID, BootstrapSubject: hosted.BootstrapSubject, Deployment: deployment, PublicURL: hosted.PublicURL, AllocationGeneration: generation}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "admin_" + string(organization), Name: "Example administrator", Hash: apikey.HashToken(string(cfg.InitialAdminToken)), Scope: "admin", CreatedAt: now, UpdatedAt: now, KeyScope: "admin", ProjectAccess: "all", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: hubapi.NewID("hub"), CursorKey: key[:]}); err != nil {
			return err
		}
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	service := openHostedPostgresService(t, cfg)
	project := tracker.ProjectID("prj_budget_second")
	seedHostedPostgresProject(t, service, project)
	return hostedSecurityFixture{service: service, provider: provider, project: project, base: "/api/v2/organizations/" + hosted.OrganizationID + "/projects/" + string(project)}
}

func coordinatorBudgetUser(t *testing.T, f hostedSecurityFixture, name, role string, manageRunner bool) hostedSecurityUser {
	t.Helper()
	organization := f.service.config.Hosted.OrganizationID
	now := f.service.config.Clock()
	subject := organization + "_" + name
	identity := auth.Identity{Subject: subject, Email: subject + "@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: subject, OrganizationID: f.service.config.Hosted.WorkOSOrganizationID, SessionID: "session_" + subject, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
	f.provider.mu.Lock()
	f.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
	f.provider.mu.Unlock()
	member, err := f.provider.CreateMembership(t.Context(), subject, identity.Hosted.OrganizationID, role)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.addHostedMember(t.Context(), identity, member); err != nil {
		t.Fatal(err)
	}
	token, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		stored, err := w.AccessMember(t.Context(), subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: tracker.OrganizationID(organization), UserID: subject, ProjectID: f.project, ManageRunner: manageRunner}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(organization), TokenID: stored.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
	return hostedSecurityUser{identity: identity, token: token}
}

func coordinatorBudgetTools(t *testing.T, f hostedSecurityFixture, user hostedSecurityUser) (*coordinatorToolset, context.Context, conversationRecord) {
	t.Helper()
	organization := tracker.OrganizationID(f.service.config.Hosted.OrganizationID)
	now := f.service.config.Clock()
	var record conversationRecord
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), user.identity.Subject)
		if err != nil {
			return err
		}
		row, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ID: conversation.NewConversationID(), ProjectID: f.project, OwnerPrincipalID: member.PrincipalID, OwnerSubject: user.identity.Subject, Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Preferences: conversation.DefaultPreferences(), Execution: conversation.Execution{Status: conversation.ExecutionIdle, UpdatedAt: now}, CreatedAt: now, UpdatedAt: now})
		record = conversationRecordFromStore(row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var actorCtx context.Context
	path := "/api/v2/organizations/:organization/budget-context/" + record.ID
	f.service.echo.GET(path, func(e echo.Context) error {
		var err error
		actorCtx, err = f.service.bindCoordinatorConnection(e.Request().Context(), record.ID)
		if err != nil {
			return err
		}
		return e.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	response := f.request(t, user, http.MethodGet, "/api/v2/organizations/"+string(organization)+"/budget-context/"+record.ID, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if actorCtx == nil {
		t.Fatal("budget coordinator context is missing")
	}
	coordinator := f.service.conversations.Coordinator.(*conversationTurnCoordinator)
	state := &coordinatorTurnState{coordinator: coordinator, conversationID: record.ID, users: []conversationMessageRecord{{Role: conversation.RoleUser, CreatedAt: now, Data: coordinatorConnectionData(actorCtx)}}}
	return newCoordinatorToolset(coordinator, state), actorCtx, record
}
