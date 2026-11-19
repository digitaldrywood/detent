package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/artifact/cataloghub"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/threads"
	"github.com/digitaldrywood/detent/internal/operatortool"
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

func (s *Service) registerCloudAttachmentRoutes(e *echo.Echo) {
	if !s.hostedShared() {
		return
	}
	s.registerAttachmentArchiveRoutes(e)
	read := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	write := s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator)
	e.POST(attachmentMetadataBase+"/check", s.checkCloudAttachment, write)
	e.POST(attachmentMetadataBase, s.recordCloudAttachment, write)
	e.POST(hubapi.NativeBase+"/attempts/:attempt/evidence/check", s.checkCloudAttachment, s.requireNativeScope(hubapi.APIScopeWorker))
	e.POST(hubapi.NativeBase+"/attempts/:attempt/evidence", s.recordCloudAttachment, s.requireNativeScope(hubapi.APIScopeWorker))
	e.GET(attachmentMetadataBase+"/:attachment", s.getCloudAttachment, read)
	e.DELETE(attachmentMetadataBase+"/:attachment", s.deleteCloudAttachment, s.requireNativeScope(hubapi.APIScopeOperator))
	e.POST(attachmentMetadataBase+"/:attachment/reference", s.referenceCloudAttachment, write)
	e.POST("/internal/v1/attachments/expired", s.expiredCloudAttachments)
	e.POST("/internal/v1/attachments/deleted", s.finishCloudAttachmentDeletion)
	e.POST("/internal/v1/attachments/exists", s.cloudAttachmentExists)
}

func (s *Service) readCloudAttachment(ctx context.Context, scope hubscope.NativeScope, id string) (attachment.Metadata, error) {
	return s.readAttachmentRecordTenant(ctx, scope, id)
}

func (s *Service) referenceCloudAttachmentCommand(ctx context.Context, scope hubscope.NativeScope, id string, input attachment.SourceReference, fence tracker.Mutation) error {
	return s.referenceCloudAttachmentTenant(ctx, scope, id, input, fence)
}

func (s *Service) checkCloudAttachment(c echo.Context) error {
	return s.attachmentHTTP().CheckCloudAttachment(c)
}

func (s *Service) recordCloudAttachment(c echo.Context) error {
	return s.attachmentHTTP().RecordCloudAttachment(c)
}

func (s *Service) getCloudAttachment(c echo.Context) error {
	return s.attachmentHTTP().GetCloudAttachment(c)
}

func (s *Service) deleteCloudAttachment(c echo.Context) error {
	return s.attachmentHTTP().DeleteCloudAttachment(c)
}

func (s *Service) deleteCloudAttachmentCommand(ctx context.Context, scope hubscope.NativeScope, id string) error {
	return s.attachmentHTTP().DeleteCloudAttachmentCommand(ctx, scope, id)
}

func (s *Service) referenceCloudAttachment(c echo.Context) error {
	return s.attachmentHTTP().ReferenceCloudAttachment(c)
}

func (s *Service) expiredCloudAttachments(c echo.Context) error {
	return s.attachmentHTTP().ExpiredCloudAttachments(c)
}

func (s *Service) finishCloudAttachmentDeletion(c echo.Context) error {
	return s.attachmentHTTP().FinishCloudAttachmentDeletion(c)
}

func (s *Service) cloudAttachmentExists(c echo.Context) error {
	return s.attachmentHTTP().CloudAttachmentExists(c)
}

func (s *Service) registerAttachmentArchiveRoutes(e *echo.Echo) {
	if !s.hostedShared() {
		return
	}
	base := attachmentMetadataBase + "/archives/:item"
	e.POST(base+"/check", s.checkAttachmentArchive, s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator))
	e.POST(base, s.recordAttachmentArchive, s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator))
	e.GET(base+"/:digest", s.readAttachmentArchive, s.requireNativeScope(hubapi.APIScopeWorker, hubapi.APIScopeOperator))
}

func (s *Service) checkAttachmentArchive(c echo.Context) error {
	return s.attachmentArchiveHTTP().CheckAttachmentArchive(c)
}

func (s *Service) recordAttachmentArchive(c echo.Context) error {
	return s.attachmentArchiveHTTP().RecordAttachmentArchive(c)
}

func (s *Service) readAttachmentArchive(c echo.Context) error {
	return s.attachmentArchiveHTTP().ReadAttachmentArchive(c)
}

func (s *Service) getCloudContentSettings(c echo.Context) error {
	return s.contentSettingsHTTP().GetCloudContentSettings(c)
}

func (s *Service) updateCloudContentSettings(c echo.Context) error {
	return s.contentSettingsHTTP().UpdateCloudContentSettings(c)
}

func (s *Service) registerArtifactCatalogRoutes(e *echo.Echo) {
	e.POST(hubapi.NativeBase+"/artifact-services/:service/catalog", s.artifactCatalog)
	e.POST("/api/v2/organizations/:organization/artifact-services/:service/catalog", s.artifactCatalog)
	e.PUT(hubapi.NativeBase+"/artifact-services/:service/catalog-policy", s.setArtifactCatalogPolicy, s.requireOnboardingAdmin())
}

func (s *Service) artifactCatalog(c echo.Context) error {
	return s.artifactCatalogHTTP().ArtifactCatalog(c)
}

func (s *Service) setArtifactCatalogPolicy(c echo.Context) error {
	return s.artifactCatalogHTTP().SetArtifactCatalogPolicy(c)
}

var resolveConversationWorkItems = conversations.ResolveConversationWorkItems

func (s *Service) executeThreadTool(ctx context.Context, call operatortool.Call) (operatortool.Result, error) {
	return threads.NewHTTP(s.threadHTTPDeps()).ExecuteTool(ctx, call, escapeCoordinatorData)
}

func artifactReadGrantRequest(c echo.Context) bool {
	return conversations.ArtifactReadGrantRequest(c)
}

func (s *Service) readConversationTenant(ctx context.Context, org tracker.OrganizationID, read func(tenantstore.Reader) error) error {
	return s.config.TenantRepository.ReadTenant(ctx, org, corestore.Serving, read)
}

func (s *Service) writeConversationTenant(ctx context.Context, org tracker.OrganizationID, write func(tenantstore.Writer) error) error {
	return s.config.TenantRepository.WriteTenant(ctx, org, corestore.Serving, write)
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

func (s *Service) attachmentHTTP() conversations.AttachmentHTTP {
	return conversations.NewAttachmentHTTP(conversations.AttachmentHTTPDeps{
		StorageAvailable:  s.config.TenantRepository != nil,
		APIError:          s.nativeAPIError,
		CheckEvidence:     s.checkAttachmentEvidence,
		Authorize:         s.authorizeAccessScope,
		QuotaLimit:        s.attachmentQuotaLimit,
		ReadTenant:        s.readConversationTenant,
		SharedClaims:      hostedSharedClaims,
		PrepareUpload:     s.prepareCloudAttachmentTenant,
		RecordUpload:      s.recordCloudAttachmentTenant,
		ReadMetadata:      s.readCloudAttachment,
		DeleteMetadata:    s.deleteCloudAttachmentTenant,
		ReferenceMetadata: s.referenceCloudAttachmentCommand,
		Expired:           s.expiredCloudAttachmentsTenant,
		FinishDeletion:    s.finishCloudAttachmentDeletionTenant,
	})
}

func (s *Service) attachmentArchiveHTTP() conversations.AttachmentArchiveHTTP {
	return conversations.NewAttachmentArchiveHTTP(conversations.AttachmentArchiveHTTPDeps{
		Now:                   s.config.Clock,
		APIError:              s.nativeAPIError,
		InvalidRequest:        s.invalidAPIRequest,
		ReadWorkItem:          s.readWorkItemRecord,
		RequireReporterTriage: requireReporterTriage,
		WriteAuthority:        s.attachmentWriteAuthority,
		QuotaLimit:            s.attachmentQuotaLimit,
		ReadConversation:      s.readPGConversation,
		Authorize:             s.authorizeAccessScope,
		ReadTenant:            s.readConversationTenant,
		WriteTenant:           s.writeConversationTenant,
	})
}

func (s *Service) artifactCatalogHTTP() conversations.ArtifactCatalogHTTP {
	return conversations.NewArtifactCatalogHTTP(conversations.ArtifactCatalogHTTPDeps{
		StorageAvailable: s.database.postgres != nil,
		Hosted:           s.config.Hosted != nil,
		Now:              s.config.Clock,
		Organization:     s.accessOrganization,
		APIError:         s.nativeAPIError,
		InvalidRequest:   s.invalidAPIRequest,
		Authenticate:     s.authenticateAPIRequest,
		Authorize:        s.authorizeAccessScope,
		NewCatalog: func(policy cataloghub.PolicyResolver, options cataloghub.Options) (*cataloghub.Catalog, error) {
			return cataloghub.New(s.database.postgres, policy, options)
		},
		Plans: s.database.plansService,
		WriteCatalog: func(ctx context.Context, org tracker.OrganizationID, write func(postgres.ArtifactCatalogWriter) error) error {
			return s.database.postgres.WriteArtifactCatalog(ctx, org, corestore.Serving, write)
		},
	})
}

func (s *Service) contentSettingsHTTP() conversations.ContentSettingsHTTP {
	return conversations.NewContentSettingsHTTP(conversations.ContentSettingsHTTPDeps{
		StorageAvailable: s.config.TenantRepository != nil,
		APIError:         s.nativeAPIError,
		InvalidRequest:   s.invalidAPIRequest,
		ReadProject:      s.readProjectMetadata,
		MutationFeature:  hostedMutationFeature,
		UpdateProject: func(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, req conversations.ContentSettingsRequest) (json.RawMessage, error) {
			return s.executeProjectRecordMutation(ctx, scope, options, req.Mutation, req)
		},
	})
}
