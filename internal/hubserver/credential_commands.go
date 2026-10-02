package hubserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) createAPITokenFor(ctx context.Context, request tokenRequest) (tokenResponse, error) {
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 200 || !validAPIScope(request.Scope) {
		return tokenResponse{}, &nativeError{Code: "invalid_token", Message: "Token name and scope are required", status: 422}
	}
	token, err := s.config.generateToken()
	if err != nil {
		return tokenResponse{}, err
	}
	now, err := s.database.currentTime()
	if err != nil {
		return tokenResponse{}, err
	}
	id := strings.TrimSpace(s.config.newTokenID())
	if id == "" {
		return tokenResponse{}, errors.New("empty token ID")
	}
	hash := apikey.HashToken(token)
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return tokenResponse{}, err
	}
	defer tx.Rollback()
	var user, organization, membership, keyScope any
	nativeOnly := false
	if request.Issuer != nil {
		issuer := *request.Issuer
		if s.config.Hosted == nil || issuer.Hosted == nil || issuer.Hosted.SupportActor != "" || issuer.HostedKeyScope != "" {
			return tokenResponse{}, nativeInvalid("Sign in as an organization member to create an API key")
		}
		if request.Scope != apiScopeOperator && request.Scope != apiScopeAdmin || (request.Scope == apiScopeAdmin) != (request.KeyScope == apikey.ScopeAdmin) || !hostedRoleAllows(issuer.HostedRole, request.KeyScope) {
			return tokenResponse{}, nativeInvalid("Select a scope allowed by your current role")
		}
		if request.ExpiresAt == nil || !request.ExpiresAt.After(now) || request.ExpiresAt.After(now.Add(90*24*time.Hour)) {
			return tokenResponse{}, nativeInvalid("Select an expiry within 90 days")
		}
		var message string
		request.ProjectAccess, message = resolveHostedProjectAccess(request.ProjectAccess, request.ProjectIDs)
		if message != "" {
			return tokenResponse{}, nativeInvalid(message)
		}
		scope := nativeScope{organization: tracker.OrganizationID(s.config.Hosted.OrganizationID), credential: issuer}
		for _, project := range request.ProjectIDs {
			scope.project = tracker.ProjectID(project)
			if err := s.requireHostedProject(ctx, tx, scope, false); err != nil {
				return tokenResponse{}, err
			}
			var granted int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM token_grants WHERE token_id=? AND organization_id=? AND project_id=?", issuer.ID, scope.organization, scope.project).Scan(&granted); err != nil {
				return tokenResponse{}, err
			}
			if granted != 1 {
				return tokenResponse{}, nativeNotFound()
			}
		}
		identity, current, err := s.hostedMutationIdentity(ctx, issuer)
		if err != nil || identity.Subject != issuer.Hosted.Subject || current.ID != issuer.HostedMembership {
			return tokenResponse{}, auth.ErrHostedIdentity
		}
		var local string
		if err := tx.QueryRowContext(ctx, "SELECT role FROM hosted_members WHERE user_id=? AND membership_id=? AND active=1", issuer.Hosted.Subject, issuer.HostedMembership).Scan(&local); err != nil || !auth.ValidOrganizationRole(local) || !hostedRoleAllows(lesserHostedRole(local, current.Role.Slug), request.KeyScope) {
			return tokenResponse{}, auth.ErrHostedIdentity
		}
		user, organization, membership, keyScope = issuer.Hosted.Subject, s.config.Hosted.OrganizationID, issuer.HostedMembership, string(request.KeyScope)
		nativeOnly = true
	}
	access := hostedProjectsSelected
	if request.Issuer != nil {
		access = request.ProjectAccess
	}
	var expiry any
	if request.ExpiresAt != nil {
		expiry = formatHubTime(*request.ExpiresAt)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at,expires_at,native_only,hosted_user_id,hosted_organization_id,hosted_membership_id,operator_key_scope,operator_project_access) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, request.Name, hash, tokenFingerprint(hash), request.Scope, formatHubTime(now), formatHubTime(now), expiry, nativeOnly, user, organization, membership, keyScope, access)
	if err != nil {
		return tokenResponse{}, &nativeError{Code: "token_conflict", Message: "API token name already exists", status: 409}
	}
	for _, project := range request.ProjectIDs {
		if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES(?,?,?) ON CONFLICT DO NOTHING", id, organization, project); err != nil {
			return tokenResponse{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return tokenResponse{}, err
	}
	projects := append([]string{}, request.ProjectIDs...)
	return tokenResponse{ProjectAccess: request.ProjectAccess, Projects: projects, ID: id, Name: request.Name, Scope: request.Scope, Token: token, Fingerprint: tokenFingerprint(hash), CreatedAt: now, ExpiresAt: request.ExpiresAt, KeyScope: request.KeyScope, NativeOnly: nativeOnly}, nil
}

func (s *Service) rotateAPITokenFor(ctx context.Context, id string) (tokenResponse, error) {
	token, err := s.config.generateToken()
	if err != nil {
		return tokenResponse{}, err
	}
	now, err := s.database.currentTime()
	if err != nil {
		return tokenResponse{}, err
	}
	result, err := s.database.db.ExecContext(ctx, `UPDATE api_tokens SET token_hash=?,token_fingerprint=?,rotated_at=?,revoked_at=NULL,updated_at=? WHERE id=? AND NOT EXISTS(SELECT 1 FROM runner_identities WHERE token_id=api_tokens.id)`, apikey.HashToken(token), tokenFingerprint(apikey.HashToken(token)), formatHubTime(now), formatHubTime(now), id)
	if err != nil {
		return tokenResponse{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return tokenResponse{}, err
	}
	if rows != 1 {
		return tokenResponse{}, nativeNotFound()
	}
	view, err := s.tokenMetadataByID(ctx, id)
	if err != nil {
		return tokenResponse{}, err
	}
	view.Token, view.RotatedAt = token, now
	return view, nil
}

func (s *Service) revokeAPITokenFor(ctx context.Context, id string) error {
	now, err := s.database.currentTime()
	if err != nil {
		return err
	}
	result, err := s.database.db.ExecContext(ctx, "UPDATE api_tokens SET revoked_at=?,updated_at=? WHERE id=? AND revoked_at IS NULL AND NOT EXISTS(SELECT 1 FROM runner_identities WHERE token_id=api_tokens.id)", formatHubTime(now), formatHubTime(now), id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return nativeNotFound()
	}
	return nil
}

func (s *Service) grantNativeTokenFor(ctx context.Context, id, organization, project string) error {
	if id == bootstrapTokenID {
		return nativeInvalid("Bootstrap administrator cannot be converted to a project token")
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var runnerCount int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM runner_identities WHERE token_id=?", id).Scan(&runnerCount); err != nil {
		return err
	}
	if runnerCount != 0 {
		return nativeInvalid("Runner grants are fixed at enrollment")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES(?,?,?) ON CONFLICT DO NOTHING", id, organization, project); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE api_tokens SET native_only=1 WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) createNativeOrganizationFor(ctx context.Context, name string) (nativeOrganization, error) {
	if strings.TrimSpace(name) == "" || len(name) > 200 {
		return nativeOrganization{}, nativeInvalid("Organization name is required")
	}
	organization := nativeOrganization{ID: tracker.OrganizationID(newNativeID("org")), Name: name}
	_, err := s.database.db.ExecContext(ctx, "INSERT INTO organizations(id,name,created_at) VALUES(?,?,?)", organization.ID, organization.Name, formatHubTime(s.config.now()))
	return organization, err
}
