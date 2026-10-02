package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
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
		view := hostedMemberView{ID: member.ID, UserID: member.UserID, Email: emails[member.UserID], Role: member.Role.Slug, Status: member.Status, Grants: []hostedMemberGrant{}}
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

func (s *Service) inviteHostedMemberFor(ctx context.Context, credential apiCredential, email, role, key string) (hostedInvitationView, error) {
	if credential.Hosted == nil || credential.HostedRole != "owner" && credential.HostedRole != "admin" || !auth.ValidOrganizationRole(role) || role == "owner" && credential.HostedRole != "owner" {
		return hostedInvitationView{}, operatortool.ErrAccessDenied
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") || hostedEmailListed(s.config.Hosted.StaffEmails, email) {
		return hostedInvitationView{}, operatortool.ErrInvalidArguments
	}
	command := hostedCommand{actor: credential.ID, operation: "POST /api/v2/organizations/" + s.config.Hosted.OrganizationID + "/members/invitations", key: key, input: struct{ Email, Role string }{email, role}}
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
	view, err := s.sendReservedHostedInvitationFor(ctx, credential, email, role, reserved)
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
	if _, err := s.database.db.ExecContext(ctx, "UPDATE hosted_members SET role=?,updated_at=? WHERE user_id=?", role, formatHubTime(s.config.now()), member.UserID); err != nil {
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

func (s *Service) resendHostedInvitationFor(ctx context.Context, id string) error {
	var email string
	if err := s.database.db.QueryRowContext(ctx, "SELECT email FROM hosted_invitations WHERE id=? AND organization_id=? AND accepted_user_id=''", id, s.config.Hosted.OrganizationID).Scan(&email); err != nil {
		return err
	}
	return auth.ResendInvitationID(ctx, s.config.Hosted.Provider, id)
}

func (s *Service) sendReservedHostedInvitationFor(ctx context.Context, credential apiCredential, email, role string, reserved bool) (hostedInvitationView, error) {
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
	view := hostedInvitationView{ID: invitation.ID, Email: email, Role: role, CreatedAt: created}
	if !invitation.ExpiresAt.IsZero() {
		view.ExpiresAt = formatHubTime(invitation.ExpiresAt)
	}
	if _, err := s.database.db.ExecContext(ctx, `INSERT INTO hosted_invitations(id,email,organization_id,role,created_at,expires_at) VALUES (?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, invitation.ID, email, s.config.Hosted.OrganizationID, role, created, view.ExpiresAt); err != nil {
		return hostedInvitationView{}, err
	}
	return view, nil
}
