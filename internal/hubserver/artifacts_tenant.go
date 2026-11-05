package hubserver

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) artifactReadGrantTenant(ctx context.Context, scope nativeScope, item, artifactID string, revision int64, now time.Time) (result artifact.Grant, err error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	if scope.credential.Hosted != nil {
		if _, _, err := s.hostedMutationIdentity(ctx, scope.credential); err != nil {
			return result, err
		}
	}
	token, err := apikey.GenerateToken()
	if err != nil {
		return result, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
			return err
		}
		if _, err := w.AccessTokenGrant(ctx, scope.credential.ID, scope.project); err != nil {
			return err
		}
		ref, err := w.ArtifactReference(ctx, scope.project, tracker.NativeWorkItemID(item), artifactID, revision)
		if err != nil {
			return err
		}
		binding, err := w.ArtifactService(ctx, scope.project, ref.ServiceID)
		if err != nil {
			return err
		}
		if !artifact.ValidOrigin(binding.Origin) || ref.Availability != "available" {
			return artifact.ErrMissing
		}
		expires := minTime(now.Add(time.Minute), ref.ExpiresAt)
		if scope.credential.Hosted != nil {
			expires = minTime(expires, scope.credential.Hosted.ExpiresAt)
		}
		if !now.Before(expires) {
			return artifact.ErrExpired
		}
		result = artifact.Grant{Token: token, Origin: binding.Origin, ArtifactID: ref.ArtifactID, Revision: ref.Revision, SHA256: ref.SHA256, ExpiresAt: expires}
		return w.CreateArtifactReadGrant(ctx, artifactstore.ReadGrant{ProjectID: scope.project, TokenHash: apikey.HashToken(token), PrincipalID: scope.credential.ID, ServiceID: ref.ServiceID, ArtifactID: ref.ArtifactID, Revision: ref.Revision, ExpiresAt: expires, SessionHash: scope.credential.SessionHash})
	})
	return result, projectRecordError(err)
}

func (s *Service) readArtifactGrantTenant(ctx context.Context, scope nativeScope, service string, request artifact.ReadAuthorization) (grant artifactstore.ReadGrant, err error) {
	err = s.config.TenantRepository.ReadTenant(ctx, scope.organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		grant, err = r.ArtifactReadGrant(ctx, scope.project, apikey.HashToken(request.Token))
		if err != nil {
			return err
		}
		if grant.ServiceID != service || grant.ArtifactID != request.ArtifactID || grant.Revision != request.Revision || !s.config.now().Before(grant.ExpiresAt) {
			return corestore.ErrNotFound
		}
		credential, err := r.AccessCredential(ctx, grant.PrincipalID)
		if err != nil {
			return err
		}
		if !credential.Valid(credential.Hash, s.config.now()) {
			return corestore.ErrNotFound
		}
		_, err = r.AccessTokenGrant(ctx, grant.PrincipalID, scope.project)
		return err
	})
	return grant, err
}
