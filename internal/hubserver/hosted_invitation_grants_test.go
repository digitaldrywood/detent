package hubserver

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
		member        bool
		editAdmin     bool
		editMember    bool
		status        int
	}{
		{name: "no access by default", actorGrant: "write", none: true},
		{name: "existing member receives exact grants", actorGrant: "write", none: true, existing: true},
		{name: "automatic admin grants allow replacement with none", actorGrant: "write", admin: true, existingOther: true, none: true},
		{name: "automatic admin grants allow removal of principal-only grant", actorGrant: "write", admin: true, existingOther: true, principalOnly: true, none: true},
		{name: "automatic admin grants allow replacement with A only", actorGrant: "write", admin: true, existingOther: true},
		{name: "authorized admin replaces with none", actorGrant: "write", admin: true, existingOther: true, actorOther: true, none: true},
		{name: "authorized admin replaces with A only", actorGrant: "write", admin: true, existingOther: true, actorOther: true},
		{name: "authorized replacement rolls back", actorGrant: "write", admin: true, existingOther: true, actorOther: true, rollback: true},
		{name: "admin role edit replaces with none", actorGrant: "write", existingOther: true, actorOther: true, edit: "none", editAdmin: true},
		{name: "admin role edit replaces with A only", actorGrant: "write", existingOther: true, actorOther: true, edit: "read", editAdmin: true},
		{name: "read access", actorGrant: "read"},
		{name: "write access", actorGrant: "write", write: true},
		{name: "read and runners", actorGrant: "runner", runner: true},
		{name: "write and runners", actorGrant: "all", write: true, runner: true},
		{name: "edit to read", actorGrant: "write", write: true, edit: "read"},
		{name: "edit to none", actorGrant: "write", write: true, edit: "none"},
		{name: "owner role edits beyond read grant", actorGrant: "read", edit: "write"},
		{name: "revoke before accept", actorGrant: "write", revoke: true},
		{name: "acceptance rolls back", actorGrant: "write", rollback: true},
		{name: "owner role delegates write beyond read grant", actorGrant: "read", write: true},
		{name: "owner role delegates runner beyond write grant", actorGrant: "write", runner: true},
		{name: "owner role delegates ungranted project"},
		{name: "member cannot delegate write from read", actorGrant: "read", write: true, member: true, status: http.StatusForbidden},
		{name: "member cannot delegate runners from write", actorGrant: "write", runner: true, member: true, status: http.StatusForbidden},
		{name: "member cannot delegate an ungranted project", member: true, status: http.StatusForbidden},
		{name: "member cannot edit to write", actorGrant: "write", edit: "write", editMember: true},
		{name: "foreign project refused", actorGrant: "write", project: "prj_other", status: http.StatusForbidden},
		{name: "duplicate project refused", actorGrant: "write", duplicate: true, status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSecurityFixture(t)
			organization := f.service.accessOrganization()
			actorRole := "owner"
			if test.admin {
				actorRole = "admin"
			} else if test.member {
				actorRole = "member"
			}
			owner := f.user(t, "owner", actorRole, "owner@example.test", test.actorGrant, "")
			if test.actorGrant == "" {
				f.withoutProjectAccess(t, owner)
			}
			if test.actorGrant == "runner" || test.actorGrant == "all" {
				f.grant(t, owner, test.actorGrant == "all", true)
			}
			if test.existing {
				f.user(t, "invitee", "member", "invitee@example.test", "write", "")
			}
			if test.existingOther {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					primary, err := w.ProjectRecord(t.Context(), f.project)
					if err != nil {
						return err
					}
					if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: "prj_second", Name: "Second project", Profile: primary.Core.Profile}, States: primary.States}); err != nil {
						return err
					}
					creator, err := w.AccessMember(t.Context(), owner.identity.Subject)
					if err != nil {
						return err
					}
					return w.InitializeProjectAccess(t.Context(), "prj_second", creator.PrincipalID)
				}); err != nil {
					t.Fatal(err)
				}
				other := f
				other.project = "prj_second"
				invitee := f.user(t, "invitee", "member", "invitee@example.test", "", "")
				other.grant(t, invitee, true, true)
				if test.principalOnly {
					if err := f.service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.DeleteAccessProjectGrant(t.Context(), invitee.identity.Subject, other.project)
					}); err != nil {
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
			if test.project == "prj_other" {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), "org_foreign", corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_foreign", Name: "Foreign organization", CreatedAt: time.Now()}); err != nil {
						return err
					}
					_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_foreign", ID: "prj_other", Name: "foreign-project-sentinel", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true}}})
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), "org_foreign", corestore.Serving, func(r tenantstore.Reader) error {
					row, err := r.ProjectRecord(t.Context(), "prj_other")
					if err == nil && (row.Core.OrganizationID != "org_foreign" || row.Core.Name != "foreign-project-sentinel") {
						return errors.New("foreign grant fixture is unavailable in its own tenant")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			base := "/api/v2/organizations/" + string(organization)
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
					second := f
					second.project = "prj_second"
					second.withoutProjectAccess(t, editor)
				}
				editStatus := http.StatusNoContent
				if test.editMember {
					editor, editStatus = f.user(t, "editor", "member", "editor@example.test", "write", ""), http.StatusForbidden
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
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), `CREATE FUNCTION fail_invitation_grant() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.organization_id='org_security' AND NEW.user_id='user_invitee' THEN RAISE EXCEPTION 'grant insert failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_invitation_grant BEFORE INSERT ON hosted_project_grants FOR EACH ROW EXECUTE FUNCTION fail_invitation_grant()`); err != nil {
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
					{"SELECT count(*) FROM hosted_members WHERE organization_id=$1 AND user_id='user_invitee'", originalCount},
					{"SELECT count(*) FROM hosted_project_grants WHERE organization_id=$1 AND user_id='user_invitee'", originalCount},
					{"SELECT count(*) FROM token_grants WHERE organization_id=$1 AND token_id=(SELECT principal_id FROM hosted_members WHERE organization_id=$1 AND user_id='user_invitee')", originalCount},
					{"SELECT count(*) FROM hosted_invitations WHERE organization_id=$1 AND accepted_user_id='user_invitee'", 0},
				} {
					var count int
					if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), check.query, organization).Scan(&count); err != nil || count != check.want {
						t.Fatalf("failed acceptance changed local access: count=%d want=%d err=%v", count, check.want, err)
					}
				}
				if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					member, err := r.AccessMember(t.Context(), identity.Subject)
					if originalCount == 0 {
						if !errors.Is(err, corestore.ErrNotFound) {
							return errors.Join(errors.New("failed acceptance created a member"), err)
						}
					} else {
						if err != nil {
							return err
						}
						if member.UserID != identity.Subject || member.Role != "member" || !member.Active {
							return errors.New("failed acceptance changed existing member")
						}
						principal, err := r.AccessTokenGrants(t.Context(), member.PrincipalID, "", 2)
						if err != nil {
							return err
						}
						if len(principal) != originalCount || principal[0].ProjectID != "prj_second" {
							return errors.New("failed acceptance changed principal grants")
						}
					}
					projects, err := r.AccessProjectGrants(t.Context(), identity.Subject, "", 2)
					if err != nil {
						return err
					}
					if len(projects) != originalCount || originalCount == 1 && (projects[0].ProjectID != "prj_second" || !projects[0].CanWrite || !projects[0].ManageRunner) {
						return errors.New("failed acceptance changed project grants")
					}
					current, err := r.AccessInvitation(t.Context(), invitation.ID)
					if test.revoke {
						if !errors.Is(err, corestore.ErrNotFound) {
							return errors.Join(errors.New("revoked invitation remains"), err)
						}
						return nil
					}
					if err != nil {
						return err
					}
					if current.AcceptedUserID != "" {
						return errors.New("failed acceptance committed invitation")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if test.revoke {
					return
				}
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "DROP TRIGGER fail_invitation_grant ON hosted_project_grants; DROP FUNCTION fail_invitation_grant()"); err != nil {
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
			var principalProjects []string
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
				member, err := r.AccessMember(t.Context(), identity.Subject)
				if err != nil {
					return err
				}
				rows, err := r.AccessTokenGrants(t.Context(), member.PrincipalID, "", 2)
				if err != nil {
					return err
				}
				for _, row := range rows {
					principalProjects = append(principalProjects, string(row.ProjectID))
				}
				return nil
			}); err != nil {
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
			if len(grants) > 0 {
				writeStatus = http.StatusForbidden
			}
			if len(grants) > 0 && grants[0].Write {
				writeStatus = http.StatusOK
			}
			requireNativeStatus(t, f.request(t, invitee, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Body: nativeContractTestBody, Mutation: tracker.Mutation{IdempotencyKey: "write"}, Title: "New work", State: "Todo"}), writeStatus)
			requireNativeStatus(t, f.request(t, owner, http.MethodPut, path, map[string]any{"grants": grants}), http.StatusNotFound)
		})
	}
}
