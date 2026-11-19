package conversation_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type hostedArtifactScope struct {
	hub         *hubfixture.Hosted
	ref         artifact.Reference
	publisher   string
	servicePath string
}

func openHostedArtifactRoutes(t *testing.T) *hubfixture.Hosted {
	t.Helper()
	cfg := hubfixture.HostedSecurityConfig(hubfixture.NewHostedProvider())
	cfg.Hosted.OrganizationID = artifact.NewID("org")
	cfg.Hosted.PublicURL = "https://hub.example.test"
	cfg.Hosted.Plans = nil
	return hubfixture.OpenHosted(t, cfg)
}

func seedHostedArtifactScope(t *testing.T, hub *hubfixture.Hosted) hostedArtifactScope {
	t.Helper()
	hub.Project = tracker.ProjectID(artifact.NewID("prj"))
	hub.Base = hubfixture.ProjectBase(hub.Primary, hub.Project)
	hub.SeedProject(t, hub.Project, string(hub.Project))
	ref, publisher, servicePath := seedHostedArtifactForRead(t, hub)
	return hostedArtifactScope{hub: hub, ref: ref, publisher: publisher, servicePath: servicePath}
}

func (f hostedArtifactScope) accessPath() string {
	return f.hub.Base + "/work-items/" + f.ref.WorkItemID + "/artifacts/" + f.ref.ArtifactID + "/access"
}

func (f hostedArtifactScope) grant(t *testing.T, user hubfixture.HostedUser) artifact.Grant {
	t.Helper()
	response := f.hub.Request(t, user, http.MethodPost, f.accessPath(), map[string]int64{"revision": f.ref.Revision})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var grant artifact.Grant
	hubfixture.Decode(t, response, &grant)
	return grant
}

func (f hostedArtifactScope) authorize(t *testing.T, request artifact.ReadAuthorization, status int) {
	t.Helper()
	response := f.hub.Hub.Request(t, http.MethodPost, f.servicePath+"/authorize", f.publisher, request)
	hubfixture.RequireStatus(t, response, status)
}

func (f hostedArtifactScope) update(t *testing.T, change func(tenantstore.Writer) error) {
	t.Helper()
	if err := f.hub.Repository.WriteTenant(t.Context(), f.hub.Primary, corestore.Serving, change); err != nil {
		t.Fatal(err)
	}
}

func TestHostedArtifactGrantRevocation(t *testing.T) {
	t.Parallel()
	hub := openHostedArtifactRoutes(t)
	for _, test := range []struct {
		name           string
		rejectNewGrant bool
		isolatedHub    bool
		revoke         func(*testing.T, hostedArtifactScope, hubfixture.HostedUser, artifact.Grant)
	}{
		{name: "provider session revoked", revoke: func(t *testing.T, f hostedArtifactScope, user hubfixture.HostedUser, _ artifact.Grant) {
			if err := f.hub.Provider.RevokeSession(t.Context(), user.Identity.Hosted.SessionID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "local session revoked", revoke: func(t *testing.T, f hostedArtifactScope, user hubfixture.HostedUser, _ artifact.Grant) {
			f.update(t, func(w tenantstore.Writer) error {
				return w.RevokeAccessSession(t.Context(), apikey.HashToken(user.Token), f.hub.Now())
			})
		}},
		{name: "session expired", revoke: func(t *testing.T, f hostedArtifactScope, user hubfixture.HostedUser, _ artifact.Grant) {
			identity := *user.Identity.Hosted
			identity.ExpiresAt = f.hub.Now().Add(-time.Second)
			f.hub.Provider.AddSession(identity)
		}},
		{name: "membership revoked", revoke: func(t *testing.T, f hostedArtifactScope, user hubfixture.HostedUser, _ artifact.Grant) {
			memberships, err := f.hub.Provider.Memberships(t.Context(), user.Identity.Subject, user.Identity.Hosted.OrganizationID)
			if err != nil {
				t.Fatal(err)
			}
			for _, membership := range memberships {
				if membership.UserID == user.Identity.Subject {
					if err := f.hub.Provider.RevokeMembership(t.Context(), membership.ID); err != nil {
						t.Fatal(err)
					}
					return
				}
			}
			t.Fatalf("provider membership for %s is missing", user.Identity.Subject)
		}},
		{name: "token grant revoked", rejectNewGrant: true, revoke: func(t *testing.T, f hostedArtifactScope, user hubfixture.HostedUser, _ artifact.Grant) {
			f.update(t, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), user.Identity.Subject)
				if err != nil {
					return err
				}
				return w.DeleteAccessTokenGrant(t.Context(), member.PrincipalID, f.hub.Project)
			})
		}},
		{name: "project grant revoked", revoke: func(t *testing.T, f hostedArtifactScope, user hubfixture.HostedUser, _ artifact.Grant) {
			f.update(t, func(w tenantstore.Writer) error {
				return w.DeleteAccessProjectGrant(t.Context(), user.Identity.Subject, f.hub.Project)
			})
		}},
		{name: "organization changed", revoke: func(t *testing.T, f hostedArtifactScope, user hubfixture.HostedUser, _ artifact.Grant) {
			identity := *user.Identity.Hosted
			identity.OrganizationID = "org_foreign"
			f.hub.Provider.AddSession(identity)
		}},
		{name: "support permission removed", isolatedHub: true, revoke: func(t *testing.T, f hostedArtifactScope, _ hubfixture.HostedUser, _ artifact.Grant) {
			f.hub.Config.Hosted.SupportActors = nil
		}},
		{name: "missing session binding", revoke: func(t *testing.T, f hostedArtifactScope, _ hubfixture.HostedUser, grant artifact.Grant) {
			if _, err := f.hub.Admin.ExecContext(t.Context(), "UPDATE artifact_grants SET hosted_session_hash='' WHERE organization_id=$1 AND token_hash=$2", f.hub.Primary, apikey.HashToken(grant.Token)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "different user session", revoke: func(t *testing.T, f hostedArtifactScope, _ hubfixture.HostedUser, grant artifact.Grant) {
			other := f.hub.User(t, artifact.NewID("artifact-viewer"), "viewer", "other@example.test", "read", "")
			if _, err := f.hub.Admin.ExecContext(t.Context(), "UPDATE artifact_grants SET hosted_session_hash=$3 WHERE organization_id=$1 AND token_hash=$2", f.hub.Primary, apikey.HashToken(grant.Token), apikey.HashToken(other.Token)); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := hub
			if test.isolatedHub {
				current = openHostedArtifactRoutes(t)
			}
			f := seedHostedArtifactScope(t, current)
			user := current.User(t, artifact.NewID("artifact-reader"), "member", "customer@example.test", "write", "support@example.test")
			grant := f.grant(t, user)
			request := artifact.ReadAuthorization{Token: grant.Token, ArtifactID: f.ref.ArtifactID, Revision: f.ref.Revision}
			f.authorize(t, request, http.StatusNoContent)
			test.revoke(t, f, user, grant)
			f.authorize(t, request, http.StatusNotFound)
			if test.rejectNewGrant {
				response := current.Request(t, user, http.MethodPost, f.accessPath(), map[string]int64{"revision": f.ref.Revision})
				hubfixture.RequireStatus(t, response, http.StatusNotFound)
			}
		})
	}
	t.Run("sessionless keys retain authority", func(t *testing.T) {
		f := seedHostedArtifactScope(t, hub)
		member := hub.User(t, artifact.NewID("artifact-member"), "member", "customer@example.test", "write", "")
		for _, kind := range []string{"personal", "service"} {
			t.Run(kind, func(t *testing.T) {
				token, err := apikey.GenerateToken()
				if err != nil {
					t.Fatal(err)
				}
				grantToken, err := apikey.GenerateToken()
				if err != nil {
					t.Fatal(err)
				}
				now := hub.Now()
				f.update(t, func(w tenantstore.Writer) error {
					ctx := t.Context()
					credential := accessstore.Credential{OrganizationID: hub.Primary, ID: artifact.NewID("reader"), Name: "Example artifact reader " + kind, Hash: apikey.HashToken(token), Scope: "operator", NativeOnly: true, Kind: kind, KeyScope: "read", ProjectAccess: "selected", CreatedAt: now, UpdatedAt: now, ExpiresAt: new(now.Add(time.Hour))}
					if kind == "personal" {
						identity, err := w.AccessMember(ctx, member.Identity.Subject)
						if err != nil {
							return err
						}
						credential.UserID, credential.MembershipID = identity.UserID, identity.MembershipID
					}
					if err := w.CreateAccessCredential(ctx, credential); err != nil {
						return err
					}
					if err := w.SetAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: hub.Primary, TokenID: credential.ID, ProjectID: hub.Project}); err != nil {
						return err
					}
					return w.CreateArtifactReadGrant(ctx, artifactstore.ReadGrant{ProjectID: hub.Project, TokenHash: apikey.HashToken(grantToken), PrincipalID: credential.ID, ServiceID: f.ref.ServiceID, ArtifactID: f.ref.ArtifactID, Revision: f.ref.Revision, ExpiresAt: now.Add(time.Minute)}, now)
				})
				f.authorize(t, artifact.ReadAuthorization{Token: grantToken, ArtifactID: f.ref.ArtifactID, Revision: f.ref.Revision}, http.StatusNoContent)
			})
		}
	})
}

func TestHostedArtifactPublisherBoundary(t *testing.T) {
	t.Parallel()
	hub := openHostedArtifactRoutes(t)
	f := seedHostedArtifactScope(t, hub)
	user := hub.User(t, artifact.NewID("artifact-viewer"), "viewer", "customer@example.test", "read", "")
	identity := *user.Identity.Hosted
	identity.ExpiresAt = hub.Now().Add(30 * time.Second)
	hub.Provider.AddSession(identity)
	withoutCSRF := httptest.NewRequestWithContext(t.Context(), http.MethodPost, f.accessPath(), strings.NewReader(`{"revision":1}`))
	withoutCSRF.Header.Set("Content-Type", "application/json")
	withoutCSRF.AddCookie(&http.Cookie{Name: hubfixture.HostedCookie, Value: user.Token})
	refused := httptest.NewRecorder()
	hub.Handler().ServeHTTP(refused, withoutCSRF)
	hubfixture.RequireStatus(t, refused, http.StatusForbidden)
	grant := f.grant(t, user)
	if grant.ExpiresAt.After(identity.ExpiresAt) {
		t.Fatal("grant must preserve the shorter provider expiry")
	}
	request := artifact.ReadAuthorization{Token: grant.Token, ArtifactID: f.ref.ArtifactID, Revision: f.ref.Revision}
	for _, test := range []struct {
		name   string
		mutate func(*artifact.ReadAuthorization)
	}{
		{"wrong revision", func(value *artifact.ReadAuthorization) { value.Revision++ }},
		{"wrong artifact", func(value *artifact.ReadAuthorization) { value.ArtifactID = artifact.NewID("artifact") }},
		{"invalid token", func(value *artifact.ReadAuthorization) { value.Token = "invalid" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := request
			test.mutate(&invalid)
			f.authorize(t, invalid, http.StatusNotFound)
		})
	}
	for _, test := range []struct{ name, method, path string }{
		{"project content", http.MethodGet, hub.Base + "/work-items"},
		{"artifact references", http.MethodGet, hub.Base + "/work-items/" + f.ref.WorkItemID + "/artifacts"},
		{"artifact read grants", http.MethodPost, f.accessPath()},
		{"wrong method", http.MethodGet, f.servicePath + "/authorize"},
		{"wrong service", http.MethodPost, hub.Base + "/artifact-services/service_foreign/authorize"},
		{"wrong organization", http.MethodPost, strings.Replace(f.servicePath, string(hub.Primary), "org_foreign", 1) + "/authorize"},
		{"wrong project", http.MethodPost, strings.Replace(f.servicePath, string(hub.Project), "prj_foreign", 1) + "/authorize"},
		{"service binding", http.MethodPut, f.servicePath},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := hub.Hub.Request(t, test.method, test.path, f.publisher, request)
			if response.Code < http.StatusBadRequest {
				t.Fatalf("publisher gained access: status %d", response.Code)
			}
			if body := response.Body.String(); strings.Contains(body, "private-issue-sentinel") || strings.Contains(body, grant.Token) {
				t.Fatal("publisher denial leaked customer content or a capability")
			}
		})
	}
	for _, test := range []struct {
		name      string
		statement string
	}{
		{"unscoped token", "UPDATE api_tokens SET native_only=false WHERE organization_id=$1 AND id=$2"},
		{"admin token", "UPDATE api_tokens SET scope='admin' WHERE organization_id=$1 AND id=$2"},
		{"unbound token", "UPDATE artifact_services SET publisher_token_id=(SELECT principal_id FROM hosted_members WHERE organization_id=$1 ORDER BY principal_id LIMIT 1) WHERE organization_id=$1 AND publisher_token_id=$2"},
		{"revoked project", "DELETE FROM token_grants WHERE organization_id=$1 AND token_id=$2"},
		{"revoked publisher", "UPDATE api_tokens SET revoked_at=created_at WHERE organization_id=$1 AND id=$2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := seedHostedArtifactScope(t, hub)
			user := hub.User(t, artifact.NewID("artifact-viewer"), "viewer", "customer@example.test", "read", "")
			grant := f.grant(t, user)
			request := artifact.ReadAuthorization{Token: grant.Token, ArtifactID: f.ref.ArtifactID, Revision: f.ref.Revision}
			f.authorize(t, request, http.StatusNoContent)
			credential, err := hub.Credential(t, hub.Primary, f.publisher)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := hub.Admin.ExecContext(t.Context(), test.statement, hub.Primary, credential.ID); err != nil {
				t.Fatal(err)
			}
			response := hub.Hub.Request(t, http.MethodPost, f.servicePath+"/authorize", f.publisher, request)
			if response.Code < http.StatusBadRequest {
				t.Fatalf("inactive publisher gained access: status %d", response.Code)
			}
		})
	}
}
