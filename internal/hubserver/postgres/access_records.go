package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) HubIdentity(ctx context.Context) (accessstore.HubIdentity, error) {
	var row accessstore.HubIdentity
	err := s.db.QueryRow(ctx, `SELECT organization_id,server_id,cursor_key FROM hub_server_identities WHERE organization_id=$1`, s.organization).Scan(&row.OrganizationID, &row.ID, &row.CursorKey)
	return row, mapError(ctx, err)
}

func (s *session) CreateHubIdentity(ctx context.Context, row accessstore.HubIdentity) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hub_server_identities (organization_id,server_id,cursor_key) VALUES ($1,$2,$3)`, row.OrganizationID, row.ID, row.CursorKey)
	return mapError(ctx, err)
}

func (s *session) AccessOrganization(ctx context.Context) (accessstore.Organization, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.organizationRecord != nil {
		return *s.organizationRecord, nil
	}
	var row accessstore.Organization
	err := s.db.QueryRow(ctx, `SELECT organization_id,name,slug,local,created_at FROM organizations WHERE organization_id=$1`+s.accessLock(), s.organization).Scan(&row.OrganizationID, &row.Name, &row.Slug, &row.Local, &row.CreatedAt)
	if err == nil && s.credentials != nil {
		s.organizationRecord = &row
	}
	return row, mapError(ctx, err)
}

func (s *session) CreateAccessOrganization(ctx context.Context, row accessstore.Organization) error {
	return s.createAccessOrganization(ctx, row, true)
}

func (s *session) createAccessOrganization(ctx context.Context, row accessstore.Organization, initializeUsage bool) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO organizations (organization_id,name,slug,local,created_at) VALUES ($1,$2,$3,$4,$5)`, row.OrganizationID, row.Name, row.Slug, row.Local, row.CreatedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	if initializeUsage {
		_, err = s.db.Exec(ctx, `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',0)`, row.OrganizationID)
	}
	return mapError(ctx, err)
}

func (s *session) LockAccessOrganization(ctx context.Context) error {
	var organization tracker.OrganizationID
	if err := s.db.QueryRow(ctx, `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR UPDATE`, s.organization).Scan(&organization); err != nil {
		return mapError(ctx, err)
	}
	return nil
}

func (s *session) UpdateAccessOrganizationName(ctx context.Context, name string) error {
	result, err := s.db.Exec(ctx, `UPDATE organizations SET name=$2 WHERE organization_id=$1`, s.organization, name)
	var rows int64
	if err == nil {
		rows = result.RowsAffected()
	}
	return affected(ctx, rows, err)
}

func (s *session) AccessTenant(ctx context.Context) (accessstore.Tenant, error) {
	var row accessstore.Tenant
	err := s.db.QueryRow(ctx, `SELECT organization_id,provider_id,bootstrap_subject,deployment,public_url,allocation_generation FROM hosted_tenant WHERE organization_id=$1`+s.accessLock(), s.organization).Scan(&row.OrganizationID, &row.ProviderID, &row.BootstrapSubject, &row.Deployment, &row.PublicURL, &row.AllocationGeneration)
	return row, accessLookupError(ctx, "tenant", err)
}

func (s *session) CreateAccessTenant(ctx context.Context, row accessstore.Tenant) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_tenant (organization_id,provider_id,bootstrap_subject,deployment,public_url,allocation_generation) VALUES ($1,$2,$3,$4,$5,$6)`, row.OrganizationID, row.ProviderID, row.BootstrapSubject, row.Deployment, row.PublicURL, row.AllocationGeneration)
	return mapError(ctx, err)
}

func (s *session) AccessCredential(ctx context.Context, id string) (accessstore.Credential, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if row, ok := s.credentials[id]; ok {
		return row, nil
	}
	var row accessstore.Credential
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at,rotated_at,revoked_at,expires_at,last_used_at,hosted_user_id,hosted_membership_id,operator_key_scope,operator_project_access,key_kind FROM api_tokens WHERE organization_id=$1 AND id=$2`+s.accessLock(), s.organization, id).Scan(&row.OrganizationID, &row.ID, &row.Name, &row.Hash, &row.Fingerprint, &row.Scope, &row.NativeOnly, &row.CreatedAt, &row.UpdatedAt, &row.RotatedAt, &row.RevokedAt, &row.ExpiresAt, &row.LastUsedAt, &row.UserID, &row.MembershipID, &row.KeyScope, &row.ProjectAccess, &row.Kind)
	if err == nil && s.credentials != nil {
		s.credentials[id] = row
	}
	return row, accessLookupError(ctx, "credential", err)
}

func (s *session) CreateAccessCredential(ctx context.Context, row accessstore.Credential) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO api_tokens (organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at,rotated_at,revoked_at,expires_at,last_used_at,hosted_user_id,hosted_membership_id,operator_key_scope,operator_project_access,key_kind) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, row.OrganizationID, row.ID, row.Name, row.Hash, row.Fingerprint, row.Scope, row.NativeOnly, row.CreatedAt, row.UpdatedAt, row.RotatedAt, row.RevokedAt, row.ExpiresAt, row.LastUsedAt, row.UserID, row.MembershipID, row.KeyScope, row.ProjectAccess, row.Kind)
	return mapError(ctx, err)
}

func (s *session) AccessMember(ctx context.Context, user_id string) (accessstore.Member, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if row, ok := s.members[user_id]; ok {
		return row, nil
	}
	var row accessstore.Member
	err := s.db.QueryRow(ctx, `SELECT organization_id,user_id,email,membership_id,role,active,principal_id,notify_issues,created_at,updated_at FROM hosted_members WHERE organization_id=$1 AND user_id=$2`+s.accessLock(), s.organization, user_id).Scan(&row.OrganizationID, &row.UserID, &row.Email, &row.MembershipID, &row.Role, &row.Active, &row.PrincipalID, &row.NotifyIssues, &row.CreatedAt, &row.UpdatedAt)
	if err == nil && s.members != nil {
		s.members[user_id] = row
	}
	return row, accessLookupError(ctx, "member", err)
}

func (s *session) CreateAccessMember(ctx context.Context, row accessstore.Member) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if err := s.lockHostedOrganization(ctx); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_members (organization_id,user_id,email,membership_id,role,active,principal_id,notify_issues,created_at,updated_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, row.OrganizationID, row.UserID, row.Email, row.MembershipID, row.Role, row.Active, row.PrincipalID, row.NotifyIssues, row.CreatedAt, row.UpdatedAt)
	return mapError(ctx, err)
}

func (s *session) AccessProjectGrant(ctx context.Context, user_id string, project_id tracker.ProjectID) (accessstore.ProjectGrant, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	key := projectAccessKey{principal: user_id, project: project_id}
	if row, ok := s.projectGrants[key]; ok {
		return row, nil
	}
	var row accessstore.ProjectGrant
	err := s.db.QueryRow(ctx, `SELECT organization_id,user_id,project_id,can_write,manage_runner FROM hosted_project_grants WHERE organization_id=$1 AND user_id=$2 AND project_id=$3`+s.accessLock(), s.organization, user_id, project_id).Scan(&row.OrganizationID, &row.UserID, &row.ProjectID, &row.CanWrite, &row.ManageRunner)
	if err == nil && s.projectGrants != nil {
		s.projectGrants[key] = row
	}
	return row, accessLookupError(ctx, "project_grant", err)
}

func (s *session) CreateAccessProjectGrant(ctx context.Context, row accessstore.ProjectGrant) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_project_grants (organization_id,user_id,project_id,can_write,manage_runner) VALUES ($1,$2,$3,$4,$5)`, row.OrganizationID, row.UserID, row.ProjectID, row.CanWrite, row.ManageRunner)
	return mapError(ctx, err)
}

func (s *session) AccessTokenGrant(ctx context.Context, token_id string, project_id tracker.ProjectID) (accessstore.TokenGrant, error) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	key := projectAccessKey{principal: token_id, project: project_id}
	if row, ok := s.tokenGrants[key]; ok {
		return row, nil
	}
	var row accessstore.TokenGrant
	err := s.db.QueryRow(ctx, `SELECT organization_id,token_id,project_id FROM token_grants WHERE organization_id=$1 AND token_id=$2 AND project_id=$3`+s.accessLock(), s.organization, token_id, project_id).Scan(&row.OrganizationID, &row.TokenID, &row.ProjectID)
	if err == nil && s.tokenGrants != nil {
		s.tokenGrants[key] = row
	}
	return row, accessLookupError(ctx, "token_grant", err)
}

func (s *session) CreateAccessTokenGrant(ctx context.Context, row accessstore.TokenGrant) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO token_grants (organization_id,token_id,project_id) VALUES ($1,$2,$3)`, row.OrganizationID, row.TokenID, row.ProjectID)
	return mapError(ctx, err)
}

func (s *session) AccessSession(ctx context.Context, token_hash string) (accessstore.Session, error) {
	var row accessstore.Session
	err := s.db.QueryRow(ctx, `SELECT organization_id,token_hash,email,identity_subject,provider_organization_id,session_id,identity_created_at,identity_expires_at,support_actor,support_reason,created_at,expires_at,revoked_at FROM hosted_sessions WHERE organization_id=$1 AND token_hash=$2`+s.accessLock(), s.organization, token_hash).Scan(&row.OrganizationID, &row.Hash, &row.Email, &row.Subject, &row.ProviderOrganizationID, &row.SessionID, &row.IdentityCreatedAt, &row.IdentityExpiresAt, &row.SupportActor, &row.SupportReason, &row.CreatedAt, &row.ExpiresAt, &row.RevokedAt)
	return row, accessLookupError(ctx, "session", err)
}

func (s *session) CreateAccessSession(ctx context.Context, row accessstore.Session) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_sessions (organization_id,token_hash,email,identity_subject,provider_organization_id,session_id,identity_created_at,identity_expires_at,support_actor,support_reason,created_at,expires_at,revoked_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, row.OrganizationID, row.Hash, row.Email, row.Subject, row.ProviderOrganizationID, row.SessionID, row.IdentityCreatedAt, row.IdentityExpiresAt, row.SupportActor, row.SupportReason, row.CreatedAt, row.ExpiresAt, row.RevokedAt)
	return mapError(ctx, err)
}

func (s *session) AccessLoginTransaction(ctx context.Context, token_hash string) (accessstore.LoginTransaction, error) {
	var row accessstore.LoginTransaction
	err := s.db.QueryRow(ctx, `SELECT organization_id,token_hash,state,verifier,provider_organization_id,support_actor,support_session,invitation_token,expires_at,consumed_at FROM hosted_transactions WHERE organization_id=$1 AND token_hash=$2`, s.organization, token_hash).Scan(&row.OrganizationID, &row.Hash, &row.State, &row.Verifier, &row.ProviderOrganizationID, &row.SupportActor, &row.SupportSession, &row.InvitationToken, &row.ExpiresAt, &row.ConsumedAt)
	return row, mapError(ctx, err)
}

func (s *session) CreateAccessLoginTransaction(ctx context.Context, row accessstore.LoginTransaction) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_transactions (organization_id,token_hash,state,verifier,provider_organization_id,support_actor,support_session,invitation_token,expires_at,consumed_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, row.OrganizationID, row.Hash, row.State, row.Verifier, row.ProviderOrganizationID, row.SupportActor, row.SupportSession, row.InvitationToken, row.ExpiresAt, row.ConsumedAt)
	return mapError(ctx, err)
}

func (s *session) AccessInvitation(ctx context.Context, id string) (accessstore.Invitation, error) {
	var row accessstore.Invitation
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,email,role,accepted_user_id,created_at,expires_at FROM hosted_invitations WHERE organization_id=$1 AND id=$2`+s.accessLock(), s.organization, id).Scan(&row.OrganizationID, &row.ID, &row.Email, &row.Role, &row.AcceptedUserID, &row.CreatedAt, &row.ExpiresAt)
	return row, mapError(ctx, err)
}

func (s *session) CreateAccessInvitation(ctx context.Context, row accessstore.Invitation) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_invitations (organization_id,id,email,role,accepted_user_id,created_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, row.OrganizationID, row.ID, row.Email, row.Role, row.AcceptedUserID, row.CreatedAt, row.ExpiresAt)
	return mapError(ctx, err)
}

func (s *session) AccessInvitationGrant(ctx context.Context, invitation_id string, project_id tracker.ProjectID) (accessstore.InvitationGrant, error) {
	var row accessstore.InvitationGrant
	err := s.db.QueryRow(ctx, `SELECT organization_id,invitation_id,project_id,can_write,manage_runner FROM hosted_invitation_grants WHERE organization_id=$1 AND invitation_id=$2 AND project_id=$3`, s.organization, invitation_id, project_id).Scan(&row.OrganizationID, &row.InvitationID, &row.ProjectID, &row.CanWrite, &row.ManageRunner)
	return row, mapError(ctx, err)
}

func (s *session) CreateAccessInvitationGrant(ctx context.Context, row accessstore.InvitationGrant) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_invitation_grants (organization_id,invitation_id,project_id,can_write,manage_runner) VALUES ($1,$2,$3,$4,$5)`, row.OrganizationID, row.InvitationID, row.ProjectID, row.CanWrite, row.ManageRunner)
	return mapError(ctx, err)
}

func (s *session) AccessMemberReservation(ctx context.Context, email string) (accessstore.MemberReservation, error) {
	var row accessstore.MemberReservation
	err := s.db.QueryRow(ctx, `SELECT organization_id,email,expires_at FROM hosted_member_reservations WHERE organization_id=$1 AND email=$2`, s.organization, email).Scan(&row.OrganizationID, &row.Email, &row.ExpiresAt)
	return row, mapError(ctx, err)
}

func (s *session) CreateAccessMemberReservation(ctx context.Context, row accessstore.MemberReservation) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_member_reservations (organization_id,email,expires_at) VALUES ($1,$2,$3)`, row.OrganizationID, row.Email, row.ExpiresAt)
	return mapError(ctx, err)
}

func (s *session) AccessMemberSeatCounts(ctx context.Context, now time.Time) (int64, int64, error) {
	var active, reserved int64
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM hosted_members WHERE organization_id=$1 AND active`, s.organization).Scan(&active); err != nil {
		return 0, 0, mapError(ctx, err)
	}
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM hosted_member_reservations WHERE organization_id=$1 AND expires_at>$2`, s.organization, now).Scan(&reserved); err != nil {
		return 0, 0, mapError(ctx, err)
	}
	return active, reserved, nil
}

func (s *session) ReserveAccessMemberReservation(ctx context.Context, row accessstore.MemberReservation, now time.Time, limit int64) (bool, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return false, err
	}
	if limit < 0 {
		return false, accessstore.ErrDenied
	}
	var locked string
	if err := s.db.QueryRow(ctx, `SELECT organization_id FROM organizations WHERE organization_id=$1 FOR UPDATE`, s.organization).Scan(&locked); err != nil {
		return false, mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM hosted_member_reservations WHERE organization_id=$1 AND expires_at<=$2`, s.organization, now); err != nil {
		return false, mapError(ctx, err)
	}
	var active int64
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM hosted_members WHERE organization_id=$1 AND lower(email)=lower($2) AND active`, s.organization, row.Email).Scan(&active); err != nil {
		return false, mapError(ctx, err)
	}
	var held int64
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM hosted_member_reservations WHERE organization_id=$1 AND email=$2 AND expires_at>$3`, s.organization, row.Email, now).Scan(&held); err != nil {
		return false, mapError(ctx, err)
	}
	if active == 0 && held == 0 && limit > 0 {
		members, reservations, err := s.AccessMemberSeatCounts(ctx, now)
		if err != nil {
			return false, err
		}
		if members+reservations >= limit {
			return false, accessstore.ErrSeatLimit
		}
	}
	if active > 0 {
		return false, nil
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_member_reservations(organization_id,email,expires_at) VALUES($1,$2,$3) ON CONFLICT(organization_id,email) DO UPDATE SET expires_at=excluded.expires_at`, row.OrganizationID, row.Email, row.ExpiresAt)
	if err != nil {
		return false, mapError(ctx, err)
	}
	return held == 0, nil
}

func (s *session) DeleteAccessMemberReservation(ctx context.Context, email string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM hosted_member_reservations WHERE organization_id=$1 AND email=$2`, s.organization, email)
	return mapError(ctx, err)
}

func (s *session) AccessBindingMigration(ctx context.Context, id int64) (accessstore.BindingMigration, error) {
	var row accessstore.BindingMigration
	err := s.db.QueryRow(ctx, `SELECT organization_id,id,from_public_url,to_public_url,from_deployment,from_generation,to_generation,migrated_at,applied FROM hosted_binding_migrations WHERE organization_id=$1 AND id=$2`, s.organization, id).Scan(&row.OrganizationID, &row.ID, &row.FromPublicURL, &row.ToPublicURL, &row.FromDeployment, &row.FromGeneration, &row.ToGeneration, &row.MigratedAt, &row.Applied)
	return row, mapError(ctx, err)
}

func (s *session) CreateAccessBindingMigration(ctx context.Context, row accessstore.BindingMigration) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_binding_migrations (organization_id,id,from_public_url,to_public_url,from_deployment,from_generation,to_generation,migrated_at,applied) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, row.OrganizationID, row.ID, row.FromPublicURL, row.ToPublicURL, row.FromDeployment, row.FromGeneration, row.ToGeneration, row.MigratedAt, row.Applied)
	return mapError(ctx, err)
}

func (s *session) CredentialByHash(ctx context.Context, hash string) (accessstore.Credential, error) {
	var id string
	if err := s.db.QueryRow(ctx, `SELECT id FROM api_tokens WHERE organization_id=$1 AND token_hash=$2`+s.accessLock(), s.organization, hash).Scan(&id); err != nil {
		return accessstore.Credential{}, mapError(ctx, err)
	}
	return s.AccessCredential(ctx, id)
}

func (s *session) UpdateAccessMember(ctx context.Context, row accessstore.Member) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if err := s.lockHostedOrganization(ctx); err != nil {
		return err
	}
	result, err := s.db.Exec(ctx, `UPDATE hosted_members SET email=$3,membership_id=$4,role=$5,active=$6,notify_issues=$7,updated_at=$8 WHERE organization_id=$1 AND user_id=$2 AND principal_id=$9`, s.organization, row.UserID, row.Email, row.MembershipID, row.Role, row.Active, row.NotifyIssues, row.UpdatedAt, row.PrincipalID)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	return affected(ctx, n, err)
}

func (s *session) RevokeAccessMember(ctx context.Context, user string, at time.Time) error {
	if err := s.lockHostedOrganization(ctx); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `UPDATE hosted_members SET active=false,updated_at=$3 WHERE organization_id=$1 AND user_id=$2`, s.organization, user, at)
	if err != nil {
		return mapError(ctx, err)
	}
	return s.ClearAccessMemberGrants(ctx, user)
}

func (s *session) RotateAccessCredential(ctx context.Context, id, hash, fingerprint string, at time.Time) error {
	result, err := s.db.Exec(ctx, `UPDATE api_tokens SET token_hash=$3,token_fingerprint=$4,rotated_at=$5,updated_at=$5 WHERE organization_id=$1 AND id=$2 AND revoked_at IS NULL`, s.organization, id, hash, fingerprint, at)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	return affected(ctx, n, err)
}

func (s *session) RenewAccessCredential(ctx context.Context, id, hash, fingerprint string, expires, at time.Time) error {
	result, err := s.db.Exec(ctx, `UPDATE api_tokens SET token_hash=$3,token_fingerprint=$4,expires_at=$5,rotated_at=$6,updated_at=$6 WHERE organization_id=$1 AND id=$2 AND revoked_at IS NULL`, s.organization, id, hash, fingerprint, expires, at)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	return affected(ctx, n, err)
}

func (s *session) RevokeAccessCredential(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.Exec(ctx, `UPDATE api_tokens SET revoked_at=COALESCE(revoked_at,$3),updated_at=$3 WHERE organization_id=$1 AND id=$2`, s.organization, id, at)
	return mapError(ctx, err)
}

func (s *session) RevokeAccessSession(ctx context.Context, hash string, at time.Time) error {
	return s.EnsureRevokedAccessSession(ctx, hash, at)
}

func (s *session) EnsureRevokedAccessSession(ctx context.Context, hash string, at time.Time) error {
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_sessions(organization_id,token_hash,email,identity_subject,provider_organization_id,session_id,identity_created_at,identity_expires_at,support_actor,support_reason,created_at,expires_at,revoked_at) VALUES($1,$2,'','','','',$3,$3,'','',$3,$3,$3) ON CONFLICT(organization_id,token_hash) DO UPDATE SET revoked_at=COALESCE(hosted_sessions.revoked_at,excluded.revoked_at)`, s.organization, hash, at)
	return mapError(ctx, err)
}

func (s *session) ConsumeAccessTransaction(ctx context.Context, hash, state string, at time.Time) (accessstore.LoginTransaction, error) {
	result, err := s.db.Exec(ctx, `UPDATE hosted_transactions SET consumed_at=$4 WHERE organization_id=$1 AND token_hash=$2 AND state=$3 AND consumed_at IS NULL AND expires_at>$4`, s.organization, hash, state, at)
	if err != nil {
		return accessstore.LoginTransaction{}, mapError(ctx, err)
	}
	n := result.RowsAffected()
	if err := affected(ctx, n, err); err != nil {
		return accessstore.LoginTransaction{}, err
	}
	return s.AccessLoginTransaction(ctx, hash)
}

func (s *session) AcceptAccessInvitation(ctx context.Context, id, email, user string, at time.Time) error {
	result, err := s.db.Exec(ctx, `UPDATE hosted_invitations SET accepted_user_id=$4 WHERE organization_id=$1 AND id=$2 AND lower(email)=lower($3) AND accepted_user_id='' AND expires_at>$5`, s.organization, id, email, user, at)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	return affected(ctx, n, err)
}

func (s *session) ReplaceAccessInvitationGrants(ctx context.Context, invitation string, grants []accessstore.InvitationGrant) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM hosted_invitation_grants WHERE organization_id=$1 AND invitation_id=$2`, s.organization, invitation); err != nil {
		return mapError(ctx, err)
	}
	for _, grant := range grants {
		if err := s.checkOrganization(grant.OrganizationID); err != nil || grant.InvitationID != invitation {
			return accessstore.ErrDenied
		}
		if err := s.CreateAccessInvitationGrant(ctx, grant); err != nil {
			return err
		}
	}
	return nil
}

func (s *session) DeleteAccessInvitation(ctx context.Context, id string) (string, error) {
	var email, accepted string
	if err := s.db.QueryRow(ctx, `SELECT email,accepted_user_id FROM hosted_invitations WHERE organization_id=$1 AND id=$2 FOR UPDATE`, s.organization, id).Scan(&email, &accepted); err != nil {
		return "", mapError(ctx, err)
	}
	if accepted != "" {
		return "", accessstore.ErrDenied
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM hosted_invitation_grants WHERE organization_id=$1 AND invitation_id=$2`, s.organization, id); err != nil {
		return "", mapError(ctx, err)
	}
	result, err := s.db.Exec(ctx, `DELETE FROM hosted_invitations WHERE organization_id=$1 AND id=$2 AND accepted_user_id=''`, s.organization, id)
	if err != nil {
		return "", mapError(ctx, err)
	}
	n := result.RowsAffected()
	if err := affected(ctx, n, err); err != nil {
		return "", err
	}
	var remaining int64
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM hosted_invitations WHERE organization_id=$1 AND lower(email)=lower($2) AND accepted_user_id=''`, s.organization, email).Scan(&remaining); err != nil {
		return "", mapError(ctx, err)
	}
	if remaining == 0 {
		if err := s.DeleteAccessMemberReservation(ctx, email); err != nil {
			return "", err
		}
	}
	return email, nil
}

func (s *session) DeleteAccessProjectGrant(ctx context.Context, user string, project tracker.ProjectID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM hosted_project_grants WHERE organization_id=$1 AND user_id=$2 AND project_id=$3`, s.organization, user, project)
	return mapError(ctx, err)
}

func (s *session) AuthorizeAccess(ctx context.Context, authority accessstore.Authority, now time.Time) (accessstore.Member, error) {

	credential, err := s.AccessCredential(ctx, authority.CredentialID)
	if err != nil {
		return accessstore.Member{}, accessAuthorizationError(err)
	}
	if !credential.Valid(authority.Hash, now) {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	required := apikey.ScopeRead
	if authority.Write {
		required = apikey.ScopeWrite
	}
	if authority.Admin {
		required = apikey.ScopeAdmin
	}
	if credential.KeyScope != "" && (!apikey.ValidScope(apikey.Scope(credential.KeyScope)) || !apikey.HasScope([]string{credential.KeyScope}, required)) {
		return accessstore.Member{}, accessstore.ErrDenied
	}
	var member accessstore.Member
	if authority.UserID != "" {
		member, err = s.AccessMember(ctx, authority.UserID)
		if err != nil {
			return member, accessAuthorizationError(err)
		}
		if !member.Active || member.MembershipID != authority.MembershipID {
			return member, accessstore.ErrDenied
		}
		principal, err := s.AccessCredential(ctx, member.PrincipalID)
		if err != nil {
			return member, accessAuthorizationError(err)
		}
		if principal.RevokedAt != nil {
			return member, accessstore.ErrDenied
		}
		if credential.ID != member.PrincipalID && (credential.UserID != member.UserID || credential.MembershipID != member.MembershipID) {
			return member, accessstore.ErrDenied
		}
		role := member.Role
		if authority.Role != "" {
			role = accessstore.LesserRole(role, authority.Role)
		}
		if authority.Admin && !accessstore.RoleAllows(role, apikey.ScopeAdmin) {
			return member, accessstore.ErrDenied
		}
		if authority.Write && !accessstore.RoleAllows(role, apikey.ScopeWrite, authority.Action) {
			return member, accessstore.ErrDenied
		}
	}
	if authority.SessionHash != "" {
		session, err := s.AccessSession(ctx, authority.SessionHash)
		if err != nil {
			return member, accessAuthorizationError(err)
		}
		if session.RevokedAt != nil || !session.ExpiresAt.After(now) || session.Subject != member.UserID {
			return member, accessstore.ErrDenied
		}
	}
	if authority.ProjectID != "" && (credential.NativeOnly || credential.Scope != "admin") {
		if member.UserID != "" {
			grant, err := s.AccessProjectGrant(ctx, member.UserID, authority.ProjectID)
			if err != nil {
				return member, accessAuthorizationError(err)
			}
			if authority.Write && !grant.CanWrite {
				return member, accessstore.ErrDenied
			}
			if _, err := s.AccessTokenGrant(ctx, member.PrincipalID, authority.ProjectID); err != nil {
				return member, accessAuthorizationError(err)
			}
		}
		if credential.ProjectAccess != "all" {
			if _, err := s.AccessTokenGrant(ctx, credential.ID, authority.ProjectID); err != nil {
				return member, accessAuthorizationError(err)
			}
		}
	}
	return member, nil
}

func accessLookupError(ctx context.Context, step string, err error) error {
	if err == nil {
		return nil
	}
	return &accessstore.LookupError{Step: step, Cause: mapError(ctx, err)}
}

func accessAuthorizationError(err error) error {
	if errors.Is(err, tenantauth.ErrUnavailable) {
		return err
	}
	if errors.Is(err, corestore.ErrNotFound) {
		return accessstore.ErrDenied
	}
	return errors.Join(tenantauth.ErrUnavailable, err)
}

var _ accessstore.Reader = (*session)(nil)
var _ accessstore.Writer = (*session)(nil)

func (s *session) accessLock() string {
	if s.readOnly {
		return ""
	}
	return " FOR SHARE"
}

func (s *session) EnsureAccessSession(ctx context.Context, row accessstore.Session) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_sessions(organization_id,token_hash,email,identity_subject,provider_organization_id,session_id,identity_created_at,identity_expires_at,support_actor,support_reason,created_at,expires_at,revoked_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13) ON CONFLICT(organization_id,token_hash) DO NOTHING`, row.OrganizationID, row.Hash, row.Email, row.Subject, row.ProviderOrganizationID, row.SessionID, row.IdentityCreatedAt, row.IdentityExpiresAt, row.SupportActor, row.SupportReason, row.CreatedAt, row.ExpiresAt, row.RevokedAt)
	return accessLookupError(ctx, "session_create", err)
}

func (s *session) ClearAccessMemberGrants(ctx context.Context, user string) error {
	if err := s.lockHostedOrganization(ctx); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM token_grants WHERE organization_id=$1 AND token_id=(SELECT principal_id FROM hosted_members WHERE organization_id=$1 AND user_id=$2)`, s.organization, user); err != nil {
		return mapError(ctx, err)
	}
	_, err := s.db.Exec(ctx, `DELETE FROM hosted_project_grants WHERE organization_id=$1 AND user_id=$2`, s.organization, user)
	return mapError(ctx, err)
}

func (s *session) SetAccessProjectGrant(ctx context.Context, row accessstore.ProjectGrant) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO hosted_project_grants(organization_id,user_id,project_id,can_write,manage_runner) VALUES($1,$2,$3,$4,$5) ON CONFLICT(organization_id,user_id,project_id) DO UPDATE SET can_write=excluded.can_write,manage_runner=excluded.manage_runner`, row.OrganizationID, row.UserID, row.ProjectID, row.CanWrite, row.ManageRunner)
	return mapError(ctx, err)
}

func (s *session) SetAccessTokenGrant(ctx context.Context, row accessstore.TokenGrant) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO token_grants(organization_id,token_id,project_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, row.OrganizationID, row.TokenID, row.ProjectID)
	return mapError(ctx, err)
}

func (s *session) DeleteAccessTokenGrant(ctx context.Context, token string, project tracker.ProjectID) error {
	_, err := s.db.Exec(ctx, `DELETE FROM token_grants WHERE organization_id=$1 AND token_id=$2 AND project_id=$3`, s.organization, token, project)
	return mapError(ctx, err)
}

func (s *session) SetAccessTransactionInvitation(ctx context.Context, hash, token string) error {
	result, err := s.db.Exec(ctx, `UPDATE hosted_transactions SET invitation_token=$3 WHERE organization_id=$1 AND token_hash=$2 AND consumed_at IS NULL`, s.organization, hash, token)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	return affected(ctx, n, err)
}
func (s *session) EnsureAccessCredential(ctx context.Context, row accessstore.Credential) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO api_tokens (organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at,rotated_at,revoked_at,expires_at,last_used_at,hosted_user_id,hosted_membership_id,operator_key_scope,operator_project_access,key_kind) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18) ON CONFLICT(organization_id,id) DO NOTHING`, row.OrganizationID, row.ID, row.Name, row.Hash, row.Fingerprint, row.Scope, row.NativeOnly, row.CreatedAt, row.UpdatedAt, row.RotatedAt, row.RevokedAt, row.ExpiresAt, row.LastUsedAt, row.UserID, row.MembershipID, row.KeyScope, row.ProjectAccess, row.Kind)
	return mapError(ctx, err)
}

func (s *session) OrganizationLogo(ctx context.Context) (accessstore.Logo, error) {
	var logo accessstore.Logo
	err := s.db.QueryRow(ctx, `SELECT logo_image_key,logo_image_sha256,logo_revision FROM organizations WHERE organization_id=$1`, s.organization).Scan(&logo.ImageKey, &logo.ImageSHA256, &logo.Revision)
	return logo, mapError(ctx, err)
}

func (s *session) UpdateOrganizationLogo(ctx context.Context, logo accessstore.Logo, revision tracker.Revision) error {
	result, err := s.db.Exec(ctx, `UPDATE organizations SET logo_image_key=$2,logo_image_sha256=$3,logo_revision=logo_revision+1 WHERE organization_id=$1 AND logo_revision=$4`, s.organization, logo.ImageKey, logo.ImageSHA256, revision)
	if err != nil {
		return mapError(ctx, err)
	}
	n := result.RowsAffected()
	if n == 0 {
		return corestore.ErrRevisionConflict
	}
	return err
}
