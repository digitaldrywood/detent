package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEntryKeyAdministrationCatalog(t *testing.T) {
	f := newSharedRouteFixture(t, func(cfg *Config) {
		plans := pilotHostedPlans()
		cfg.Hosted.Plans = &plans
		org := cfg.Hosted.OrganizationID
		cfg.Hosted.Provider = newHostedSecurityProvider()
		cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: org, ProviderOrganization: "provider_" + org, Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
		}
	})
	for _, org := range []string{"org_startup", "org_other"} {
		t.Run(org, func(t *testing.T) {
			service := f.services[org]
			provider := service.config.Hosted.Provider.(*hostedSecurityProvider)
			for _, suffix := range []string{"owner", "member"} {
				id := org + "_" + suffix
				member := auth.Membership{ID: id, UserID: id, OrganizationID: "provider_" + org, Status: "active"}
				member.Role.Slug = suffix
				provider.members[id] = member
			}
			organization := tracker.OrganizationID(org)
			if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				now := time.Now().UTC()
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project", Name: "Example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: org + "_owner", ProjectID: "project", CanWrite: true}); err != nil {
					return err
				}
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: org + "_member", Name: "Example member", Hash: apikey.HashToken(org + "_member"), Scope: "operator", Kind: "personal", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				return w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: org + "_member", Email: org + "_member@example.test", MembershipID: org + "_member", PrincipalID: org + "_member", Role: "member", Active: true, CreatedAt: now, UpdatedAt: now})
			}); err != nil {
				t.Fatal(err)
			}
			keyClaims := func(c *cloudassert.Claims) {
				c.Kind, c.ProviderOrganization = cloudassert.KindKey, "provider_"+org
				c.Key = &apikey.KeyAuthority{ID: org + "_key", CredentialBinding: strings.Repeat("e", 64), Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}
			}
			catalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, keyClaims)
			requireNativeStatus(t, catalog, http.StatusOK)
			var definitions []operatortool.Definition
			decodeHubResponse(t, catalog, &definitions)
			for _, test := range []struct {
				name string
				args map[string]any
			}{
				{operatortool.OrganizationSession, map[string]any{}},
				{operatortool.MembershipList, map[string]any{}},
				{operatortool.InvitationSend, map[string]any{"email": org + "_invite@example.test", "role": "member"}},
				{operatortool.InvitationEdit, map[string]any{"invitation_id": "invitation_1", "grants": []any{}}},
				{operatortool.InvitationResend, map[string]any{"invitation_id": "invitation_1"}},
				{operatortool.MemberRole, map[string]any{"member_id": org + "_member", "role": "viewer"}},
				{operatortool.MemberGrant, map[string]any{"member_id": org + "_member", "project_id": "project", "write": true}},
			} {
				t.Run(test.name, func(t *testing.T) {
					definition, _ := operatortool.Lookup(test.name)
					if !entryKeyHasDefinition(definitions, test.name) {
						t.Errorf("%s missing from signed catalog", test.name)
					}
					if !definition.Annotations.ReadOnly {
						test.args["request_id"] = org + "_" + test.name
					}
					body, err := json.Marshal(map[string]any{"name": test.name, "connection_id": org + "_admin", "arguments": test.args})
					if err != nil {
						t.Fatal(err)
					}
					if !definition.Annotations.ReadOnly {
						for _, restrict := range []func(*cloudassert.Claims){func(c *cloudassert.Claims) { c.Key.Permission = apikey.ScopeRead }, func(c *cloudassert.Claims) { c.Role = "viewer" }} {
							deniedClaims := func(c *cloudassert.Claims) { keyClaims(c); restrict(c) }
							deniedCatalog := f.request(t, org, "/internal/v1/keys/catalog", `{}`, deniedClaims)
							requireNativeStatus(t, deniedCatalog, http.StatusOK)
							var deniedDefinitions []operatortool.Definition
							decodeHubResponse(t, deniedCatalog, &deniedDefinitions)
							if entryKeyHasDefinition(deniedDefinitions, test.name) {
								t.Fatal("restricted administrator tool advertised")
							}
							requireNativeStatus(t, f.request(t, org, "/internal/v1/keys/call", string(body), deniedClaims), http.StatusForbidden)
						}
					}
					response := f.request(t, org, "/internal/v1/keys/call", string(body), keyClaims)
					requireNativeStatus(t, response, http.StatusOK)
					if definition.Annotations.ReadOnly {
						if !strings.Contains(response.Body.String(), org) {
							t.Fatal("read lost tenant identity")
						}
						return
					}
					var result struct{ Action chat.Action }
					decodeHubResponse(t, response, &result)
					if result.Action.Status != chat.ActionSucceeded {
						t.Fatalf("administration failed: %s", response.Body)
					}
				})
			}
			for _, name := range []string{operatortool.InvitationRevoke, operatortool.MemberRemove} {
				if entryKeyHasDefinition(definitions, name) {
					t.Fatal("incomplete deletion adapter advertised")
				}
				body, err := json.Marshal(map[string]any{"name": name, "connection_id": org + "_admin", "arguments": map[string]any{"request_id": name, "invitation_id": "invitation_1", "member_id": org + "_member"}})
				if err != nil {
					t.Fatal(err)
				}
				requireNativeStatus(t, f.request(t, org, "/internal/v1/keys/call", string(body), keyClaims), http.StatusForbidden)
			}
			if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				member, err := r.AccessMember(t.Context(), org+"_member")
				if err == nil && (!member.Active || member.Role != "viewer") {
					t.Error("member role did not persist")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if provider.invitations["invitation_1"].State != "pending" || provider.members[org+"_member"].Role.Slug != "viewer" {
				t.Fatal("provider mutations did not complete")
			}
		})
	}
}
