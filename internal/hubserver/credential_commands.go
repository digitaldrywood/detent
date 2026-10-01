package hubserver

import (
	"context"
	"errors"
	"strings"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) createAPITokenFor(ctx context.Context, request tokenRequest) (tokenResponse, error) {
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || !validAPIScope(request.Scope) {
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
	_, err = s.database.db.ExecContext(ctx, `INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, id, request.Name, hash, tokenFingerprint(hash), request.Scope, formatHubTime(now), formatHubTime(now))
	if err != nil {
		return tokenResponse{}, &nativeError{Code: "token_conflict", Message: "API token name already exists", status: 409}
	}
	return tokenResponse{ID: id, Name: request.Name, Scope: request.Scope, Token: token, Fingerprint: tokenFingerprint(hash), CreatedAt: now}, nil
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
