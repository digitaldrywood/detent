package access

import (
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedWorkAccess(t *testing.T) {
	t.Parallel()
	f := hubfixture.OpenHosted(t, hubfixture.HostedSecurityConfig(hubfixture.NewHostedProvider()))
	project := f.SeedProject(t, "prj_security", "Example project")
	f.Project, f.Base = project.ID, hubfixture.ProjectBase(f.Primary, project.ID)
	owner := f.User(t, "owner", "owner", "owner@example.test", "write", "")
	for _, test := range []struct {
		name, subject, role, grant, email, revoke string
		allowed, write                            bool
	}{
		{name: "member", subject: "writer", role: "member", grant: "write", allowed: true, write: true},
		{name: "viewer", subject: "viewer", role: "viewer", grant: "write", allowed: true},
		{name: "read grant", subject: "reader", role: "member", grant: "read", allowed: true},
		{name: "no grant", subject: "ungranted", role: "member"},
		{name: "owner without grant", subject: "implicit_owner", role: "owner", allowed: true, write: true},
		{name: "staff", subject: "staff", role: "owner", grant: "write", email: "staff@example.test"},
		{name: "revoked session", subject: "revoked_session", role: "owner", grant: "write", revoke: "session"},
		{name: "revoked membership", subject: "revoked_member", role: "owner", grant: "write", revoke: "member"},
		{name: "revoked grant", subject: "revoked_grant", role: "member", grant: "write", revoke: "grant"},
		{name: "wrong tenant", subject: "foreign", role: "owner", grant: "write", revoke: "tenant"},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := hubtest.Issue(t, f.Repository, f.Config.RunDetails, project, hubtest.IssueOptions{
				Title: "private-issue-sentinel", Body: "private-body-sentinel", Now: f.Now(),
			}).Issue.WorkItemID
			api := f.Base + "/work-items/" + string(item)
			response := f.Request(t, owner, http.MethodPost, api+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "private-change-sentinel", Body: "private-change-body"})
			hubfixture.RequireStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			hubfixture.Decode(t, response, &change)
			email := test.email
			if email == "" {
				email = test.subject + "@example.test"
			}
			user := f.User(t, test.subject, test.role, email, test.grant, "")
			if test.grant == "" {
				f.Revoke(t, user, f.Project)
			}
			switch test.revoke {
			case "session":
				if err := f.Provider.RevokeSession(t.Context(), user.Identity.Hosted.SessionID); err != nil {
					t.Fatal(err)
				}
			case "member":
				if err := f.Provider.RevokeMembership(t.Context(), "membership_"+user.Identity.Subject); err != nil {
					t.Fatal(err)
				}
			case "grant":
				if err := f.Repository.WriteTenant(t.Context(), f.Primary, corestore.Serving, func(w tenantstore.Writer) error {
					return w.DeleteAccessProjectGrant(t.Context(), user.Identity.Subject, f.Project)
				}); err != nil {
					t.Fatal(err)
				}
			case "tenant":
				identity := *user.Identity.Hosted
				identity.OrganizationID = "org_other"
				f.Provider.AddSession(identity)
			}
			organizationAPI := hubfixture.OrganizationBase(f.Primary) + "/work-items/" + string(item)
			for _, path := range []string{api, organizationAPI, api + "/changes/" + change.ID, f.Base + "/work-items?include=work&limit=100"} {
				response := f.Request(t, user, http.MethodGet, path, nil)
				if test.allowed {
					hubfixture.RequireStatus(t, response, http.StatusOK)
					continue
				}
				if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "private-") {
					t.Errorf("revoked read of %s = %d", path, response.Code)
				}
			}
			for _, suffix := range []string{"/comments", "/history", "/attempts", "/changes", "/artifacts"} {
				response := f.Request(t, user, http.MethodGet, api+suffix, nil)
				if test.allowed {
					hubfixture.RequireStatus(t, response, http.StatusOK)
				} else if response.Code == http.StatusOK {
					t.Errorf("revoked API read allowed: %s", suffix)
				}
			}
			response = f.Request(t, user, http.MethodPost, api+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment"}, Body: "discussion"})
			if test.write {
				hubfixture.RequireStatus(t, response, http.StatusOK)
			} else if response.Code == http.StatusOK {
				t.Error("read-only comment allowed")
			}
		})
	}
}
