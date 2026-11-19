package conversation_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedArtifactReadPermissions(t *testing.T) {
	t.Parallel()
	hub := openHostedArtifactRoutes(t)
	scope := seedHostedArtifactScope(t, hub)
	ref, publisher, servicePath := scope.ref, scope.publisher, scope.servicePath

	for index, test := range []struct {
		name, role, projectGrant, email, support string
		want                                     int
	}{
		{name: "viewer", role: "viewer", projectGrant: "read", want: http.StatusOK},
		{name: "member read", role: "member", projectGrant: "read", want: http.StatusOK},
		{name: "owner without project grant", role: "owner", want: http.StatusOK},
		{name: "admin without project grant", role: "admin", want: http.StatusOK},
		{name: "member without project", role: "member", want: http.StatusNotFound},
		{name: "staff", role: "member", projectGrant: "write", email: "staff@example.test", want: http.StatusForbidden},
		{name: "support viewer", role: "viewer", projectGrant: "read", support: "support@example.test", want: http.StatusOK},
		{name: "support without project", role: "member", support: "support@example.test", want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			email := test.email
			if email == "" {
				email = "customer@example.test"
			}
			user := hub.User(t, fmt.Sprintf("artifact-reader-%d", index), test.role, email, test.projectGrant, test.support)
			path := hub.Base + "/work-items/" + ref.WorkItemID + "/artifacts/" + ref.ArtifactID + "/access"
			response := hub.Request(t, user, http.MethodPost, path, map[string]int64{"revision": 1})
			hubfixture.RequireStatus(t, response, test.want)
			if test.want != http.StatusOK {
				return
			}
			var grant artifact.Grant
			hubfixture.Decode(t, response, &grant)
			request := artifact.ReadAuthorization{Token: grant.Token, ArtifactID: ref.ArtifactID, Revision: 1}
			hubfixture.RequireStatus(t, hub.Hub.Request(t, http.MethodPost, servicePath+"/authorize", publisher, request), http.StatusNoContent)
			if response.Header().Get("Cache-Control") != "no-store" || grant.ExpiresAt.After(user.Identity.Hosted.ExpiresAt) {
				t.Fatal("artifact grant outlives its session or permits caching")
			}
			if test.support != "" {
				var actor, effective, reason, audit string
				if err := hub.Admin.QueryRowContext(t.Context(), "SELECT actual_actor,effective_user,reason,event || route || project_id FROM hosted_audit WHERE session_id=$1 AND route LIKE '%/authorize' ORDER BY id DESC LIMIT 1", user.Identity.Hosted.SessionID).Scan(&actor, &effective, &reason, &audit); err != nil {
					t.Fatal(err)
				}
				if actor != test.support || effective != user.Identity.Subject || reason != "customer-request" || strings.Contains(audit, grant.Token) || strings.Contains(audit, user.Token) || strings.Contains(audit, ref.ArtifactID) {
					t.Fatal("artifact audit must retain identities without content or capabilities")
				}
			}
		})
	}
}

func seedHostedArtifactForRead(t *testing.T, hub *hubfixture.Hosted) (artifact.Reference, string, string) {
	t.Helper()
	now := hub.Now().UTC().Truncate(time.Microsecond)
	org, project := hub.Primary, hub.Project
	owner := hub.User(t, "artifact-fixture-owner-"+string(project), "owner", "artifact-owner@example.test", "write", "")
	created := hub.Request(t, owner, http.MethodPost, hub.Base+"/work-items", tracker.CreateIssue{
		Mutation: tracker.Mutation{IdempotencyKey: "artifact-issue-" + string(project)}, Title: "Example run", Body: "private-issue-sentinel\n" + hubfixture.IssueBody(), State: "Todo",
	})
	hubfixture.RequireStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, created, &issue)
	item := issue.WorkItemID
	publisher := artifact.NewID("token")
	binding := artifact.Binding{ServiceID: artifact.NewID("service"), Origin: "https://artifacts.example.test", Mode: "customer", PublisherTokenID: artifact.NewID("publisher")}
	ref := artifact.Reference{
		SchemaVersion: 1,
		Scope:         artifact.Scope{OrganizationID: string(org), ProjectID: string(project), WorkItemID: string(item), RunID: artifact.NewID("run"), AttemptID: artifact.NewID("attempt")},
		ServiceID:     binding.ServiceID, ArtifactID: artifact.NewID("artifact"), ManifestID: artifact.NewID("manifest"),
		Revision: 1, SHA256: artifact.Digest([]byte("manifest")), Kind: "log", State: "partial", Availability: "available",
		Bytes: 100, Objects: 1, ExpiresAt: now.Add(time.Hour), ObservedAt: now,
	}
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := hub.Repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		ctx := t.Context()
		credential := accessstore.Credential{
			OrganizationID: org, ID: binding.PublisherTokenID, Name: "Example publisher " + binding.PublisherTokenID, Hash: apikey.HashToken(publisher),
			Scope: "worker", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now,
		}
		if err := w.CreateAccessCredential(ctx, credential); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: org, TokenID: credential.ID, ProjectID: project}); err != nil {
			return err
		}
		if err := w.BindArtifactService(ctx, project, binding); err != nil {
			return err
		}
		_, err := w.AppendArtifactReference(ctx, ref)
		return err
	}); err != nil {
		t.Fatalf("seed artifact publisher and reference: %v", err)
	}
	return ref, publisher, hub.Base + "/artifact-services/" + binding.ServiceID
}
