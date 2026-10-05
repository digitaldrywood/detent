package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// These commands are shared with dashboard adapters. They own persistence and
// provider effects; MCP only supplies typed input and trusted mutation context.
func (s *Service) hostedMembersFor(ctx context.Context, credential apiCredential) (hostedMembersResponse, error) {
	manage := credential.HostedRole == "owner" || credential.HostedRole == "admin"
	memberships, err := s.config.Hosted.Provider.Memberships(ctx, "", credential.Hosted.OrganizationID)
	if err != nil {
		return hostedMembersResponse{}, err
	}
	emails, err := s.hostedMemberEmails(ctx)
	if err != nil {
		return hostedMembersResponse{}, err
	}
	grants, err := s.hostedMemberGrants(ctx)
	if err != nil {
		return hostedMembersResponse{}, err
	}
	response := hostedMembersResponse{Members: []hostedMemberView{}, Invitations: []hostedInvitationView{}}
	for _, member := range memberships {
		if member.OrganizationID != credential.Hosted.OrganizationID || member.Status != "active" || !manage && member.UserID != credential.Hosted.Subject {
			continue
		}
		// Local removal/downgrade is authoritative even before provider propagation.
		var active bool
		var role string
		err := s.database.db.QueryRowContext(ctx, "SELECT active,role FROM hosted_members WHERE user_id=?", member.UserID).Scan(&active, &role)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return response, err
		}
		if err == nil && !active {
			continue
		}
		if err == nil {
			member.Role.Slug = lesserHostedRole(member.Role.Slug, role)
		}
		view, err := s.hostedMemberIdentity(ctx, member, emails)
		if err != nil {
			return response, err
		}
		if list, ok := grants[member.UserID]; ok {
			view.Grants = list
		}
		response.Members = append(response.Members, view)
	}
	sortHostedMembers(response.Members)
	if manage {
		response.Invitations, err = s.hostedPendingInvitations(ctx)
	}
	return response, err
}

func (s *Service) inviteHostedMemberFor(ctx context.Context, credential apiCredential, email, role, key string, grants []hostedMemberGrant) (hostedInvitationView, error) {
	if credential.Hosted == nil || credential.HostedRole != "owner" && credential.HostedRole != "admin" || !auth.ValidOrganizationRole(role) || role == "owner" && credential.HostedRole != "owner" {
		return hostedInvitationView{}, operatortool.ErrAccessDenied
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") || hostedEmailListed(s.config.Hosted.StaffEmails, email) {
		return hostedInvitationView{}, operatortool.ErrInvalidArguments
	}
	grants = append([]hostedMemberGrant{}, grants...)
	sort.Slice(grants, func(i, j int) bool { return grants[i].ProjectID < grants[j].ProjectID })
	if err := s.validateHostedInvitationGrants(ctx, s.database.db, credential, email, grants); err != nil {
		return hostedInvitationView{}, err
	}
	command := hostedCommand{actor: credential.ID, operation: "POST /api/v2/organizations/" + s.config.Hosted.OrganizationID + "/members/invitations", key: key, input: struct {
		Email, Role string
		Grants      []hostedMemberGrant `json:",omitempty"`
	}{email, role, grants}}
	claimed, raw, err := s.claimHostedOperation(ctx, command)
	if err != nil {
		return hostedInvitationView{}, err
	}
	if !claimed {
		var view hostedInvitationView
		err := json.Unmarshal(raw, &view)
		return view, err
	}
	reserved, err := s.reserveHostedInvitationSeat(ctx, email)
	if err != nil {
		s.abandonHostedOperation(ctx, command)
		return hostedInvitationView{}, err
	}
	view, err := s.sendReservedHostedInvitationFor(ctx, credential, email, role, reserved, grants)
	if err != nil {
		return hostedInvitationView{}, err
	}

	if _, err := s.completeHostedOperation(ctx, command, view); err != nil {
		return hostedInvitationView{}, err
	}
	return view, nil
}

func (s *Service) removeHostedMemberFor(ctx context.Context, credential apiCredential, id string) error {
	member, err := s.hostedManagedMemberFor(ctx, credential, id, true)
	if err != nil {
		return err
	}
	if err := s.revokeHostedMemberLocally(ctx, member.UserID); err != nil {
		return err
	}
	return s.config.Hosted.Provider.RevokeMembership(ctx, member.ID)
}

func (s *Service) changeHostedRoleFor(ctx context.Context, credential apiCredential, id, role string) (hostedMemberView, error) {
	if !auth.ValidOrganizationRole(role) || role == "owner" && credential.HostedRole != "owner" {
		return hostedMemberView{}, operatortool.ErrAccessDenied
	}
	member, err := s.hostedManagedMemberFor(ctx, credential, id, role != "owner")
	if err != nil {
		return hostedMemberView{}, err
	}
	if err := s.config.Hosted.Provider.SetMembershipRole(ctx, member.ID, role); err != nil {
		return hostedMemberView{}, err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return hostedMemberView{}, err
	}
	defer tx.Rollback()
	if role != member.Role.Slug && lesserHostedRole(role, member.Role.Slug) == role {
		if _, err := tx.ExecContext(ctx, "DELETE FROM operator_connections WHERE organization_id = ? AND json_extract(identity_json, '$.principal_id') = (SELECT principal_id FROM hosted_members WHERE user_id = ?)", s.config.Hosted.OrganizationID, member.UserID); err != nil {
			return hostedMemberView{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE hosted_members SET role=?,updated_at=? WHERE user_id=?", role, formatHubTime(s.config.now()), member.UserID); err != nil {
		return hostedMemberView{}, err
	}
	if err := tx.Commit(); err != nil {
		return hostedMemberView{}, err
	}
	return s.hostedMemberResponse(ctx, member)
}

func (s *Service) changeHostedGrantFor(ctx context.Context, credential apiCredential, id, project string, write, runner, revoke bool) (hostedMemberView, error) {
	member, err := s.hostedMemberByID(ctx, credential, id)
	if err != nil {
		return hostedMemberView{}, nativeNotFound()
	}
	if !hostedSafeID(member.UserID) || !hostedSafeID(project) {
		return hostedMemberView{}, nativeInvalid("Select a member and project")
	}
	if err := s.hostedGrant(ctx, credential, member.UserID, project, write, runner, revoke); err != nil {
		return hostedMemberView{}, err
	}
	return s.hostedMemberResponse(ctx, member)
}

func (s *Service) revokeHostedInvitationFor(ctx context.Context, id string) error {
	var email string
	if err := s.database.db.QueryRowContext(ctx, "SELECT email FROM hosted_invitations WHERE id=? AND organization_id=? AND accepted_user_id=''", id, s.config.Hosted.OrganizationID).Scan(&email); err != nil {
		return err
	}
	if err := auth.RevokeInvitationID(ctx, s.config.Hosted.Provider, id); err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, "DELETE FROM hosted_invitations WHERE id=? AND organization_id=? AND accepted_user_id='' RETURNING email", id, s.config.Hosted.OrganizationID).Scan(&email); err != nil {
		return err
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM hosted_invitations WHERE email=? AND accepted_user_id=''", email).Scan(&remaining); err != nil {
		return err
	}
	if remaining == 0 {
		if _, err := tx.ExecContext(ctx, "DELETE FROM hosted_member_reservations WHERE email=?", email); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Service) resendHostedInvitationFor(ctx context.Context, credential apiCredential, id string) error {
	view, err := s.pendingHostedInvitationFor(ctx, s.database.db, credential, id)
	if err != nil {
		return err
	}
	if err := s.validateHostedInvitationGrants(ctx, s.database.db, credential, view.Email, view.Grants); err != nil {
		return err
	}
	return auth.ResendInvitationID(ctx, s.config.Hosted.Provider, id)
}

func (s *Service) pendingHostedInvitationFor(ctx context.Context, query nativeQueryer, credential apiCredential, id string) (hostedInvitationView, error) {
	var view hostedInvitationView
	var grants string
	var providerID string
	err := query.QueryRowContext(ctx, "SELECT provider_id FROM hosted_tenant WHERE singleton=1").Scan(&providerID)
	if err != nil || credential.Hosted == nil || credential.Hosted.OrganizationID != providerID || credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return view, operatortool.ErrAccessDenied
	}
	if err := query.QueryRowContext(ctx, "SELECT id,email,role,created_at,expires_at,grants_json FROM hosted_invitations WHERE id=? AND organization_id=? AND accepted_user_id=''", id, s.config.Hosted.OrganizationID).Scan(&view.ID, &view.Email, &view.Role, &view.CreatedAt, &view.ExpiresAt, &grants); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return view, nativeNotFound()
		}
		return view, err
	}
	if view.Role == "owner" && credential.HostedRole != "owner" {
		return view, &nativeError{Code: "forbidden", Message: "You cannot change owner invitations", status: 403}
	}
	invitation, err := auth.LookupInvitationID(ctx, s.config.Hosted.Provider, id)
	if err != nil {
		return view, operatoradmin.ErrUnavailable
	}
	if invitation.ID != id || invitation.OrganizationID != credential.Hosted.OrganizationID || !strings.EqualFold(invitation.Email, view.Email) || invitation.State != "pending" || !invitation.ExpiresAt.After(s.config.now()) {
		return view, nativeNotFound()
	}
	if view.ExpiresAt != "" {
		expiry, err := parseTimeValue(view.ExpiresAt)
		if err != nil || !expiry.After(s.config.now()) {
			return view, nativeNotFound()
		}
		if invitation.ExpiresAt.Before(expiry) {
			view.ExpiresAt = formatHubTime(invitation.ExpiresAt)
		}
	} else {
		view.ExpiresAt = formatHubTime(invitation.ExpiresAt)
	}
	if err := json.Unmarshal([]byte(grants), &view.Grants); err != nil {
		return view, operatoradmin.ErrUnavailable
	}
	return view, nil
}

func (s *Service) sendReservedHostedInvitationFor(ctx context.Context, credential apiCredential, email, role string, reserved bool, grants []hostedMemberGrant) (view hostedInvitationView, resultErr error) {
	grants = append([]hostedMemberGrant{}, grants...)
	if err := s.validateHostedInvitationGrants(ctx, s.database.db, credential, email, grants); err != nil {
		return hostedInvitationView{}, err
	}
	invitation, err := s.config.Hosted.Provider.Invite(ctx, credential.Hosted.OrganizationID, email, role, credential.Hosted.Subject)
	if err != nil || invitation.OrganizationID != credential.Hosted.OrganizationID || !strings.EqualFold(invitation.Email, email) || invitation.State != "pending" {
		if reserved {
			if releaseErr := s.releaseHostedInvitation(ctx, email); releaseErr != nil {
				return hostedInvitationView{}, operatoradmin.ErrUnavailable
			}
		}
		return hostedInvitationView{}, errors.Join(mutation.ErrUncertain, &nativeError{Code: "unavailable", Message: "The invitation could not be sent", status: 503})
	}
	created := formatHubTime(s.config.now())
	view = hostedInvitationView{ID: invitation.ID, Email: email, Role: role, CreatedAt: created, Grants: grants}
	if !invitation.ExpiresAt.IsZero() {
		view.ExpiresAt = formatHubTime(invitation.ExpiresAt)
	}
	encoded, err := json.Marshal(grants)
	if err != nil {
		return hostedInvitationView{}, err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return hostedInvitationView{}, err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := s.recheckHostedMutation(ctx, tx, nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential, requireHostedAdmin: true}); err != nil {
		return hostedInvitationView{}, err
	}
	if err := s.validateHostedInvitationGrants(ctx, tx, credential, email, grants); err != nil {
		return hostedInvitationView{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_invitations(id,email,organization_id,role,created_at,expires_at,grants_json) VALUES (?,?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, invitation.ID, email, s.config.Hosted.OrganizationID, role, created, view.ExpiresAt, string(encoded)); err != nil {
		return hostedInvitationView{}, err
	}
	if err := tx.Commit(); err != nil {
		return hostedInvitationView{}, err
	}
	return view, nil
}

func (s *Service) validateHostedGrants(ctx context.Context, query nativeQueryer, credential apiCredential, grants []hostedMemberGrant) error {
	if credential.Hosted == nil || credential.HostedRole != "owner" && credential.HostedRole != "admin" {
		return &nativeError{Code: "forbidden", Message: "You cannot manage project grants", status: 403}
	}
	var role string
	if err := query.QueryRowContext(ctx, "SELECT role FROM hosted_members WHERE user_id=? AND active=1", credential.Hosted.Subject).Scan(&role); err != nil || role != "owner" && role != "admin" {
		return &nativeError{Code: "forbidden", Message: "You cannot manage project grants", status: 403}
	}
	seen := map[string]bool{}
	for _, grant := range grants {
		if !hostedSafeID(grant.ProjectID) || seen[grant.ProjectID] {
			return nativeInvalid("Select each project only once")
		}
		seen[grant.ProjectID] = true
		if credential.HostedKeyScope != "" {
			condition, args := credential.projectGrantSQL("p.organization_id", "p.id")
			args = append([]any{s.config.Hosted.OrganizationID, grant.ProjectID}, args...)
			var count int
			if err := query.QueryRowContext(ctx, "SELECT count(*) FROM projects p WHERE p.organization_id=? AND p.id=? AND ("+condition+")", args...).Scan(&count); err != nil || count != 1 {
				return operatortool.ErrAccessDenied
			}
		}
		var count int
		err := query.QueryRowContext(ctx, `SELECT count(*) FROM projects p JOIN hosted_project_grants g ON g.project_id=p.id AND g.organization_id=p.organization_id
WHERE p.organization_id=? AND p.id=? AND g.user_id=? AND (?=0 OR g.can_write=1) AND (?=0 OR g.manage_runner=1)`, s.config.Hosted.OrganizationID, grant.ProjectID, credential.Hosted.Subject, grant.Write, grant.Runner).Scan(&count)
		if err != nil {
			return err
		}
		if count != 1 {
			return &nativeError{Code: "forbidden", Message: "You can only grant project access that you hold", status: 403}
		}
	}
	return nil
}

func (s *Service) validateHostedInvitationGrants(ctx context.Context, query nativeQueryer, credential apiCredential, email string, grants []hostedMemberGrant) error {
	if err := s.validateHostedGrants(ctx, query, credential, grants); err != nil {
		return err
	}
	rows, err := query.QueryContext(ctx, `SELECT g.project_id FROM hosted_members m JOIN hosted_project_grants g ON g.user_id=m.user_id WHERE lower(m.email)=?
UNION SELECT g.project_id FROM hosted_members m JOIN token_grants g ON g.token_id=m.principal_id WHERE lower(m.email)=?`, email, email)
	if err != nil {
		return err
	}
	defer rows.Close()
	selected := make(map[string]bool, len(grants))
	for _, grant := range grants {
		selected[grant.ProjectID] = true
	}
	var revoked []hostedMemberGrant
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return err
		}
		if !selected[project] {
			revoked = append(revoked, hostedMemberGrant{ProjectID: project})
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	return s.validateHostedGrants(ctx, query, credential, revoked)
}

func (s *Service) editHostedInvitationFor(ctx context.Context, credential apiCredential, id string, grants []hostedMemberGrant) (resultErr error) {
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := s.recheckHostedMutation(ctx, tx, nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: credential, requireHostedAdmin: true}); err != nil {
		return &nativeError{Code: "forbidden", Message: "You cannot edit invitations", status: 403}
	}
	view, err := s.pendingHostedInvitationFor(ctx, tx, credential, id)
	if err != nil {
		return err
	}
	if err := s.validateHostedInvitationEdit(ctx, tx, credential, view, grants); err != nil {
		return err
	}
	if grants == nil {
		grants = []hostedMemberGrant{}
	}
	encoded, err := json.Marshal(grants)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE hosted_invitations SET grants_json=? WHERE id=? AND organization_id=? AND accepted_user_id=''", string(encoded), id, s.config.Hosted.OrganizationID)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count != 1 {
		return nativeNotFound()
	}
	return tx.Commit()
}

func (s *Service) validateHostedInvitationEdit(ctx context.Context, query nativeQueryer, credential apiCredential, view hostedInvitationView, grants []hostedMemberGrant) error {
	previous := make([]hostedMemberGrant, 0, len(view.Grants))
	for _, grant := range view.Grants {
		previous = append(previous, hostedMemberGrant{ProjectID: grant.ProjectID})
	}
	if err := s.validateHostedGrants(ctx, query, credential, previous); err != nil {
		return err
	}
	return s.validateHostedInvitationGrants(ctx, query, credential, view.Email, grants)
}
