package hubserver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func accessTokenResponse(row accessstore.Credential) hubapi.TokenResponse {
	result := hubapi.TokenResponse{Kind: row.Kind, ProjectAccess: hubapi.HostedProjectAccess(row.ProjectAccess), Projects: []string{}, Grants: []hubapi.TokenGrantResponse{}, ExpiresAt: row.ExpiresAt, KeyScope: apikey.Scope(row.KeyScope), NativeOnly: row.NativeOnly, RevokedAt: row.RevokedAt, ID: row.ID, Name: row.Name, Scope: hubapi.APIScope(row.Scope), Fingerprint: row.Fingerprint, CreatedAt: row.CreatedAt}
	if row.RotatedAt != nil {
		result.RotatedAt = *row.RotatedAt
	}
	return result
}

func (s *Service) createAccessToken(ctx context.Context, request hubapi.TokenRequest) (hubapi.TokenResponse, error) {
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || len(request.Name) > 200 || !validAPIScope(request.Scope) {
		return hubapi.TokenResponse{}, hubapi.NativePublicInvalid("Token name and scope are required")
	}
	now := s.config.Clock()
	token, err := s.config.Deps.GenerateToken()
	if err != nil {
		return hubapi.TokenResponse{}, err
	}
	hash := apikey.HashToken(token)
	row := accessstore.Credential{OrganizationID: s.accessOrganization(), ID: strings.TrimSpace(s.config.Deps.NewTokenID()), Name: request.Name, Hash: hash, Fingerprint: hubapi.TokenFingerprint(hash), Scope: string(request.Scope), CreatedAt: now, UpdatedAt: now, ExpiresAt: request.ExpiresAt, ProjectAccess: "selected", Kind: "service"}
	if row.ID == "" {
		return hubapi.TokenResponse{}, hubapi.NativePublicInvalid("Token identity is required")
	}
	var scope hubscope.NativeScope
	if request.Issuer != nil {
		issuer := *request.Issuer
		if issuer.Hosted == nil || issuer.Hosted.SupportActor != "" || issuer.HostedKeyScope != "" {
			return hubapi.TokenResponse{}, auth.ErrHostedIdentity
		}
		if request.Scope != hubapi.APIScopeOperator && request.Scope != hubapi.APIScopeAdmin || (request.Scope == hubapi.APIScopeAdmin) != (request.KeyScope == apikey.ScopeAdmin) || !hubscope.HostedRoleAllows(issuer.HostedRole, request.KeyScope, "credentials") {
			return hubapi.TokenResponse{}, hubapi.NativePublicInvalid("Select a scope allowed by your current role")
		}
		if request.ExpiresAt != nil && (!request.ExpiresAt.After(now) || request.ExpiresAt.After(now.Add(90*24*time.Hour))) {
			return hubapi.TokenResponse{}, hubapi.NativePublicInvalid("Select no expiry or an expiry within 90 days")
		}
		access, message := resolveHostedProjectAccess(request.ProjectAccess, request.ProjectIDs)
		if message != "" {
			return hubapi.TokenResponse{}, hubapi.NativePublicInvalid(message)
		}
		identity, current, err := s.hostedMutationIdentity(ctx, issuer)
		if err != nil {
			return hubapi.TokenResponse{}, hubscope.OperatorAuthorizationError(err)
		}
		if identity.Subject != issuer.Hosted.Subject || current.ID != issuer.HostedMembership {
			return hubapi.TokenResponse{}, auth.ErrHostedIdentity
		}
		row.Kind = request.Kind
		if row.Kind == "" {
			row.Kind = "personal"
		}
		if row.Kind != "personal" && row.Kind != "service" {
			return hubapi.TokenResponse{}, hubapi.NativePublicInvalid("Choose a personal or service key")
		}
		row.NativeOnly, row.UserID, row.MembershipID, row.KeyScope, row.ProjectAccess = true, issuer.Hosted.Subject, issuer.HostedMembership, string(request.KeyScope), string(access)
		scope = hubscope.NativeScope{Organization: row.OrganizationID, Credential: issuer, Action: "credentials", RequireHostedAdmin: request.KeyScope == apikey.ScopeAdmin}
	}
	err = s.accessWrite(ctx, row.OrganizationID, func(w tenantstore.Writer) error {
		if err := s.authorizeCredentialMaintenanceWrite(ctx, w, row.OrganizationID); err != nil {
			return err
		}
		if request.Issuer != nil {
			member, err := w.AuthorizeAccess(ctx, s.accessAuthority(scope, false), now)
			if err != nil {
				return err
			}
			if !hubscope.HostedRoleAllows(hubapi.LesserHostedRole(member.Role, scope.Credential.HostedRole), request.KeyScope, "credentials") {
				return auth.ErrHostedIdentity
			}
			for _, project := range request.ProjectIDs {
				if !hostedplans.SafeID(project) {
					return hubapi.NativeNotFound()
				}
				scope.Project = tracker.ProjectID(project)
				if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
					return err
				}
			}
		}
		if err := w.CreateAccessCredential(ctx, row); err != nil {
			if errors.Is(err, corestore.ErrUniqueViolation) {
				return hubapi.NativeConflict(0)
			}
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
		return hubapi.TokenResponse{}, err
	}
	response := accessTokenResponse(row)
	response.Token = token
	response.Projects = append([]string{}, request.ProjectIDs...)
	return response, nil
}

func (s *Service) rotateAccessToken(ctx context.Context, id string) (hubapi.TokenResponse, error) {
	token, err := s.config.Deps.GenerateToken()
	if err != nil {
		return hubapi.TokenResponse{}, err
	}
	now := s.config.Clock()
	hash := apikey.HashToken(token)
	var response hubapi.TokenResponse
	err = s.accessWrite(ctx, s.accessOrganization(), func(w tenantstore.Writer) error {
		if err := s.authorizeCredentialMaintenanceWrite(ctx, w, s.accessOrganization()); err != nil {
			return err
		}
		if err := w.RotateAccessCredential(ctx, id, hash, hubapi.TokenFingerprint(hash), now); err != nil {
			return err
		}
		row, err := w.AccessCredential(ctx, id)
		response = accessTokenResponse(row)
		return err
	})
	if err != nil {
		return hubapi.TokenResponse{}, err
	}
	response.Token = token
	return response, nil
}

func (s *Service) accessHostedKeys(ctx context.Context, credential hubapi.APICredential) ([]hostedAPIKey, error) {
	credential, err := s.hostedKeyCredentialFor(ctx, credential)
	if err != nil {
		return nil, err
	}
	result := []hostedAPIKey{}
	var after string
	for {
		var rows []accessstore.Credential
		err := s.accessRead(ctx, s.accessOrganization(), func(r tenantstore.Reader) error {
			if err := s.authorizeAccessScope(ctx, r, hubscope.NativeScope{Organization: s.accessOrganization(), Credential: credential}, false); err != nil {
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
			key := hostedAPIKey{Kind: row.Kind, ProjectAccess: hubapi.HostedProjectAccess(row.ProjectAccess), ID: row.ID, Name: row.Name, Scope: apikey.Scope(row.KeyScope), CreatedAt: hubapi.FormatHubTime(row.CreatedAt), Fingerprint: row.Fingerprint, Revoked: row.RevokedAt != nil, Projects: []string{}}
			if row.ExpiresAt != nil {
				key.Expiry = new(hubapi.FormatHubTime(*row.ExpiresAt))
			}
			if row.RevokedAt != nil {
				key.RevokedAt = new(hubapi.FormatHubTime(*row.RevokedAt))
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
