package access_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
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
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type entryCatalogInvitationProvider struct{ *hubfixture.HostedProvider }

func (p entryCatalogInvitationProvider) InvitationByID(ctx context.Context, id string) (auth.Invitation, error) {
	return p.Invitation(ctx, id)
}

func (p entryCatalogInvitationProvider) AcceptInvitationByID(ctx context.Context, id, user string) error {
	return p.AcceptInvitation(ctx, id, user)
}

func TestPostgresEntryKeyAdministrationCatalog(t *testing.T) {
	t.Parallel()
	providers := map[string]*hubfixture.HostedProvider{}
	f := openEntryCatalog(t, func(cfg *hubfixture.Config) {
		plans := hostedplans.PilotPlans()
		cfg.Hosted.Plans = &plans
		org := cfg.Hosted.OrganizationID
		providers[org] = cfg.Hosted.Provider.(*hubfixture.HostedProvider)
		cfg.Hosted.Provider = entryCatalogInvitationProvider{providers[org]}
		cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: org, ProviderOrganization: "provider_" + org, Subject: origin.Subject, Role: "owner", Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: apikey.ScopeAdmin, ProjectContext: apikey.ProjectContext{Access: "all"}}}, nil
		}
	})
	for _, org := range f.organizations {
		t.Run(org, func(t *testing.T) {
			provider := providers[org]
			for _, suffix := range []string{"owner", "member"} {
				id := org + "_" + suffix
				member := auth.Membership{ID: id, UserID: id, OrganizationID: "provider_" + org, Status: "active"}
				member.Role.Slug = suffix
				provider.Mu.Lock()
				provider.Members[id] = member
				provider.Mu.Unlock()
			}
			organization := tracker.OrganizationID(org)
			if err := f.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				now := time.Now().UTC()
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "project", Name: "Example", Profile: "native"}, States: hubfixture.States()}); err != nil {
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
			hubfixture.RequireStatus(t, catalog, http.StatusOK)
			var definitions []operatortool.Definition
			hubfixture.Decode(t, catalog, &definitions)
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
				{operatortool.InvitationRevoke, map[string]any{"invitation_id": "invitation_1"}},
				{operatortool.MemberRemove, map[string]any{"member_id": org + "_member"}},
			} {
				t.Run(test.name, func(t *testing.T) {
					definition, _ := operatortool.Lookup(test.name)
					if !slices.ContainsFunc(definitions, func(d operatortool.Definition) bool { return d.Name == test.name }) {
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
							hubfixture.RequireStatus(t, deniedCatalog, http.StatusOK)
							var deniedDefinitions []operatortool.Definition
							hubfixture.Decode(t, deniedCatalog, &deniedDefinitions)
							if slices.ContainsFunc(deniedDefinitions, func(d operatortool.Definition) bool { return d.Name == test.name }) {
								t.Fatal("restricted administrator tool advertised")
							}
							hubfixture.RequireStatus(t, f.request(t, org, "/internal/v1/keys/call", string(body), deniedClaims), http.StatusForbidden)
						}
					}
					response := f.request(t, org, "/internal/v1/keys/call", string(body), keyClaims)
					hubfixture.RequireStatus(t, response, http.StatusOK)
					if definition.Annotations.ReadOnly {
						if !strings.Contains(response.Body.String(), org) {
							t.Fatal("read lost tenant identity")
						}
						return
					}
					var result struct{ Action chat.Action }
					hubfixture.Decode(t, response, &result)
					if result.Action.Status != chat.ActionSucceeded {
						t.Fatalf("administration failed: %s", response.Body)
					}
				})
			}
			if err := f.repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				member, err := r.AccessMember(t.Context(), org+"_member")
				if err == nil && (member.Active || member.Role != "viewer") {
					t.Error("member role did not persist")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			invitation, err := provider.Invitation(t.Context(), "invitation_1")
			if err != nil || invitation.State != "revoked" {
				t.Fatalf("provider invitation = %+v, error = %v", invitation, err)
			}
			provider.Mu.Lock()
			member := provider.Members[org+"_member"]
			provider.Mu.Unlock()
			if member.Role.Slug != "viewer" {
				t.Fatal("provider role change did not complete")
			}
		})
	}
}
