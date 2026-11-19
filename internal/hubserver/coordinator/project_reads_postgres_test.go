package coordinator

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCoordinatorProjectReadsRespectKeyGrants(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
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
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Slug: strings.ReplaceAll(string(project), "_", "-"), Profile: "native", CreatedAt: now}, Settings: projectstore.Settings{OrganizationID: organization, ProjectID: project}, States: hubtest.States()}); err != nil {
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
	authenticator := hubauth.New(hubauth.Deps{Organization: organization, Hosted: &hubauth.Hosted{OrganizationID: string(organization)}, Now: func() time.Time { return now }})
	authorize := func(ctx context.Context, r accessstore.Reader, scope hubscope.NativeScope, write bool) error {
		return authenticator.Authorize(ctx, r, conversations.ScopeInput(scope), write)
	}
	service := &conversations.ConversationService{Store: &conversations.ConversationStore{Contents: postgresConversationObjects{hubtest.NewDetails()}}}
	service.Server = conversations.NewServer(conversations.Deps{
		Config:        func() conversations.Config { return conversations.Config{TenantRepository: repository} },
		Conversations: func() *conversations.ConversationService { return service },
		Authorization: hubscope.Authorization{Access: authorize},
	})
	for _, project := range projects {
		t.Run(string(project), func(t *testing.T) {
			key := "key_" + string(project)
			scope := hubscope.NativeScope{Organization: organization, Credential: hubapi.APICredential{ID: key, Hash: key + "-hash", Scope: hubapi.APIScopeOperator, HostedRole: "owner", HostedMembership: "example_membership", Hosted: &auth.HostedIdentity{Subject: "example_user"}}}
			if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				allowed, err := AllowedProjects(t.Context(), r, scope, projects, authorize)
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
			if project == projects[1] {
				return
			}
			other := projects[1]
			scope.Project = project
			for _, restriction := range []string{"token grant", "entry projects", "entry blocked projects"} {
				t.Run(restriction, func(t *testing.T) {
					current := scope
					current.Credential.HostedKeyScope = apikey.ScopeRead
					if restriction != "token grant" {
						current.Credential.EntryKey = &apikey.KeyAuthority{ID: key, Kind: "personal", Permission: apikey.ScopeRead, ProjectContext: apikey.ProjectContext{Access: "all"}}
					}
					id := conversation.NewConversationID()
					text := "Private answer from " + string(other)
					body, err := conversations.MessageContent(conversations.ConversationMessageRecord{Text: text})
					if err != nil {
						t.Fatal(err)
					}
					ref, err := service.Store.PublishPayload(t.Context(), string(organization), body)
					if err != nil {
						t.Fatal(err)
					}
					contentID := conversations.ContentArchiveID(string(organization), ref)
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: key, ProjectID: other}); err != nil {
							return err
						}
						allowed, err := AllowedProjects(t.Context(), w, current, projects, authorize)
						if err != nil {
							return err
						}
						if !slices.Equal(allowed, projects) {
							t.Fatalf("initial key access = %v", allowed)
						}
						audience, err := conversations.ConversationReadAudienceRecord(t.Context(), w, organization, hubscope.AuthCredential(current.Credential))
						if err != nil {
							return err
						}
						if err := w.PutConversationContent(t.Context(), conversationstore.ContentObject{OrganizationID: organization, ID: contentID, Reference: ref, CreatedAt: now}); err != nil {
							return err
						}
						if _, err := w.CreateConversation(t.Context(), conversationstore.Conversation{OrganizationID: organization, ProjectID: project, ID: id, OwnerPrincipalID: key, Origin: conversations.ConversationOriginUser, Title: "Cross-project answer", Visibility: conversation.VisibilityPrivate, Status: conversation.StatusActive, Revision: 1, Preferences: conversation.DefaultPreferences(), ReadAudience: audience, CreatedAt: now, UpdatedAt: now}); err != nil {
							return err
						}
						_, err = w.CreateMessage(t.Context(), conversationstore.Message{OrganizationID: organization, ConversationID: id, ID: conversation.NewMessageID(), Role: conversation.RoleAssistant, Actor: conversation.Actor{Kind: conversation.ActorRunner}, Kind: conversation.MessageText, ContentReference: contentID, TextBytes: int64(len(text)), StoredTextBytes: int64(len(text)), Delivery: conversation.DeliveryDelivered, CreatedAt: now, UpdatedAt: now})
						return err
					}); err != nil {
						t.Fatal(err)
					}
					page, err := service.Server.ReadPGConversationPage(t.Context(), conversations.ScopeInput(current), id, 0, 10)
					if err != nil {
						t.Fatal(err)
					}
					if len(page.Messages) != 1 || page.Messages[0].Text != text {
						t.Fatalf("stored answer = %+v", page.Messages)
					}
					switch restriction {
					case "token grant":
						if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteAccessTokenGrant(t.Context(), key, other) }); err != nil {
							t.Fatal(err)
						}
					case "entry projects":
						current.Credential.EntryKey.ProjectContext = apikey.ProjectContext{Access: "selected", Projects: []string{string(project)}}
					case "entry blocked projects":
						current.Credential.EntryKey.BlockedProjects = []string{string(other)}
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						allowed, err := AllowedProjects(t.Context(), r, current, projects, authorize)
						if err == nil && !slices.Equal(allowed, []tracker.ProjectID{project}) {
							t.Fatalf("narrowed key access = %v", allowed)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if _, err := service.Server.ReadPGConversationPage(t.Context(), conversations.ScopeInput(current), id, 0, 10); !hubapi.IsNativeNotFound(err) {
						t.Errorf("stored answer remained readable after %s narrowing: %v", restriction, err)
					}
					if _, err := service.Server.ReadPGConversation(t.Context(), conversations.ScopeInput(current), id); !hubapi.IsNativeNotFound(err) {
						t.Errorf("stored conversation remained readable after %s narrowing: %v", restriction, err)
					}
					if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteAccessTokenGrant(t.Context(), key, other) }); err != nil {
						t.Fatal(err)
					}
				})
			}

		})
	}
}
