package hubserver

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorPostgresReadsPendingMessages(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC()
	for _, organization := range []tracker.OrganizationID{"org_coordinator_first", "org_coordinator_second"} {
		t.Run(string(organization), func(t *testing.T) {
			project := tracker.ProjectID("prj_" + string(organization))
			owner := "owner_" + string(organization)
			id := conversation.NewConversationID()
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: now}); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: owner, Name: "Example owner", Hash: owner + "-hash", Scope: "operator", ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Example", Slug: "example", Profile: "native", CreatedAt: now}, Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: project, ID: id, OwnerPrincipalID: owner, Origin: conversationOriginUser, Title: "Example", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Preferences: conversation.DefaultPreferences(), CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				deliveries := []conversation.Delivery{conversation.DeliveryDelivered, conversation.DeliverySaved, conversation.DeliveryQueued}
				for range coordinatorPendingMessages {
					deliveries = append(deliveries, conversation.DeliverySaved)
				}
				for _, delivery := range deliveries {
					if _, err := w.CreateMessage(t.Context(), conversationstore.Message{OrganizationID: organization, ConversationID: id, ID: conversation.NewMessageID(), Role: conversation.RoleUser, Actor: conversation.Actor{Kind: conversation.ActorHuman, PrincipalID: owner}, Kind: conversation.MessageText, Text: string(delivery), Delivery: delivery, CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			server := &Service{config: Config{TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(organization)}}}
			coordinator := &conversationTurnCoordinator{service: &conversationService{server: server, store: &conversationStore{}}}
			record, err := coordinator.readConversation(t.Context(), nil, id)
			if err != nil {
				t.Fatal(err)
			}
			if record.OrganizationID != organization || record.ID != id {
				t.Fatalf("wrong conversation: %+v", record)
			}
			messages, err := coordinator.pendingMessages(t.Context(), nil, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(messages) != coordinatorPendingMessages || messages[0].Seq != 2 || messages[len(messages)-1].Seq != coordinatorPendingMessages+1 || messages[0].Delivery != conversation.DeliverySaved || messages[1].Delivery != conversation.DeliveryQueued {
				t.Fatalf("pending messages = %+v", messages)
			}
		})
	}
}

func TestCoordinatorProjectReadsRespectKeyGrants(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Now().UTC()
	organization := tracker.OrganizationID("org_coordinator_access")
	projects := []tracker.ProjectID{"prj_first", "prj_second"}
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "owner", Name: "Example owner", Hash: "owner-hash", Scope: "operator", ProjectAccess: "all", Kind: "personal", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "example_user", Email: "user@example.test", MembershipID: "example_membership", Role: "owner", Active: true, PrincipalID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		for _, project := range projects {
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native", CreatedAt: now}, Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			key := "key_" + string(project)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: key, Name: key, Hash: key + "-hash", Scope: "operator", ProjectAccess: "selected", Kind: "personal", UserID: "example_user", MembershipID: "example_membership", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "example_user", ProjectID: project}); err != nil {
				return err
			}
			for _, token := range []string{"owner", key} {
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: token, ProjectID: project}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := &Service{config: Config{Hosted: &HostedConfig{OrganizationID: string(organization)}, now: func() time.Time { return now }}}
	for _, project := range projects {
		t.Run(string(project), func(t *testing.T) {
			key := "key_" + string(project)
			scope := nativeScope{organization: organization, credential: apiCredential{ID: key, Hash: key + "-hash", Scope: apiScopeOperator, HostedRole: "owner", HostedMembership: "example_membership", Hosted: &auth.HostedIdentity{Subject: "example_user"}}}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				allowed, err := service.coordinatorAllowedProjects(t.Context(), r, scope, projects)
				if err != nil {
					return err
				}
				if !slices.Equal(allowed, []tracker.ProjectID{project}) {
					t.Fatalf("key projects = %v, want only %s", allowed, project)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
