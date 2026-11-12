package postgres

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
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

func (s *session) AccessCredentialMetadataPage(ctx context.Context, offset, limit int) ([]accessstore.Credential, bool, error) {
	if err := checkPage(limit); err != nil {
		return nil, false, err
	}
	if offset < 0 || offset > 100000 {
		return nil, false, corestore.ErrInvalidPage
	}
	rows, err := s.db.Query(ctx, `SELECT c.organization_id,c.id,c.name,c.token_hash,c.token_fingerprint,c.scope,c.native_only,c.created_at,c.updated_at,c.rotated_at,c.revoked_at,c.expires_at,c.last_used_at,c.hosted_user_id,c.hosted_membership_id,c.operator_key_scope,c.operator_project_access,c.key_kind
 FROM api_tokens c WHERE c.organization_id=$1 AND NOT EXISTS(SELECT 1 FROM runner_identities r WHERE r.organization_id=c.organization_id AND r.token_id=c.id)
 ORDER BY c.id OFFSET $2 LIMIT $3`, s.organization, offset, limit+1)
	if err != nil {
		return nil, false, mapError(ctx, err)
	}
	defer rows.Close()
	result := []accessstore.Credential{}
	for rows.Next() {
		var row accessstore.Credential
		if err := rows.Scan(&row.OrganizationID, &row.ID, &row.Name, &row.Hash, &row.Fingerprint, &row.Scope, &row.NativeOnly, &row.CreatedAt, &row.UpdatedAt, &row.RotatedAt, &row.RevokedAt, &row.ExpiresAt, &row.LastUsedAt, &row.UserID, &row.MembershipID, &row.KeyScope, &row.ProjectAccess, &row.Kind); err != nil {
			return nil, false, mapError(ctx, err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, mapError(ctx, err)
	}
	more := len(result) > limit
	return result[:min(len(result), limit)], more, nil
}

func (s *session) AccessInstanceOrganizationPage(ctx context.Context, credential, hash string, offset, limit int) ([]accessstore.Organization, bool, error) {
	if err := checkPage(limit); err != nil {
		return nil, false, err
	}
	if offset < 0 || offset > 100000 {
		return nil, false, corestore.ErrInvalidPage
	}
	pageError := func(err error) error {
		var state interface{ SQLState() string }
		if errors.As(err, &state) && state.SQLState() == "28000" {
			return accessstore.ErrDenied
		}
		return mapError(ctx, err)
	}
	rows, err := s.db.Query(ctx, `SELECT organization_id,name,local FROM public.operator_organization_metadata_page($1,$2,$3,$4)`, credential, hash, offset, limit)
	if err != nil {
		return nil, false, pageError(err)
	}
	defer rows.Close()
	result := []accessstore.Organization{}
	for rows.Next() {
		var row accessstore.Organization
		if err := rows.Scan(&row.OrganizationID, &row.Name, &row.Local); err != nil {
			return nil, false, pageError(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, pageError(err)
	}
	more := len(result) > limit
	return result[:min(len(result), limit)], more, nil
}
