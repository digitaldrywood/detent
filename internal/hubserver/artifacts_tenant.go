package hubserver

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *Service) artifactReadGrantTenant(ctx context.Context, scope hubscope.NativeScope, item, artifactID string, revision int64, now time.Time) (result artifact.Grant, err error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if scope.Credential.Hosted != nil {
		session, err := s.WebSession(ctx, scope.Credential.SessionHash, now)
		if err != nil {
			return result, err
		}
		if s.config.Hosted.SharedEntry == nil && hostedplans.EmailListed(s.config.Hosted.StaffEmails, session.Email) && (session.Identity == nil || session.Identity.SupportActor == "") {
			return result, auth.ErrHostedIdentity
		}
		if _, _, err := s.hostedMutationIdentity(ctx, scope.Credential); err != nil {
			return result, err
		}
	}
	token, err := apikey.GenerateToken()
	if err != nil {
		return result, err
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, false); err != nil {
			return err
		}
		if _, err := w.AccessTokenGrant(ctx, scope.Credential.ID, scope.Project); err != nil {
			return err
		}
		workItem, err := w.WorkItem(ctx, scope.Project, tracker.NativeWorkItemID(item))
		if err != nil {
			return err
		}
		if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(workItem.ActorPrincipalID)}) {
			return hubapi.NativeNotFound()
		}
		ref, err := w.ArtifactReference(ctx, scope.Project, tracker.NativeWorkItemID(item), artifactID, revision)
		if err != nil {
			return err
		}
		binding, err := w.ArtifactService(ctx, scope.Project, ref.ServiceID)
		if err != nil {
			return err
		}
		if !artifact.ValidOrigin(binding.Origin) || ref.Availability != "available" {
			return artifact.ErrMissing
		}
		expires := hubapi.MinTime(now.Add(time.Minute), ref.ExpiresAt)
		if scope.Credential.Hosted != nil {
			expires = hubapi.MinTime(expires, scope.Credential.Hosted.ExpiresAt)
		}
		if !now.Before(expires) {
			return artifact.ErrExpired
		}
		result = artifact.Grant{Token: token, Origin: binding.Origin, ArtifactID: ref.ArtifactID, Revision: ref.Revision, SHA256: ref.SHA256, ExpiresAt: expires}
		return w.CreateArtifactReadGrant(ctx, artifactstore.ReadGrant{ProjectID: scope.Project, TokenHash: apikey.HashToken(token), PrincipalID: scope.Credential.ID, ServiceID: ref.ServiceID, ArtifactID: ref.ArtifactID, Revision: ref.Revision, ExpiresAt: expires, SessionHash: scope.Credential.SessionHash}, now)
	})
	return result, hubapi.ProjectRecordError(err)
}

func (s *Service) readArtifactGrantTenant(ctx context.Context, scope hubscope.NativeScope, service string, request artifact.ReadAuthorization) (grant artifactstore.ReadGrant, owner tracker.Actor, err error) {
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		grant, err = r.ArtifactReadGrant(ctx, scope.Project, apikey.HashToken(request.Token))
		if err != nil {
			return err
		}
		if grant.ServiceID != service || grant.ArtifactID != request.ArtifactID || grant.Revision != request.Revision || !s.config.Clock().Before(grant.ExpiresAt) {
			return corestore.ErrNotFound
		}
		credential, err := r.AccessCredential(ctx, grant.PrincipalID)
		if err != nil {
			return err
		}
		if !credential.Valid(credential.Hash, s.config.Clock()) {
			return corestore.ErrNotFound
		}
		if _, err := r.AccessTokenGrant(ctx, grant.PrincipalID, scope.Project); err != nil {
			return err
		}
		item, err := r.WorkItem(ctx, scope.Project, grant.WorkItemID)
		owner.PrincipalID = hubapi.StringValue(item.ActorPrincipalID)
		return err
	})
	return grant, owner, err
}

func (s *Service) authorizeArtifactUploadTenant(ctx context.Context, scope hubscope.NativeScope, reservation artifact.Reservation) error {
	if s.config.TenantRepository == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	command := tracker.Mutation{LeaseID: tracker.LeaseID(reservation.LeaseID), FencingToken: tracker.FencingToken(reservation.FencingToken)}
	pinned, requirements, err := s.dependencyReasonPolicy(ctx, scope, command.LeaseID)
	if err != nil {
		return hubapi.ProjectRecordError(err)
	}
	err = s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		binding, err := w.ArtifactService(ctx, scope.Project, reservation.ServiceID)
		if err != nil {
			return err
		}
		if binding.Mode != reservation.Mode || binding.HostedOptIn != reservation.HostedOptIn {
			return hubapi.NativeNotFound()
		}
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		item, err := w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(reservation.WorkItemID))
		if err != nil {
			return err
		}
		if item.Archived {
			return hubapi.NativeNotFound()
		}
		if err := s.authorizeDependencyReasonLease(ctx, w, scope, item, command, pinned, requirements, s.config.Clock().UTC()); err != nil {
			return err
		}
		attempt, err := w.Attempt(ctx, reservation.AttemptID)
		if err != nil {
			return err
		}
		if attempt.IssueID != item.ID || attempt.LeaseID != reservation.LeaseID || attempt.FencingToken != reservation.FencingToken || attempt.RunID != reservation.RunID || attempt.Status != "running" {
			return hubapi.NativeNotFound()
		}
		return w.AuthorizeArtifactUpload(ctx, reservation, s.config.Clock().UTC())
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return hubapi.NativeConflict(0)
	}
	return hubapi.ProjectRecordError(err)
}

func (s *Service) publishArtifactReceiptTenant(ctx context.Context, scope hubscope.NativeScope, reference artifact.Reference) error {
	if s.config.TenantRepository == nil {
		return hubapi.NativeWorkItemStorageUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	err := s.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := s.authorizeAccessScope(ctx, w, scope, true); err != nil {
			return err
		}
		binding, err := w.ArtifactService(ctx, scope.Project, reference.ServiceID)
		if err != nil {
			return err
		}
		if binding.PublisherTokenID != scope.Credential.ID {
			return hubapi.NativeNotFound()
		}
		item, err := w.LockWorkItem(ctx, scope.Project, tracker.NativeWorkItemID(reference.WorkItemID))
		if err != nil {
			return err
		}
		attempt, err := w.Attempt(ctx, reference.AttemptID)
		if err != nil {
			return err
		}
		if attempt.IssueID != item.ID || attempt.RunID != reference.RunID {
			return hubapi.NativeNotFound()
		}
		if reference.VersionID != "" {
			matches, err := w.ArtifactVersionAttemptMatches(ctx, scope.Project, reference.VersionID, reference.AttemptID)
			if err != nil {
				return err
			}
			if !matches {
				return hubapi.NativeNotFound()
			}
		}
		_, readErr := w.ArtifactReference(ctx, scope.Project, tracker.NativeWorkItemID(reference.WorkItemID), reference.ArtifactID, reference.Revision)
		if readErr != nil && !errors.Is(readErr, corestore.ErrNotFound) {
			return readErr
		}
		if readErr != nil {
			if err := w.ConsumeArtifactUpload(ctx, reference, s.config.Clock().UTC()); err != nil {
				return err
			}
		}
		if _, err = w.AppendArtifactReference(ctx, reference); err != nil {
			return err
		}
		if readErr == nil {
			return nil
		}
		return s.applyRunnerTenantUsage(ctx, w, s.config.Clock(), "artifact-receipt/"+reference.ManifestID, false, false)
	})
	if errors.Is(err, corestore.ErrRevisionConflict) {
		return hubapi.NativeConflict(0)
	}
	return hubapi.ProjectRecordError(err)
}
