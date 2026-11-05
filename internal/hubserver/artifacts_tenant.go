package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
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

func (s *Service) authorizeArtifactUploadTenant(ctx context.Context, scope nativeScope, reservation artifact.Reservation) error {
	if s.config.TenantRepository == nil {
		return workItemStorageUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	command := tracker.Mutation{LeaseID: tracker.LeaseID(reservation.LeaseID), FencingToken: tracker.FencingToken(reservation.FencingToken)}
	pinned, requirements, err := s.dependencyReasonPolicy(ctx, scope, command.LeaseID)
	if err != nil {
		return projectRecordError(err)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		binding, err := w.ArtifactService(ctx, scope.project, reservation.ServiceID)
		if err != nil {
			return err
		}
		if binding.Mode != reservation.Mode || binding.HostedOptIn != reservation.HostedOptIn {
			return nativeNotFound()
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		item, err := w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(reservation.WorkItemID))
		if err != nil {
			return err
		}
		if item.Archived {
			return nativeNotFound()
		}
		locker, ok := w.(leasestore.FencedLeaseLocker)
		if !ok {
			return workItemStorageUnavailable()
		}
		if _, err := locker.LockLease(ctx, reservation.LeaseID); err != nil {
			return err
		}

		if err := s.authorizeDependencyReasonLease(ctx, w, scope, item, command, pinned, requirements, s.config.now().UTC()); err != nil {
			return err
		}
		attempt, err := w.Attempt(ctx, reservation.AttemptID)
		if err != nil {
			return err
		}
		if attempt.IssueID != item.ID || attempt.LeaseID != reservation.LeaseID || attempt.FencingToken != reservation.FencingToken || attempt.RunID != reservation.RunID || attempt.Status != "running" {
			return nativeNotFound()
		}
		return nil
	})
	return projectRecordError(err)
}

func (s *Service) publishArtifactReceiptTenant(ctx context.Context, scope nativeScope, reference artifact.Reference) error {
	if s.config.TenantRepository == nil {
		return workItemStorageUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	err := s.config.TenantRepository.WriteTenant(ctx, scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		binding, err := w.ArtifactService(ctx, scope.project, reference.ServiceID)
		if err != nil {
			return err
		}
		if binding.PublisherTokenID != scope.credential.ID {
			return nativeNotFound()
		}
		item, err := w.LockWorkItem(ctx, scope.project, tracker.NativeWorkItemID(reference.WorkItemID))
		if err != nil {
			return err
		}
		attempt, err := w.Attempt(ctx, reference.AttemptID)
		if err != nil {
			return err
		}
		if attempt.IssueID != item.ID || attempt.RunID != reference.RunID {
			return nativeNotFound()
		}
		if reference.VersionID != "" {
			matches, err := w.ArtifactVersionAttemptMatches(ctx, scope.project, reference.VersionID, reference.AttemptID)
			if err != nil {
				return err
			}
			if !matches {
				return nativeNotFound()
			}
		}
		_, readErr := w.ArtifactReference(ctx, scope.project, tracker.NativeWorkItemID(reference.WorkItemID), reference.ArtifactID, reference.Revision)
		if readErr != nil && !errors.Is(readErr, corestore.ErrNotFound) {
			return readErr
		}
		if _, err = w.AppendArtifactReference(ctx, reference); err != nil {
			return err
		}
		if readErr == nil {
			return nil
		}
		return s.applyRunnerTenantUsage(ctx, w, s.config.now(), "artifact-receipt/"+reference.ManifestID, false, false)
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return nativeConflict(0)
	}
	return projectRecordError(err)
}
