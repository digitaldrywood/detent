package hubserver

import (
	"context"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func accessTokenResponse(row accessstore.Credential) tokenResponse {
	result := tokenResponse{Kind: row.Kind, ProjectAccess: hostedProjectAccess(row.ProjectAccess), Projects: []string{}, Grants: []tokenGrantResponse{}, ExpiresAt: row.ExpiresAt, KeyScope: apikey.Scope(row.KeyScope), NativeOnly: row.NativeOnly, RevokedAt: row.RevokedAt, ID: row.ID, Name: row.Name, Scope: apiScope(row.Scope), Fingerprint: row.Fingerprint, CreatedAt: row.CreatedAt}
	if row.RotatedAt != nil {
		result.RotatedAt = *row.RotatedAt
	}
	return result
}

func (s *Service) createAccessToken(ctx context.Context, request tokenRequest) (tokenResponse, error) {
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 200 || !validAPIScope(request.Scope) {
		return tokenResponse{}, nativePublicInvalid("Token name and scope are required")
	}
	now := s.config.now()
	token, err := s.config.generateToken()
	if err != nil {
		return tokenResponse{}, err
	}
	hash := apikey.HashToken(token)
	row := accessstore.Credential{OrganizationID: s.accessOrganization(), ID: strings.TrimSpace(s.config.newTokenID()), Name: request.Name, Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: string(request.Scope), CreatedAt: now, UpdatedAt: now, ExpiresAt: request.ExpiresAt, ProjectAccess: "selected", Kind: "service"}
	if row.ID == "" {
		return tokenResponse{}, nativePublicInvalid("Token identity is required")
	}
	var scope nativeScope
	if request.Issuer != nil {
		issuer := *request.Issuer
		if issuer.Hosted == nil || issuer.Hosted.SupportActor != "" || issuer.HostedKeyScope != "" {
			return tokenResponse{}, auth.ErrHostedIdentity
		}
		if request.Scope != apiScopeOperator && request.Scope != apiScopeAdmin || (request.Scope == apiScopeAdmin) != (request.KeyScope == apikey.ScopeAdmin) || !hostedRoleAllows(issuer.HostedRole, request.KeyScope, "credentials") {
			return tokenResponse{}, auth.ErrHostedIdentity
		}
		if request.ExpiresAt != nil && (!request.ExpiresAt.After(now) || request.ExpiresAt.After(now.Add(90*24*time.Hour))) {
			return tokenResponse{}, nativePublicInvalid("Select no expiry or an expiry within 90 days")
		}
		access, message := resolveHostedProjectAccess(request.ProjectAccess, request.ProjectIDs)
		if message != "" {
			return tokenResponse{}, nativePublicInvalid(message)
		}
		identity, current, err := s.hostedMutationIdentity(ctx, issuer)
		if err != nil {
			return tokenResponse{}, operatorAuthorizationError(err)
		}
		if identity.Subject != issuer.Hosted.Subject || current.ID != issuer.HostedMembership {
			return tokenResponse{}, auth.ErrHostedIdentity
		}
		row.Kind = request.Kind
		if row.Kind == "" {
			row.Kind = "personal"
		}
		if row.Kind != "personal" && row.Kind != "service" {
			return tokenResponse{}, nativePublicInvalid("Choose a personal or service key")
		}
		row.NativeOnly, row.UserID, row.MembershipID, row.KeyScope, row.ProjectAccess = true, issuer.Hosted.Subject, issuer.HostedMembership, string(request.KeyScope), string(access)
		scope = nativeScope{organization: row.OrganizationID, credential: issuer, action: "credentials", requireHostedAdmin: request.KeyScope == apikey.ScopeAdmin}
	}
	err = s.accessWrite(ctx, row.OrganizationID, func(w tenantstore.Writer) error {
		if request.Issuer != nil {
			member, err := w.AuthorizeAccess(ctx, s.accessAuthority(scope, false), now)
			if err != nil {
				return err
			}
			if !hostedRoleAllows(lesserHostedRole(member.Role, scope.credential.HostedRole), request.KeyScope, "credentials") {
				return auth.ErrHostedIdentity
			}
			for _, project := range request.ProjectIDs {
				scope.project = tracker.ProjectID(project)
				if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
					return err
				}
			}
		}
		if err := w.CreateAccessCredential(ctx, row); err != nil {
			return err
		}
		for _, project := range request.ProjectIDs {
			if err := w.SetAccessTokenGrant(ctx, accessstore.TokenGrant{OrganizationID: row.OrganizationID, TokenID: row.ID, ProjectID: tracker.ProjectID(project)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return tokenResponse{}, err
	}
	response := accessTokenResponse(row)
	response.Token = token
	response.Projects = append([]string{}, request.ProjectIDs...)
	return response, nil
}

func (s *Service) rotateAccessToken(ctx context.Context, id string) (tokenResponse, error) {
	token, err := s.config.generateToken()
	if err != nil {
		return tokenResponse{}, err
	}
	now := s.config.now()
	hash := apikey.HashToken(token)
	var response tokenResponse
	err = s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		if err := w.RotateAccessCredential(ctx, id, hash, tokenFingerprint(hash), now); err != nil {
			return err
		}
		row, err := w.AccessCredential(ctx, id)
		response = accessTokenResponse(row)
		return err
	})
	if err != nil {
		return tokenResponse{}, err
	}
	response.Token = token
	return response, nil
}

func (s *Service) accessHostedKeys(ctx context.Context, credential apiCredential) ([]hostedAPIKey, error) {
	credential, err := s.hostedKeyCredentialFor(ctx, credential)
	if err != nil {
		return nil, err
	}
	result := []hostedAPIKey{}
	var after string
	for {
		var rows []accessstore.Credential
		err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, nativeScope{organization: s.accessOrganization(), credential: credential}, false); err != nil {
				return err
			}
			var err error
			rows, err = r.AccessMemberCredentials(ctx, credential.Hosted.Subject, after, 200)
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			after = row.ID
			if row.UserID != credential.Hosted.Subject || row.MembershipID != credential.HostedMembership {
				continue
			}
			key := hostedAPIKey{Kind: row.Kind, ProjectAccess: hostedProjectAccess(row.ProjectAccess), ID: row.ID, Name: row.Name, Scope: apikey.Scope(row.KeyScope), CreatedAt: formatHubTime(row.CreatedAt), Fingerprint: row.Fingerprint, Revoked: row.RevokedAt != nil, Projects: []string{}}
			if row.ExpiresAt != nil {
				key.Expiry = new(formatHubTime(*row.ExpiresAt))
			}
			if row.RevokedAt != nil {
				key.RevokedAt = new(formatHubTime(*row.RevokedAt))
			}
			if row.ProjectAccess == "selected" {
				var project tracker.ProjectID
				for {
					var grants []accessstore.TokenGrant
					if err := s.accessRead(ctx, row.OrganizationID, func(r tenantstore.Reader) error {
						var err error
						grants, err = r.AccessTokenGrants(ctx, row.ID, project, 200)
						return err
					}); err != nil {
						return nil, err
					}
					for _, grant := range grants {
						project = grant.ProjectID
						key.Projects = append(key.Projects, string(project))
					}
					if len(grants) < 200 {
						break
					}
				}
			}
			result = append(result, key)
		}
		if len(rows) < 200 {
			return result, nil
		}
	}
}
