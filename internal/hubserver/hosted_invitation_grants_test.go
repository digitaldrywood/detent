package hubserver

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedInvitationGrants(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		actorGrant    string
		write         bool
		runner        bool
		none          bool
		edit          string
		revoke        bool
		rollback      bool
		project       string
		duplicate     bool
		existing      bool
		existingOther bool
		principalOnly bool
		actorOther    bool
		admin         bool
		editAdmin     bool
		status        int
	}{
		{name: "no access by default", actorGrant: "write", none: true},
		{name: "existing member receives exact grants", actorGrant: "write", none: true, existing: true},
		{name: "restricted admin cannot replace with none", actorGrant: "write", admin: true, existingOther: true, none: true, status: http.StatusForbidden},
		{name: "restricted admin cannot remove principal-only grant", actorGrant: "write", admin: true, existingOther: true, principalOnly: true, none: true, status: http.StatusForbidden},
		{name: "restricted admin cannot replace with A only", actorGrant: "write", admin: true, existingOther: true, status: http.StatusForbidden},
		{name: "authorized admin replaces with none", actorGrant: "write", admin: true, existingOther: true, actorOther: true, none: true},
		{name: "authorized admin replaces with A only", actorGrant: "write", admin: true, existingOther: true, actorOther: true},
		{name: "authorized replacement rolls back", actorGrant: "write", admin: true, existingOther: true, actorOther: true, rollback: true},
		{name: "restricted edit cannot replace with none", actorGrant: "write", existingOther: true, actorOther: true, edit: "none", editAdmin: true},
		{name: "restricted edit cannot replace with A only", actorGrant: "write", existingOther: true, actorOther: true, edit: "read", editAdmin: true},
		{name: "read access", actorGrant: "read"},
		{name: "write access", actorGrant: "write", write: true},
		{name: "read and runners", actorGrant: "runner", runner: true},
		{name: "write and runners", actorGrant: "all", write: true, runner: true},
		{name: "edit to read", actorGrant: "write", write: true, edit: "read"},
		{name: "edit to none", actorGrant: "write", write: true, edit: "none"},
		{name: "edit escalation refused", actorGrant: "read", edit: "write"},
		{name: "revoke before accept", actorGrant: "write", revoke: true},
		{name: "acceptance rolls back", actorGrant: "write", rollback: true},
		{name: "write escalation refused", actorGrant: "read", write: true, status: http.StatusForbidden},
		{name: "runner escalation refused", actorGrant: "write", runner: true, status: http.StatusForbidden},
		{name: "ungranted project refused", status: http.StatusForbidden},
		{name: "foreign project refused", actorGrant: "write", project: "prj_other", status: http.StatusForbidden},
		{name: "duplicate project refused", actorGrant: "write", duplicate: true, status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			actorRole := "owner"
			if test.admin {
				actorRole = "admin"
			}
			owner := f.user(t, "owner", actorRole, "owner@example.test", test.actorGrant, "")
			if test.actorGrant == "runner" || test.actorGrant == "all" {
				f.grant(t, owner, test.actorGrant == "all", true)
			}
			if test.existing {
				f.user(t, "invitee", "member", "invitee@example.test", "write", "")
			}
			if test.existingOther {
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,github_repository_enabled) SELECT 'prj_second',organization_id,'Second project',profile,states_json,created_at,0 FROM projects WHERE id=?", f.project); err != nil {
					t.Fatal(err)
				}
				other := f
				other.project = "prj_second"
				invitee := f.user(t, "invitee", "member", "invitee@example.test", "", "")
				other.grant(t, invitee, true, true)
				if test.principalOnly {
					if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE user_id=? AND project_id=?", invitee.identity.Subject, other.project); err != nil {
						t.Fatal(err)
					}
				}
				if test.actorOther {
					other.grant(t, owner, false, false)
				}
			}
			project := string(f.project)
			if test.project != "" {
				project = test.project
			}
			grants := []hostedMemberGrant{}
			if !test.none {
				grants = append(grants, hostedMemberGrant{ProjectID: project, Write: test.write, Runner: test.runner})
			}
			if test.duplicate {
				grants = append(grants, grants[0])
			}
			base := "/api/v2/organizations/org_security"
			status := test.status
			if status == 0 {
				status = http.StatusCreated
			}
			input := map[string]any{"email": "invitee@example.test", "role": "member", "idempotency_key": "invite", "grants": grants}
			if test.none {
				delete(input, "grants")
			}
			response := f.request(t, owner, http.MethodPost, base+"/members/invitations", input)
			requireNativeStatus(t, response, status)
			if status != http.StatusCreated {
				if len(f.provider.invitations) != 0 {
					t.Fatal("refused request sent a provider invitation")
				}
				if test.existingOther {
					var count int
					want := 1
					if test.principalOnly {
						want = 0
					}
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_project_grants WHERE user_id='user_invitee' AND project_id='prj_second' AND can_write=1 AND manage_runner=1").Scan(&count); err != nil || count != want {
						t.Fatalf("refused replacement changed existing access: count=%d want=%d err=%v", count, want, err)
					}
					if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM token_grants WHERE token_id=(SELECT principal_id FROM hosted_members WHERE user_id='user_invitee') AND project_id='prj_second'").Scan(&count); err != nil || count != 1 {
						t.Fatalf("refused replacement changed principal access: count=%d err=%v", count, err)
					}
				}
				return
			}
			var invitation hostedInvitationView
			decodeHubResponse(t, response, &invitation)
			if !slices.Equal(invitation.Grants, grants) {
				t.Fatalf("created grants = %v, want %v", invitation.Grants, grants)
			}
			response = f.request(t, owner, http.MethodPost, base+"/members/invitations", input)
			requireNativeStatus(t, response, http.StatusCreated)
			if len(f.provider.invitations) != 1 {
				t.Fatal("retry created another invitation")
			}
			if len(grants) > 0 {
				input["grants"] = []hostedMemberGrant{}
				requireNativeStatus(t, f.request(t, owner, http.MethodPost, base+"/members/invitations", input), http.StatusConflict)
			}
			path := base + "/members/invitations/" + invitation.ID
			if test.edit != "" {
				updated := []hostedMemberGrant{}
				if test.edit != "none" {
					updated = append(updated, hostedMemberGrant{ProjectID: project, Write: test.edit == "write"})
				}
				editor := owner
				if test.editAdmin {
					editor = f.user(t, "admin", "admin", "admin@example.test", "write", "")
				}
				editStatus := http.StatusNoContent
				if test.edit == "write" || test.editAdmin {
					editStatus = http.StatusForbidden
				} else {
					grants = updated
				}
				requireNativeStatus(t, f.request(t, editor, http.MethodPut, path, map[string]any{"grants": updated}), editStatus)
			}
			var pending hostedMembersResponse
			decodeHubResponse(t, f.request(t, owner, http.MethodGet, base+"/members", nil), &pending)
			if len(pending.Invitations) != 1 || !slices.Equal(pending.Invitations[0].Grants, grants) {
				t.Fatalf("pending invitations = %#v", pending.Invitations)
			}
			if test.revoke {
				requireNativeStatus(t, f.request(t, owner, http.MethodDelete, path, nil), http.StatusNoContent)
			}
			if test.rollback {
				if _, err := f.service.database.db.ExecContext(t.Context(), `CREATE TRIGGER fail_invitation_grant BEFORE INSERT ON hosted_project_grants WHEN NEW.user_id='user_invitee' BEGIN SELECT RAISE(ABORT,'grant insert failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now()
			identity := auth.Identity{Subject: "user_invitee", Email: "invitee@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: "user_invitee", OrganizationID: "org_provider", SessionID: "session_invitee", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}}
			err := f.service.acceptHostedInvitationFor(t.Context(), identity, invitation.ID)
			if test.revoke || test.rollback {
				if err == nil {
					t.Fatal("acceptance unexpectedly succeeded")
				}
				originalCount := 0
				if test.existingOther {
					originalCount = 1
				}
				for _, check := range []struct {
					query string
					want  int
				}{
					{"SELECT count(*) FROM hosted_members WHERE user_id='user_invitee'", originalCount},
					{"SELECT count(*) FROM hosted_project_grants WHERE user_id='user_invitee'", originalCount},
					{"SELECT count(*) FROM token_grants WHERE token_id=(SELECT principal_id FROM hosted_members WHERE user_id='user_invitee')", originalCount},
					{"SELECT count(*) FROM hosted_invitations WHERE accepted_user_id='user_invitee'", 0},
				} {
					var count int
					if err := f.service.database.db.QueryRowContext(t.Context(), check.query).Scan(&count); err != nil || count != check.want {
						t.Fatalf("failed acceptance changed local access: count=%d want=%d err=%v", count, check.want, err)
					}
				}
				if test.revoke {
					return
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "DROP TRIGGER fail_invitation_grant"); err != nil {
					t.Fatal(err)
				}
				if err := f.service.acceptHostedInvitationFor(t.Context(), identity, invitation.ID); err != nil {
					t.Fatalf("retry after provider acceptance: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			f.provider.mu.Lock()
			f.provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
			f.provider.mu.Unlock()
			token, _, err := f.service.hostedSessions.CreateIdentitySession(t.Context(), identity)
			if err != nil {
				t.Fatal(err)
			}
			invitee := hostedSecurityUser{identity: identity, token: token}
			var members hostedMembersResponse
			decodeHubResponse(t, f.request(t, invitee, http.MethodGet, base+"/members", nil), &members)
			if len(members.Members) != 1 || !slices.Equal(members.Members[0].Grants, grants) {
				t.Fatalf("accepted member = %#v, want grants %v", members.Members, grants)
			}
			rows, err := f.service.database.db.QueryContext(t.Context(), "SELECT project_id FROM token_grants WHERE token_id=(SELECT principal_id FROM hosted_members WHERE user_id='user_invitee') ORDER BY project_id")
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var principalProjects []string
			for rows.Next() {
				var project string
				if err := rows.Scan(&project); err != nil {
					t.Fatal(err)
				}
				principalProjects = append(principalProjects, project)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			if len(principalProjects) != len(grants) || len(grants) > 0 && principalProjects[0] != grants[0].ProjectID {
				t.Fatalf("principal projects = %v, want grants %v", principalProjects, grants)
			}
			var projects []hostedProjectView
			decodeHubResponse(t, f.request(t, invitee, http.MethodGet, base+"/projects", nil), &projects)
			if len(projects) != len(grants) || len(projects) > 0 && projects[0].CanWrite != grants[0].Write {
				t.Fatalf("visible projects = %#v, want grants %v", projects, grants)
			}
			writeStatus := http.StatusNotFound
			if len(grants) > 0 && grants[0].Write {
				writeStatus = http.StatusOK
			}
			requireNativeStatus(t, f.request(t, invitee, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "write"}, Title: "New work", State: "Todo"}), writeStatus)
			requireNativeStatus(t, f.request(t, owner, http.MethodPut, path, map[string]any{"grants": grants}), http.StatusNotFound)
		})
	}
}
