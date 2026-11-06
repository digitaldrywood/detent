package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) AccessMembers(ctx context.Context, after string, limit int) ([]accessstore.Member, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,user_id,email,membership_id,role,active,principal_id,notify_issues,created_at,updated_at FROM hosted_members WHERE organization_id=$1 AND user_id>$2 ORDER BY user_id LIMIT $3`, s.organization, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.Member{}
	for rows.Next() {
		var row accessstore.Member
		if err := rows.Scan(&row.OrganizationID, &row.UserID, &row.Email, &row.MembershipID, &row.Role, &row.Active, &row.PrincipalID, &row.NotifyIssues, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) AccessMembersByEmail(ctx context.Context, email, after string, limit int) ([]accessstore.Member, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,user_id,email,membership_id,role,active,principal_id,notify_issues,created_at,updated_at FROM hosted_members WHERE organization_id=$1 AND email=$2 AND user_id>$3 ORDER BY user_id LIMIT $4`, s.organization, email, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.Member{}
	for rows.Next() {
		var row accessstore.Member
		if err := rows.Scan(&row.OrganizationID, &row.UserID, &row.Email, &row.MembershipID, &row.Role, &row.Active, &row.PrincipalID, &row.NotifyIssues, &row.CreatedAt, &row.UpdatedAt); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) AccessCredentials(ctx context.Context, after string, limit int) ([]accessstore.Credential, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at,rotated_at,revoked_at,expires_at,last_used_at,hosted_user_id,hosted_membership_id,operator_key_scope,operator_project_access,key_kind FROM api_tokens WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT $3`, s.organization, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.Credential{}
	for rows.Next() {
		var row accessstore.Credential
		if err := rows.Scan(&row.OrganizationID, &row.ID, &row.Name, &row.Hash, &row.Fingerprint, &row.Scope, &row.NativeOnly, &row.CreatedAt, &row.UpdatedAt, &row.RotatedAt, &row.RevokedAt, &row.ExpiresAt, &row.LastUsedAt, &row.UserID, &row.MembershipID, &row.KeyScope, &row.ProjectAccess, &row.Kind); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) AccessInvitations(ctx context.Context, after string, limit int) ([]accessstore.Invitation, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,id,email,role,accepted_user_id,created_at,expires_at FROM hosted_invitations WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT $3`, s.organization, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.Invitation{}
	for rows.Next() {
		var row accessstore.Invitation
		if err := rows.Scan(&row.OrganizationID, &row.ID, &row.Email, &row.Role, &row.AcceptedUserID, &row.CreatedAt, &row.ExpiresAt); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) AccessInvitationGrants(ctx context.Context, owner string, after tracker.ProjectID, limit int) ([]accessstore.InvitationGrant, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,invitation_id,project_id,can_write,manage_runner FROM hosted_invitation_grants WHERE organization_id=$1 AND invitation_id=$2 AND project_id>$3 ORDER BY project_id LIMIT $4`, s.organization, owner, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.InvitationGrant{}
	for rows.Next() {
		var row accessstore.InvitationGrant
		if err := rows.Scan(&row.OrganizationID, &row.InvitationID, &row.ProjectID, &row.CanWrite, &row.ManageRunner); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) AccessProjectGrants(ctx context.Context, owner string, after tracker.ProjectID, limit int) ([]accessstore.ProjectGrant, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,user_id,project_id,can_write,manage_runner FROM hosted_project_grants WHERE organization_id=$1 AND user_id=$2 AND project_id>$3 ORDER BY project_id LIMIT $4`, s.organization, owner, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.ProjectGrant{}
	for rows.Next() {
		var row accessstore.ProjectGrant
		if err := rows.Scan(&row.OrganizationID, &row.UserID, &row.ProjectID, &row.CanWrite, &row.ManageRunner); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) AccessTokenGrants(ctx context.Context, owner string, after tracker.ProjectID, limit int) ([]accessstore.TokenGrant, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,token_id,project_id FROM token_grants WHERE organization_id=$1 AND token_id=$2 AND project_id>$3 ORDER BY project_id LIMIT $4`, s.organization, owner, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.TokenGrant{}
	for rows.Next() {
		var row accessstore.TokenGrant
		if err := rows.Scan(&row.OrganizationID, &row.TokenID, &row.ProjectID); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
func (s *session) AccessMemberCredentials(ctx context.Context, user, after string, limit int) ([]accessstore.Credential, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at,rotated_at,revoked_at,expires_at,last_used_at,hosted_user_id,hosted_membership_id,operator_key_scope,operator_project_access,key_kind FROM api_tokens WHERE organization_id=$1 AND hosted_user_id=$2 AND id>$3 ORDER BY id LIMIT $4`, s.organization, user, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.Credential{}
	for rows.Next() {
		var row accessstore.Credential
		if err := rows.Scan(&row.OrganizationID, &row.ID, &row.Name, &row.Hash, &row.Fingerprint, &row.Scope, &row.NativeOnly, &row.CreatedAt, &row.UpdatedAt, &row.RotatedAt, &row.RevokedAt, &row.ExpiresAt, &row.LastUsedAt, &row.UserID, &row.MembershipID, &row.KeyScope, &row.ProjectAccess, &row.Kind); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}
