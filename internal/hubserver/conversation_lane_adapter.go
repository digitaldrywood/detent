package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func (s *Service) registerArtifactRoutes(e *echo.Echo) {
	read := s.requireNativeScope(hubapi.APIScopeOperator, hubapi.APIScopeWorker)
	e.PUT(hubapi.NativeBase+"/artifact-services/:service", s.bindArtifactService, s.requireNativeScope(hubapi.APIScopeAdmin))
	e.GET(hubapi.NativeBase+"/artifact-services", s.artifactServices, read)
	e.POST(hubapi.NativeBase+"/artifact-services/:service/receipts", s.artifactReceipt, read)
	e.POST(hubapi.NativeBase+"/artifact-services/:service/authorize", s.authorizeArtifactRead, read)
	e.POST(hubapi.NativeBase+"/work-items/:item/artifact-authority", s.authorizeArtifactUpload, s.requireNativeScope(hubapi.APIScopeWorker))
	e.GET(hubapi.NativeBase+"/work-items/:item/artifacts", s.artifactReferences, read)
	e.GET(hubapi.NativeBase+"/work-items/:item/artifacts/:artifact/revisions/:revision", s.getArtifactReference, read)
	e.POST(hubapi.NativeBase+"/work-items/:item/artifacts/:artifact/access", s.artifactReadGrant, read)
}

func (s *Service) artifactPublisherToken(ctx context.Context, scope hubscope.NativeScope, service string) (id string, err error) {
	if s.config.TenantRepository == nil {
		return "", hubapi.NativeWorkItemStorageUnavailable()
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		binding, err := r.ArtifactService(ctx, scope.Project, service)
		id = binding.PublisherTokenID
		return err
	})
	return id, err
}

func (s *Service) readArtifactReferences(ctx context.Context, scope hubscope.NativeScope, item string) ([]artifact.Reference, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	row, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return nil, err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
		return nil, hubapi.NativeNotFound()
	}
	var refs []artifact.Reference
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		refs, err = r.ArtifactReferences(ctx, scope.Project, tracker.NativeWorkItemID(item), 64)
		return err
	})
	if err != nil {
		return nil, hubapi.ProjectRecordError(err)
	}
	for i := range refs {
		if !s.config.Clock().Before(refs[i].ExpiresAt) {
			refs[i].Availability = "expired"
		}
	}
	return refs, nil
}

func (s *Service) hostedArtifactReader(c echo.Context, principal, sessionHash string) (hubapi.APICredential, error) {
	ctx := c.Request().Context()
	readCtx, cancelRead := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancelRead()
	if sessionHash == "" {
		scope := hubscope.NativeRequestScope(c)
		var stored accessstore.Credential
		err := s.config.TenantRepository.ReadTenant(readCtx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			stored, err = r.AccessCredential(readCtx, principal)
			if err != nil {
				return err
			}
			member, err := r.AccessCredentialHasMember(readCtx, principal)
			if err != nil {
				return err
			}
			if member {
				return auth.ErrHostedIdentity
			}
			return nil
		})
		if err != nil {
			return hubapi.APICredential{}, err
		}
		credential, _, err := s.authenticateTenantAPIHash(readCtx, scope.Organization, stored.Hash, "", "")
		if err != nil || credential.ID != principal {
			return hubapi.APICredential{}, auth.ErrHostedIdentity
		}
		return credential, nil
	}
	session, err := s.WebSession(ctx, sessionHash, s.config.Clock())
	if err != nil {
		return hubapi.APICredential{}, err
	}
	if s.config.Hosted.SharedEntry == nil && hostedplans.EmailListed(s.config.Hosted.StaffEmails, session.Email) && (session.Identity == nil || session.Identity.SupportActor == "") {
		return hubapi.APICredential{}, auth.ErrHostedIdentity
	}
	credential, _, err := s.hostedSessionCredential(ctx, session, sessionHash)
	if err != nil || credential.ID != principal {
		return hubapi.APICredential{}, auth.ErrHostedIdentity
	}
	scope := hubscope.NativeRequestScope(c)
	scope.Credential = credential
	if s.config.TenantRepository == nil {
		return hubapi.APICredential{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	if err := s.config.TenantRepository.ReadTenant(readCtx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		return s.authorizeAccessScope(readCtx, r, scope, false)
	}); err != nil {
		return hubapi.APICredential{}, err
	}
	return credential, s.hostedAudit(ctx, session.Identity, "action", c.Request().Method+" "+c.Path(), string(scope.Project), 0)
}

func (s *Service) artifactServicesCommand(ctx context.Context, scope hubscope.NativeScope) (result []artifact.Binding, err error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	if s.config.TenantRepository == nil {
		return nil, hubapi.NativeWorkItemStorageUnavailable()
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		result, err = r.ArtifactServices(ctx, scope.Project)
		return err
	})
	for i := range result {
		result[i].PublisherTokenID = ""
	}
	return result, hubapi.ProjectRecordError(err)
}

func (s *Service) artifactReadGrantCommand(ctx context.Context, scope hubscope.NativeScope, item, artifactID string, revision int64, now time.Time) (artifact.Grant, error) {
	if s.config.TenantRepository == nil {
		return artifact.Grant{}, hubapi.NativeWorkItemStorageUnavailable()
	}
	return s.artifactReadGrantTenant(ctx, scope, item, artifactID, revision, now)
}

func (s *Service) artifactReferenceCommand(ctx context.Context, scope hubscope.NativeScope, item, id string, revision int64, now time.Time) (artifact.Reference, error) {
	var result artifact.Reference
	if s.config.TenantRepository == nil {
		return result, hubapi.NativeWorkItemStorageUnavailable()
	}
	row, _, err := s.readWorkItemRecord(ctx, scope, item)
	if err != nil {
		return result, err
	}
	if !scope.ReporterOwns(tracker.Actor{PrincipalID: hubapi.StringValue(row.ActorPrincipalID)}) {
		return result, hubapi.NativeNotFound()
	}
	err = s.config.TenantRepository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		result, err = r.ArtifactReference(ctx, scope.Project, tracker.NativeWorkItemID(item), id, revision)
		return err
	})
	if err != nil {
		return artifact.Reference{}, hubapi.ProjectRecordError(err)
	}
	if !now.Before(result.ExpiresAt) {
		result.Availability = "expired"
	}
	return result, nil
}

func (s *Service) hostedArtifactPublisher(c echo.Context, credential hubapi.APICredential) bool {
	return s.artifactHTTP().HostedArtifactPublisher(c, credential)
}

func (s *Service) bindArtifactService(c echo.Context) error {
	return s.artifactHTTP().BindArtifactService(c)
}

func (s *Service) artifactServices(c echo.Context) error {
	return s.artifactHTTP().ArtifactServices(c)
}

func (s *Service) artifactReceipt(c echo.Context) error {
	return s.artifactHTTP().ArtifactReceipt(c)
}

func (s *Service) artifactReferences(c echo.Context) error {
	return s.artifactHTTP().ArtifactReferences(c)
}

func (s *Service) authorizeArtifactUpload(c echo.Context) error {
	return s.artifactHTTP().AuthorizeArtifactUpload(c)
}

func (s *Service) artifactReadGrant(c echo.Context) error {
	return s.artifactHTTP().ArtifactReadGrant(c)
}

func (s *Service) authorizeArtifactRead(c echo.Context) error {
	return s.artifactHTTP().AuthorizeArtifactRead(c)
}

func (s *Service) getArtifactReference(c echo.Context) error {
	return s.artifactHTTP().GetArtifactReference(c)
}

func artifactReadGrantRequest(c echo.Context) bool {
	return conversations.ArtifactReadGrantRequest(c)
}

func (s *Service) artifactHTTP() conversations.ArtifactHTTP {
	return conversations.NewArtifactHTTP(conversations.ArtifactHTTPDeps{
		StorageAvailable: s.config.TenantRepository != nil,
		Hosted:           s.config.Hosted != nil,
		Now:              s.config.Clock,
		APIError:         s.nativeAPIError,
		InvalidRequest:   s.invalidAPIRequest,
		Refused:          s.refusedAPIError,
		Bind: func(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, req conversations.BindArtifactServiceRequest) (json.RawMessage, error) {
			return s.executeProjectRecordMutation(ctx, scope, options, req.Mutation, req)
		},
		PublisherToken:  s.artifactPublisherToken,
		References:      s.readArtifactReferences,
		HostedReader:    s.hostedArtifactReader,
		Services:        s.artifactServicesCommand,
		ReadGrant:       s.artifactReadGrantCommand,
		Reference:       s.artifactReferenceCommand,
		PublishReceipt:  s.publishArtifactReceiptTenant,
		AuthorizeUpload: s.authorizeArtifactUploadTenant,
		ReadStoredGrant: s.readArtifactGrantTenant,
	})
}
