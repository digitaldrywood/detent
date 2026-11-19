package hubserver

import (
	"context"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/sprites"

	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"

	"github.com/digitaldrywood/detent/internal/runnerauth"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubserver/coordinator"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/runner"

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
		Organization:     s.artifactCatalogOrganization,
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

var coordinatorTool = coordinator.Tool
var decodeCoordinatorArguments = coordinator.DecodeArguments
var decodeCoordinatorArgumentsWithLimit = coordinator.DecodeArgumentsWithLimit
var errCoordinatorToolArguments = coordinator.ErrToolArguments
var coordinatorActionRequestID = coordinator.ActionRequestID
var escapeCoordinatorData = coordinator.EscapeData
var coordinatorWorkflowTool = coordinator.WorkflowTool
var coordinatorWorkflowMutation = coordinator.WorkflowMutation
var coordinatorWorkflowTools = coordinator.WorkflowTools
var coordinatorSettingsDiff = coordinator.SettingsDiff
var coordinatorPolicyEvidence = coordinator.PolicyEvidence

func (t *coordinatorToolset) workflowTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return coordinator.PreviewWorkflow(ctx, record, call, t.workflowPreviewDeps())
}

func (s *Service) executeCoordinatorWorkflowAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return coordinator.ExecuteWorkflow(ctx, action, s.workflowExecutionDeps())
}

var coordinatorSlashActionTools = coordinator.SlashActionTools

func (t *coordinatorToolset) slashAction(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return coordinator.PreviewSlashAction(ctx, record, call, t.slashActionDeps())
}

func (t *coordinatorToolset) issueContext(ctx context.Context, record conversationRecord) (operatortool.IssueContext, error) {
	return t.issueContextDeps().IssueContext(ctx, record)
}

func (t *coordinatorToolset) readIssueHistory(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return t.issueContextDeps().ReadIssueHistory(ctx, record, call)
}

var coordinatorSubjectPrompt = coordinator.SubjectPrompt

func (t *coordinatorToolset) proposeStopRun(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return coordinator.PreviewStopRun(ctx, record, call, t.stopRunPreviewDeps())
}

func (s *Service) executeCoordinatorStopRun(ctx context.Context, scope hubscope.NativeScope, action chat.Action) (chat.ActionExecution, error) {
	return coordinator.ExecuteStopRun(ctx, scope, action, s.stopNativeRunCommand)
}

func (s *Service) stopNativeRunCommand(ctx context.Context, scope hubscope.NativeScope, requestID string, request coordinatorStopRunArguments) (json.RawMessage, error) {
	return coordinator.StopRun(ctx, scope, requestID, request, s.transitionNativeIssueTenantWithStop)
}

var coordinatorActionError = coordinator.ActionError

type coordinatorCatalogEntry = coordinator.CatalogEntry

func (s *Service) coordinatorCatalog(ctx context.Context) ([]operatortool.Definition, error) {
	return coordinator.Catalog(ctx, (hostedOperatorExecutor{service: s}).ListTools, coordinatorCatalogExcluded)
}

func (t *coordinatorToolset) findTools(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return t.catalogToolDeps().FindTools(ctx, record, call)
}

func (t *coordinatorToolset) callTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return t.catalogToolDeps().CallTool(ctx, record, call)
}

func (s *Service) executeCoordinatorCatalogCall(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return coordinator.ExecuteCatalogCall(ctx, action, s.catalogExecutionDeps())
}

var errCoordinatorProjectUnreadable = coordinator.ErrProjectUnreadable

func (t *coordinatorToolset) proposalDeps() coordinator.ProposalDeps {
	return coordinator.ProposalDeps{
		ActionContext: t.actionContext,
		MessageID: func() string {
			return t.state.users[len(t.state.users)-1].ID
		},
		Submit: t.submitCoordinatorAction,
	}
}

func (t *coordinatorToolset) workflowPreviewDeps() coordinator.WorkflowPreviewDeps {
	executor := hubProjectExecutor{t.coordinator.server}
	return coordinator.WorkflowPreviewDeps{
		ProposalDeps:       t.proposalDeps(),
		ProjectScope:       projectToolScope,
		LocalConfiguration: executor.localProjectConfiguration,
		ReadConfiguration:  executor.Execute,
		ReadPolicy:         executor.read,
		PreviewApproval: func(ctx context.Context, call operatortool.Call) (json.RawMessage, error) {
			return executor.command(ctx, call, projectCommandPreview)
		},
		PreviewConfiguration: func(ctx context.Context, name string, args operatortool.LocalProjectArguments) (operatortool.Result, error) {
			return executor.projectConfigurationCommand(ctx, name, args, true)
		},
	}
}

func (s *Service) workflowExecutionDeps() coordinator.WorkflowExecutionDeps {
	executor := hubProjectExecutor{s}
	return coordinator.WorkflowExecutionDeps{
		LocalConfiguration: executor.localProjectConfiguration,
		Approve: func(ctx context.Context, call operatortool.Call) (json.RawMessage, error) {
			return executor.command(ctx, call, projectCommandExecute)
		},
		Audit: executor.AuditAction,
	}
}

func (s *Service) resolveCoordinatorIssue(ctx context.Context, scope hubscope.NativeScope, reference string) (tracker.NativeIssue, error) {
	return s.resolveOperatorNativeItem(ctx, nil, scope, reference)
}

func (t *coordinatorToolset) slashActionDeps() coordinator.SlashActionDeps {
	s := t.coordinator.server
	return coordinator.SlashActionDeps{
		ProposalDeps:  t.proposalDeps(),
		ResolveIssue:  s.resolveCoordinatorIssue,
		ReadProject:   s.readProjectRecord,
		ProjectAction: t.projectAction,
	}
}

func (t *coordinatorToolset) issueContextDeps() coordinator.IssueContextDeps {
	return coordinator.IssueContextDeps{
		ActionContext:    t.actionContext,
		ResolveReference: t.resolveIssueReference,
		WorkReader: func(scope hubscope.NativeScope) operatortool.WorkReader {
			return operatorWorkReads{service: t.coordinator.server, scope: scope}
		},
	}
}

func (t *coordinatorToolset) stopRunPreviewDeps() coordinator.StopRunPreviewDeps {
	s := t.coordinator.server
	return coordinator.StopRunPreviewDeps{
		ActionContext: t.actionContext,
		Submit:        t.submitCoordinatorAction,
		ResolveIssue:  s.resolveCoordinatorIssue,
		ReadProject:   s.readProjectRecord,
		ActiveLease:   s.readCoordinatorStopRunLease,
		Now:           s.config.Clock,
	}
}

func (s *Service) readCoordinatorStopRunLease(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID) (lease leasestore.Lease, err error) {
	err = s.readConversationTenant(ctx, scope.Organization, func(r tenantstore.Reader) error {
		lease, err = coordinator.ReadStopRunLease(ctx, r, scope, item, s.authorizeAccessScope)
		return err
	})
	return lease, err
}

func (t *coordinatorToolset) catalogToolDeps() coordinator.CatalogToolDeps {
	s := t.coordinator.server
	return coordinator.CatalogToolDeps{
		ProposalDeps: t.proposalDeps(),
		Operator:     currentHubOperator,
		WithOperator: s.withOperatorCatalog,
		Catalog:      s.coordinatorCatalog,
		Execute:      (hostedOperatorExecutor{service: s}).Execute,
		Excluded:     coordinatorCatalogExcluded,
	}
}

func (s *Service) catalogExecutionDeps() coordinator.CatalogExecutionDeps {
	return coordinator.CatalogExecutionDeps{
		BindConnection: s.bindCoordinatorConnection,
		Catalog:        s.coordinatorCatalog,
		Execute:        (hostedOperatorExecutor{service: s}).Execute,
	}
}

func (t *coordinatorToolset) workReadContext(ctx context.Context, record conversationRecord) (context.Context, hubscope.NativeScope, error) {
	return t.issueContextDeps().WorkReadContext(ctx, record)
}
func (s *Service) coordinatorRunner(ctx context.Context, scope hubscope.NativeScope, id string) (runnerauth.Runner, error) {
	return s.coordinatorRunnerDeps().Runner(ctx, scope, id)
}

func (s *Service) coordinatorRunners(ctx context.Context, scope hubscope.NativeScope, args coordinatorRunnerArguments) ([]runnerauth.Runner, error) {
	return s.coordinatorRunnerDeps().Runners(ctx, scope, args)
}

func (t *coordinatorToolset) runnerTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return coordinator.PreviewRunner(ctx, record, call, t.runnerPreviewDeps())
}

func (s *Service) executeCoordinatorRunnerAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return s.coordinatorRunnerDeps().Execute(ctx, action)
}

var coordinatorRunnerTools = coordinator.RunnerTools

type coordinatorRunnerArguments = coordinator.RunnerArguments

func coordinatorSettingsRequirement(name, project string) operatortool.Requirement {
	return coordinator.SettingsRequirement(name, project, hubFleetRequirement, projectToolScope)
}

func (s *Service) coordinatorSettingsTarget(ctx context.Context, record conversationRecord, name string, raw json.RawMessage) error {
	return s.coordinatorSettingsDeps().CheckTarget(ctx, record, name, raw)
}

func (t *coordinatorToolset) settingsTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return coordinator.PreviewSettings(ctx, record, call, t.settingsPreviewDeps())
}

func (s *Service) executeCoordinatorSettingsAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return s.coordinatorSettingsDeps().Execute(ctx, action)
}

var coordinatorSettingsNames = coordinator.SettingsNames
var coordinatorSettingsTool = coordinator.SettingsTool
var coordinatorSettingsTools = coordinator.SettingsTools

func (s *Service) coordinatorRunnerDeps() coordinator.RunnerDeps {
	repository, available := s.config.TenantRepository.(tenantstore.SnapshotRepository)
	return coordinator.RunnerDeps{
		SnapshotAvailable: available,
		ReadSnapshot: func(ctx context.Context, org tracker.OrganizationID, read func(tenantstore.Reader) error) error {
			return repository.ReadTenantSnapshot(ctx, org, corestore.Serving, read)
		},
		Authorize: s.authorizeAccessScope,
		View:      runnerFromRoutingRecord,
		Scope:     s.coordinatorScope,
		Now:       s.config.Clock,
		UpdateRouting: func(ctx context.Context, scope hubscope.NativeScope, id string, change runnerauth.RoutingChange, tier *string) error {
			_, err := s.updateRunnerRoutingCommand(ctx, scope, id, runnerRoutingRequest{RoutingChange: change, IsolationTier: tier})
			return err
		},
	}
}

func (t *coordinatorToolset) runnerPreviewDeps() coordinator.RunnerPreviewDeps {
	return coordinator.RunnerPreviewDeps{
		ProposalDeps:     t.proposalDeps(),
		Runners:          t.coordinator.server.coordinatorRunnerDeps(),
		ReadableProjects: t.readableProjects,
	}
}

func (s *Service) coordinatorSettingsDeps() coordinator.SettingsDeps {
	return coordinator.SettingsDeps{
		Operator: currentHubOperator,
		Runner:   s.coordinatorRunner,
		DecodeRouting: func(raw json.RawMessage) (runnerauth.RoutingChange, error) {
			var request runnerRoutingRequest
			err := operatortool.DecodeArguments(raw, &request)
			return request.RoutingChange, err
		},
		DecodeCapacity: func(raw json.RawMessage) (string, string, error) {
			var args hubFleetRequest
			if err := operatortool.DecodeArguments(raw, &args); err != nil {
				return "", "", err
			}
			var change runnerCapacityChange
			err := operatortool.DecodeArguments(args.Change, &change)
			return args.RunnerID, change.Backend, err
		},
		FleetTool:          hubFleetTool,
		FleetPermitted:     s.hubFleetReadPermitted,
		ExecuteProject:     (hubProjectExecutor{s}).ExecuteAction,
		ExecuteFleet:       (hubFleetExecutor{s}).ExecuteAction,
		ReadCapacity:       s.readRunnerCapacity,
		ReadProjectRunners: s.coordinatorProjectRunners,
	}
}

func (t *coordinatorToolset) settingsPreviewDeps() coordinator.SettingsPreviewDeps {
	s := t.coordinator.server
	return coordinator.SettingsPreviewDeps{
		ProposalDeps: t.proposalDeps(),
		Settings:     s.coordinatorSettingsDeps(),
		Requirement:  coordinatorSettingsRequirement,
		PreviewFleet: (hubFleetExecutor{s}).proposal,
		PreviewProject: func(ctx context.Context, call operatortool.Call) (json.RawMessage, error) {
			return (hubProjectExecutor{s}).command(ctx, call, projectCommandPreview)
		},
		ProjectError: projectToolError,
	}
}
func (s *Service) attachmentQuotaLimit(ctx context.Context, r tenantstore.Reader) (int64, error) {
	return s.attachmentRecords().AttachmentQuotaLimit(ctx, r)
}

func (s *Service) attachmentWriteAuthority(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, evidence *attachment.EvidenceRequest) (corestore.WorkItem, error) {
	return s.attachmentRecords().AttachmentWriteAuthority(ctx, w, scope, evidence)
}

func (s *Service) prepareCloudAttachmentTenant(c echo.Context, input attachment.UploadRequest) error {
	return s.attachmentRecords().PrepareCloudAttachmentTenant(c, input)
}

func (s *Service) prepareEvidenceComment(ctx context.Context, scope hubscope.NativeScope, item tracker.NativeWorkItemID, body string, offset int64) (hubrecords.AttachmentEvidencePublication, error) {
	return s.attachmentRecords().PrepareEvidenceComment(ctx, scope, item, body, offset)
}

func (s *Service) prepareEvidenceCommentRecord(ctx context.Context, scope hubscope.NativeScope, row corestore.WorkItem, version eventstore.CollaborationVersion, body string, offset int64) (hubrecords.AttachmentEvidencePublication, error) {
	return s.attachmentRecords().PrepareEvidenceCommentRecord(ctx, scope, row, version, body, offset)
}

func (s *Service) recordCloudAttachmentTenant(c echo.Context, input attachment.UploadRequest) error {
	return s.attachmentRecords().RecordCloudAttachmentTenant(c, input)
}

func (s *Service) readAttachmentRecordTenant(ctx context.Context, scope hubscope.NativeScope, id string) (attachment.Metadata, error) {
	return s.attachmentRecords().ReadAttachmentRecordTenant(ctx, scope, id)
}

func (s *Service) readAttachmentRecordWith(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, id string, metadata *attachment.Metadata) error {
	return s.attachmentRecords().ReadAttachmentRecordWith(ctx, r, scope, id, metadata)
}

func (s *Service) prepareWorkItemAttachmentReferences(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan) (hubrecords.WorkItemAttachmentReferences, error) {
	return s.attachmentRecords().PrepareWorkItemAttachmentReferences(ctx, scope, plan)
}

func (s *Service) authorizeWorkerItemFence(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, row corestore.WorkItem, command tracker.Mutation) error {
	return s.attachmentRecords().AuthorizeWorkerItemFence(ctx, w, scope, row, command)
}

func (s *Service) authorizeAttachmentCommentLease(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, row corestore.WorkItem, command tracker.Mutation) error {
	return s.attachmentRecords().AuthorizeAttachmentCommentLease(ctx, w, scope, row, command)
}

func (s *Service) referenceCloudAttachmentTenant(ctx context.Context, scope hubscope.NativeScope, id string, input attachment.SourceReference, fence tracker.Mutation) error {
	return s.attachmentRecords().ReferenceCloudAttachmentTenant(ctx, scope, id, input, fence)
}

func (s *Service) deleteCloudAttachmentTenant(ctx context.Context, scope hubscope.NativeScope, id string) error {
	return s.attachmentRecords().DeleteCloudAttachmentTenant(ctx, scope, id)
}

func (s *Service) finishCloudAttachmentDeletionTenant(c echo.Context) error {
	return s.attachmentRecords().FinishCloudAttachmentDeletionTenant(c)
}

func (s *Service) expiredCloudAttachmentsTenant(c echo.Context) error {
	return s.attachmentRecords().ExpiredCloudAttachmentsTenant(c)
}

func (s *Service) artifactReadGrantTenant(ctx context.Context, scope hubscope.NativeScope, item, artifactID string, revision int64, now time.Time) (result artifact.Grant, err error) {
	return s.artifactRecords().ArtifactReadGrantTenant(ctx, scope, item, artifactID, revision, now)
}

func (s *Service) readArtifactGrantTenant(ctx context.Context, scope hubscope.NativeScope, service string, request artifact.ReadAuthorization) (grant artifactstore.ReadGrant, owner tracker.Actor, err error) {
	return s.artifactRecords().ReadArtifactGrantTenant(ctx, scope, service, request)
}

func (s *Service) authorizeArtifactUploadTenant(ctx context.Context, scope hubscope.NativeScope, reservation artifact.Reservation) error {
	return s.artifactRecords().AuthorizeArtifactUploadTenant(ctx, scope, reservation)
}

func (s *Service) publishArtifactReceiptTenant(ctx context.Context, scope hubscope.NativeScope, reference artifact.Reference) error {
	return s.artifactRecords().PublishArtifactReceiptTenant(ctx, scope, reference)
}

func (s *Service) prepareConversationPreferenceIssue(ctx context.Context, scope hubscope.NativeScope, record conversationRecord, preferences conversation.Preferences) (conversationPreferenceIssue, error) {
	return s.preferenceIssueDeps().PrepareConversationPreferenceIssue(ctx, scope, record, preferences)
}

type conversationPreferenceIssue = conversations.PreparedPreferenceIssue

var contentWriting = conversations.SetArchiveWriting

func (s *Service) attachmentRecords() conversations.AttachmentRecords {
	return conversations.NewAttachmentRecords(conversations.AttachmentRecordDeps{
		Now:                   s.config.Clock,
		ReadTenant:            s.readConversationTenantWorkload,
		WriteTenant:           s.writeConversationTenantWorkload,
		Authorize:             s.authorizeAccessScope,
		CheckEvidence:         s.checkAttachmentEvidence,
		APIError:              s.nativeAPIError,
		SharedClaims:          hostedSharedClaims,
		Plans:                 s.database.plansService,
		Charge:                s.database.chargeAttachmentAPIMutationsPostgres,
		LeaseMatches:          runnerIdentityMatchesHeldLease,
		RequireReporterTriage: requireReporterTriage,
		EscapeEvidenceCaption: escapeEvidenceCaption,
		ReadWorkItem:          s.readWorkItemRecord,
		Hydrate:               s.hydrateWorkItemRecord,
		HydrateCanonical:      s.hydrateCanonicalWorkItemRecord,
		PrepareFields:         s.prepareWorkItemFields,
		Receipt:               s.workItemRecordReceipt,
		Details:               s.runDetailStore,
	})
}

func (s *Service) artifactRecords() conversations.ArtifactRecords {
	return conversations.NewArtifactRecords(conversations.ArtifactRecordDeps{
		StorageAvailable:       s.config.TenantRepository != nil,
		Now:                    s.config.Clock,
		ReadTenant:             s.readConversationTenantWorkload,
		WriteTenant:            s.writeConversationTenantWorkload,
		WebSession:             s.WebSession,
		HostedMutationIdentity: s.hostedMutationIdentity,
		RefuseStaffSession: func(session auth.Session) bool {
			return s.config.Hosted.SharedEntry == nil && hostedplans.EmailListed(s.config.Hosted.StaffEmails, session.Email) && (session.Identity == nil || session.Identity.SupportActor == "")
		},
		Authorize:      s.authorizeAccessScope,
		LeasePolicy:    s.dependencyReasonPolicy,
		AuthorizeLease: s.authorizeDependencyReasonLease,
		Charge:         s.applyRunnerTenantUsage,
	})
}

func (s *Service) preferenceIssueDeps() conversations.PreferenceIssueDeps {
	return conversations.PreferenceIssueDeps{
		LoadPlan: s.loadWorkItemPlan,
		Authorize: func(ctx context.Context, scope hubscope.NativeScope) error {
			return s.readConversationTenant(ctx, scope.Organization, func(reader tenantstore.Reader) error {
				return s.authorizeWorkItemRecordScope(ctx, reader, scope)
			})
		},
		Now:           s.config.Clock,
		PrepareFields: s.prepareWorkItemFields,
		Details:       s.runDetailStore,
	}
}

func (s *Service) readConversationTenantWorkload(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return s.config.TenantRepository.ReadTenant(ctx, org, workload, read)
}

func (s *Service) writeConversationTenantWorkload(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return s.config.TenantRepository.WriteTenant(ctx, org, workload, write)
}
func (t *coordinatorToolset) proposeIssueArchive(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return t.issueProposalDeps().PreviewArchive(ctx, record, call)
}

func (s *Service) executeCoordinatorIssueArchive(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return s.issueCommandDeps().ExecuteArchive(ctx, action)
}

func (s *Service) archiveCoordinatorWorkItems(ctx context.Context, scope hubscope.NativeScope, command tracker.Mutation, archive chat.IssueArchive) (json.RawMessage, error) {
	return s.issueCommandDeps().Archive(ctx, scope, command, archive)
}

var validateCoordinatorArchiveIDs = coordinator.ValidateArchiveIDs

const coordinatorArchiveMaxItems = coordinator.ArchiveMaxItems

func (t *coordinatorToolset) loadSplitIssueSkill(raw json.RawMessage) (any, error) {
	return coordinator.LoadSplitIssueSkill(raw)
}

func (t *coordinatorToolset) proposeIssueSplit(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return t.issueProposalDeps().PreviewSplit(ctx, record, call)
}

func (s *Service) executeCoordinatorIssueSplit(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return s.issueCommandDeps().ExecuteSplit(ctx, action)
}

func (s *Service) executeCoordinatorSplitRecords(ctx context.Context, scope hubscope.NativeScope, options hubapi.NativeCommandOptions, command tracker.Mutation, split chat.IssueSplit) (json.RawMessage, error) {
	return s.issueCommandDeps().Split(ctx, scope, options, command, split)
}

var coordinatorIssueSplitTool = coordinator.IssueSplitTool
var validateCoordinatorIssueSplit = coordinator.ValidateIssueSplit

const coordinatorSplitMaxChildren = coordinator.SplitMaxChildren

func (t *coordinatorToolset) issueProposalDeps() coordinator.IssueProposalDeps {
	return coordinator.IssueProposalDeps{ProposalDeps: t.proposalDeps(), Commands: t.coordinator.server.issueCommandDeps()}
}

func (s *Service) issueCommandDeps() coordinator.IssueCommandDeps {
	return coordinator.IssueCommandDeps{
		StorageAvailable: s.config.TenantRepository != nil && s.config.AccessRepository != nil,
		ReadTenant:       s.readConversationTenantWorkload,
		WriteTenant:      s.writeConversationTenantWorkload,
		Now:              s.config.Clock,
		Operation:        nativeOperation,
		HydrateIssue:     s.hydrateWorkItemRecord,
		ReadWorkItem:     s.readWorkItemRecord,
		ReadProject:      s.readProjectRecord,
		BeginAuthority:   s.beginProjectRecordAuthority,
		RecheckMutation:  s.recheckProjectRecordMutation,
		Authorize:        s.authorizeWorkItemRecordScope,
		LoadArchivePlan: func(ctx context.Context, scope hubscope.NativeScope, item string, revision tracker.Revision) (hubrecords.WorkItemRecordPlan, error) {
			input := workItemArchive{archiveIssueRequest: archiveIssueRequest{ExpectedRevision: revision}, Archived: true}
			return s.loadWorkItemPlan(ctx, scope, item, "", input)
		},
		PlanArchive: func(ctx context.Context, scope hubscope.NativeScope, plan hubrecords.WorkItemRecordPlan, revision tracker.Revision) (hubrecords.WorkItemRecordPlan, error) {
			input := workItemArchive{archiveIssueRequest: archiveIssueRequest{ExpectedRevision: revision}, Archived: true}
			return s.planWorkItemRecord(ctx, scope, plan, input)
		},
		LoadPlan:           s.loadWorkItemPlan,
		PlanWorkItem:       s.planWorkItemRecord,
		PrepareFields:      s.prepareWorkItemFields,
		IssueResponse:      s.nativeIssueResponse,
		Receipt:            s.workItemRecordReceipt,
		RecordEntryAudit:   s.recordEntryAccessKeyAuditWith,
		SignalDispatch:     s.signalNativeDispatch,
		Details:            s.runDetailStore,
		Plans:              s.database.plansService,
		ValidateCreate:     validateWorkItemRecordCreateInput,
		ValidateFiling:     s.validateWorkItemRecordFilingTenant,
		PrepareAttachments: s.prepareWorkItemAttachmentReferences,
		AttachmentQuota:    s.attachmentQuotaLimit,
		WakeSpriteRunners:  s.wakeSpriteRunnersAfter,
	}
}

var coordinatorToolError = coordinator.ToolError

func (c *conversationTurnCoordinator) modelChoiceGranted(ctx context.Context, r tenantstore.Reader, organization tracker.OrganizationID, now time.Time) (bool, error) {
	return c.server.conversationModelChoiceGranted(ctx, r, organization, now)
}

func (c *conversationTurnCoordinator) modelChoices(ctx context.Context, r tenantstore.Reader, project tracker.ProjectID, now time.Time) ([]conversationModel, error) {
	return c.server.conversationModelChoices(ctx, r, []tracker.ProjectID{project}, now)
}
func (c *conversationTurnCoordinator) appendMessage(ctx context.Context, w tenantstore.Writer, record *conversationRecord, message *conversationMessageRecord, now time.Time) error {
	return coordinator.AppendMessage(ctx, w, record, message, now)
}

func (c *conversationTurnCoordinator) updateMessage(ctx context.Context, w tenantstore.Writer, message conversationMessageRecord, now time.Time) error {
	return coordinator.UpdateMessage(ctx, w, message, now)
}

func (c *conversationTurnCoordinator) saveConversation(ctx context.Context, w tenantstore.Writer, record *conversationRecord, now time.Time) error {
	return coordinator.SaveConversation(ctx, w, record, now)
}

func (c *conversationTurnCoordinator) updateExecution(ctx context.Context, w tenantstore.Writer, record *conversationRecord, execution conversation.Execution, now time.Time) error {
	return coordinator.UpdateExecution(ctx, w, record, execution, now)
}

func (c *conversationTurnCoordinator) appendDelta(ctx context.Context, w tenantstore.Writer, message *conversationMessageRecord, text string, now time.Time) error {
	return coordinator.AppendDelta(ctx, w, message, text, now)
}

func (c *conversationTurnCoordinator) setControlDelivery(ctx context.Context, w tenantstore.Writer, message *conversationMessageRecord, delivery conversation.Delivery, failure *conversation.ReceiptError, now time.Time) error {
	return coordinator.SetControlDelivery(ctx, w, message, delivery, failure, now)
}

func (c *conversationTurnCoordinator) coordinatorMessages(ctx context.Context, filter conversationstore.MessageFilter) ([]conversationMessageRecord, error) {
	return c.recordDeps().ReadMessages(ctx, filter)
}

func (c *conversationTurnCoordinator) authorizeRecord(ctx context.Context, scope hubscope.NativeScope, record conversationRecord, write bool) error {
	return c.recordDeps().AuthorizeRecord(ctx, scope, record, write)
}

func (c *conversationTurnCoordinator) attachmentContent(ctx context.Context, id string) ([]byte, error) {
	return c.recordDeps().AttachmentContent(ctx, id)
}

func (c *conversationTurnCoordinator) DeliverCoordinatorCommands(ctx context.Context, id string) error {
	return c.recordDeps().DeliverCommands(ctx, id)
}

var appendCoordinatorEvent = coordinator.AppendEvent
var coordinatorMonthlyBudget = coordinator.MonthlyBudget

func (c *conversationTurnCoordinator) recordDeps() coordinator.RecordDeps {
	return coordinator.RecordDeps{
		Service:     c.service,
		ReadTenant:  c.server.readConversationTenant,
		WriteTenant: c.server.writeConversationTenant,
		ReadObject: func(ctx context.Context, org string, ref attachment.ArchiveReference, target *conversations.ConversationContent) error {
			return c.server.readConversationObject(ctx, org, ref, target)
		},
		Authorize: c.server.authorizeAccessScope,
		Now:       func() time.Time { return c.server.config.Clock() },
		Cancel:    c.cancelRecordedTurn,
	}
}
func (t *coordinatorToolset) tools() []runner.AgentTool {
	return coordinator.Tools(append(append(append(append(append(append(append(coordinatorActionTools(), coordinatorWorkflowTools()...), coordinatorSlashActionTools()...), coordinatorSpriteTools()...), coordinatorRunnerTools()...), coordinatorScreenTools()...), coordinatorSettingsTools()...), coordinatorCatalogTools()...))
}

func (t *coordinatorToolset) handle(ctx context.Context, call runner.AgentToolCall) (runner.AgentToolResult, error) {
	return t.toolDeps().Handle(ctx, call)
}

func (t *coordinatorToolset) execute(ctx context.Context, call runner.AgentToolCall) (any, error) {
	return t.toolDeps().Execute(ctx, call)
}

func (t *coordinatorToolset) readableProjects(ctx context.Context, record conversationRecord) ([]tracker.ProjectID, error) {
	return t.workToolDeps().ReadableProjects(ctx, record)
}

func (t *coordinatorToolset) listAttention(ctx context.Context, record conversationRecord, readable []tracker.ProjectID, scope string, limit int) (any, error) {
	return t.workToolDeps().ListAttention(ctx, record, readable, scope, limit)
}

func (t *coordinatorToolset) resolveIssueReference(ctx context.Context, record conversationRecord, reference string) (string, error) {
	return t.workToolDeps().ResolveIssueReference(ctx, record, reference)
}

func (t *coordinatorToolset) explainIssue(ctx context.Context, record conversationRecord, readable []tracker.ProjectID, workItemID string) (any, error) {
	return t.workToolDeps().ExplainIssue(ctx, record, readable, workItemID)
}

func (s *Service) coordinatorAllowedProjects(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, ids []tracker.ProjectID) ([]tracker.ProjectID, error) {
	return coordinator.AllowedProjects(ctx, r, scope, ids, s.authorizeAccessScope)
}

func (t *coordinatorToolset) proposeIssue(ctx context.Context, record conversationRecord, readable []tracker.ProjectID, title, objective, projectID string) (any, error) {
	return coordinator.ProposeIssue(ctx, record, readable, title, objective, projectID, t.publishIssueProposal)
}

const coordinatorToolResultBytes = coordinator.ToolResultBytes

type coordinatorIssue = coordinator.IssueExplanation

func (t *coordinatorToolset) toolDeps() coordinator.ToolDeps {
	deps := coordinator.ToolDeps{
		ConversationID:  t.state.conversationID,
		CatalogCallName: coordinatorToolCallTool,
		CatalogFindName: coordinatorToolFindTools,
		Logger:          t.coordinator.logger,
		EndSegment:      t.state.endTextSegment,
		ReadConversation: func(ctx context.Context) (conversationRecord, error) {
			return t.coordinator.readConversation(ctx, nil, t.state.conversationID)
		},
		ActionContext:           t.actionContext,
		Operator:                currentHubOperator,
		AuthorizeRecord:         t.coordinator.authorizeRecord,
		ReadableProjects:        t.readableProjects,
		PostActionRefusal:       t.postActionRefusal,
		RestrictReadAudience:    t.restrictReadAudience,
		ScreenTool:              coordinatorScreenRead,
		IsSpriteTool:            coordinatorSpriteTool,
		SpriteMutation:          coordinatorSpriteMutation,
		ScreenRead:              t.screenRead,
		SettingsTool:            t.settingsTool,
		WorkflowTool:            t.workflowTool,
		RunnerTool:              t.runnerTool,
		PromoteSpritesTokenTool: t.promoteSpritesTokenTool,
		SpriteTool:              t.spriteTool,
		ReadIssueHistory:        t.readIssueHistory,
		ProjectAction:           t.projectAction,
		ProposeStopRun:          t.proposeStopRun,
		SlashAction:             t.slashAction,
		FindTools:               t.findTools,
		CallTool:                t.callTool,
		ProposeIssueArchive:     t.proposeIssueArchive,
		ProposeIssueSplit:       t.proposeIssueSplit,
		ListAttention:           t.listAttention,
		ExplainIssue:            t.explainIssue,
		ProposeIssue:            t.proposeIssue,
	}
	if t.coordinator.server != nil {
		deps.RunDetailContext = t.coordinator.server.runDetailContext
	}
	return deps
}

func (t *coordinatorToolset) workToolDeps() coordinator.WorkToolDeps {
	s := t.coordinator.server
	return coordinator.WorkToolDeps{
		ActionContext:   t.actionContext,
		WorkReadContext: t.workReadContext,
		Operator:        currentHubOperator,
		ReadTenant:      s.readConversationTenant,
		Authorize:       s.authorizeAccessScope,
		AllowedProjects: s.coordinatorAllowedProjects,
		Now:             s.config.Clock,
		ResolveIssue:    s.resolveCoordinatorIssue,
		ReadWorkItem:    s.readWorkItemRecord,
		HydrateIssue:    s.hydrateWorkItemRecord,
		ReadAttempt:     s.readNativeAttemptTenant,
		ReadComments:    s.readCommentRecords,
	}
}

func (t *coordinatorToolset) publishIssueProposal(ctx context.Context, id string, message conversationMessageRecord) error {
	t.state.mu.Lock()
	defer t.state.mu.Unlock()
	message.ThreadID = t.state.threadID
	return t.coordinator.appendPreparedMessage(ctx, id, message)
}
func (c *conversationTurnCoordinator) write(ctx context.Context, conversationID string, fn func(ctx context.Context, tx tenantstore.Writer, record *conversationRecord, now time.Time) error, preparations ...func(context.Context, *conversationPublicationPlan) error) error {
	return c.turnWriteDeps().Write(ctx, conversationID, fn, preparations...)
}

var coordinatorHandles = coordinator.Handles
var errCoordinatorRevision = coordinator.ErrRevision
var errCoordinatorLinked = coordinator.ErrLinked

func (c *conversationTurnCoordinator) callBackend(ctx context.Context, backend runner.AgentBackend, request runner.AgentTurnRequest, state *coordinatorTurnState) (runner.AgentTurnResult, error) {
	return coordinator.CallBackend(ctx, backend, request, c.stop, state.handleUpdate, func() ([]runner.AgentTool, runner.AgentToolHandler) {
		toolset := newCoordinatorToolset(c, state)
		return toolset.tools(), toolset.handle
	})
}

var coordinatorOutcome = coordinator.Outcome
var coordinatorToolSummary = coordinator.ToolSummary

const coordinatorInstructions = coordinator.Instructions

type coordinatorAttachment = coordinator.TextAttachment

var coordinatorSkillsPrompt = coordinator.SkillsPrompt
var appendCoordinatorData = coordinator.AppendData
var coordinatorAttachmentBlock = coordinator.AttachmentBlock
var coordinatorPrompt = coordinator.Prompt

func (c *conversationTurnCoordinator) coordinatorAttachments(ctx context.Context, pending []conversationMessageRecord) ([]coordinatorAttachment, error) {
	return coordinator.ReadTextAttachments(ctx, pending, c.attachmentContent)
}

func (c *conversationTurnCoordinator) turnWriteDeps() coordinator.TurnWriteDeps {
	return coordinator.TurnWriteDeps{
		Service:     c.service,
		Now:         func() time.Time { return c.server.config.Clock() },
		WriteTenant: c.server.writeConversationTenant,
	}
}

var coordinatorScreenReadNames = coordinator.ScreenReadNames

var coordinatorScreenTools = coordinator.ScreenTools

var coordinatorScreenRead = coordinator.ScreenRead

var coordinatorReadPermission = coordinator.ReadPermission

func (t *coordinatorToolset) screenRead(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return t.screenReadDeps().Read(ctx, record, call)
}

var coordinatorProjectReadScoped = coordinator.ProjectReadScoped

func (s *Service) coordinatorRunnerAttached(ctx context.Context, record conversationRecord, id string) (bool, error) {
	return coordinator.RunnerAttached(ctx, record, id, currentHubOperator, s.coordinatorRunner)
}

func (s *Service) coordinatorScreenReport(ctx context.Context, record conversationRecord, credential hubapi.APICredential, call runner.AgentToolCall) (any, error) {
	return s.screenReportDeps().Report(ctx, record, credential, call)
}

var coordinatorScreenTarget = coordinator.ScreenTarget

func (t *coordinatorToolset) restrictReadAudience(ctx context.Context, record conversationRecord, credential hubapi.APICredential) error {
	return t.readAudienceDeps().Restrict(ctx, record, credential)
}

func (t *coordinatorToolset) spriteTool(ctx context.Context, record conversationRecord, call runner.AgentToolCall) (any, error) {
	return t.spritePreviewDeps().Preview(ctx, record, call)
}

func (s *Service) coordinatorScope(ctx context.Context, project tracker.ProjectID, manage bool) (hubscope.NativeScope, error) {
	return coordinator.NativeScope(ctx, project, manage)
}

func (s *Service) validateCoordinatorSpriteChange(ctx context.Context, scope hubscope.NativeScope, settings spritePoolSettings) error {
	return s.spriteCommandDeps().Validate(ctx, scope, settings)
}

func (s *Service) executeCoordinatorSpriteAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return s.spriteCommandDeps().Execute(ctx, action)
}

var coordinatorSpriteTools = coordinator.SpriteTools

var coordinatorSpriteTool = coordinator.IsSpriteTool

var coordinatorSpriteMutation = coordinator.SpriteMutation

var decodeCoordinatorSpriteArguments = coordinator.DecodeSpriteArguments

type coordinatorSpriteChange = coordinator.SpriteChange

func (s *Service) coordinatorProposalAgent(ctx context.Context, action chat.Action) (string, error) {
	return s.readCoordinatorProposalAgent(ctx, action)
}

const coordinatorAgentName = coordinator.AgentName

var coordinatorAgent = coordinator.Agent

type coordinatorSpriteArguments = coordinator.SpriteArguments

func (t *coordinatorToolset) screenReadDeps() coordinator.ScreenReadDeps {
	s := t.coordinator.server
	return coordinator.ScreenReadDeps{
		ActionContext:    t.actionContext,
		Operator:         currentHubOperator,
		AuthorizeRecord:  t.coordinator.authorizeRecord,
		RestrictAudience: t.restrictReadAudience,
		ReadSlack: func(ctx context.Context, org tracker.OrganizationID) (any, error) {
			return s.coordinatorSlackIntegration(ctx, org)
		},
		ReadBudget: func(ctx context.Context, scope hubscope.NativeScope) (any, error) {
			return readBillingMonthlySettings(ctx, s.config.TenantRepository, scope)
		},
		ReadRunners:      s.coordinatorProjectRunners,
		AllRunnerGrants:  s.hostedAllRunnerGrants,
		ReadReport:       s.coordinatorScreenReport,
		FleetRequirement: hubFleetRequirement,
		RunnerAttached:   s.coordinatorRunnerAttached,
		ExecuteOperator:  (hostedOperatorExecutor{service: s}).Execute,
		ProjectMembers:   coordinatorProjectMembers,
	}
}

func (s *Service) screenReportDeps() coordinator.ScreenReportDeps {
	return coordinator.ScreenReportDeps{
		Now:            s.config.Clock,
		FleetPermitted: s.hubFleetReadPermitted,
		UsageWindow: func(name string, now time.Time) (operatortool.AnalyticsWindow, error) {
			window, err := usageRangeWindow(name, now)
			return operatortool.AnalyticsWindow{From: window.From, To: window.To, Bucket: time.Hour}, err
		},
		ReadProjectReport: func(ctx context.Context, credential hubapi.APICredential, project string, window operatortool.AnalyticsWindow) (any, bool, error) {
			return s.readProjectReport(ctx, credential, project, window, reportsFilter{})
		},
		ReadDiagnostics: func(ctx context.Context, credential hubapi.APICredential, window operatortool.AnalyticsWindow, project tracker.ProjectID) (any, error) {
			return s.readDiagnosticsTenant(ctx, credential, window, project)
		},
	}
}

func (t *coordinatorToolset) readAudienceDeps() coordinator.ReadAudienceDeps {
	return coordinator.ReadAudienceDeps{
		ReadTenant: t.coordinator.server.readConversationTenant,
		Write:      t.coordinator.write,
	}
}

func (s *Service) spriteCommandDeps() coordinator.SpriteCommandDeps {
	return coordinator.SpriteCommandDeps{
		HostedSecrets: s.config.Hosted != nil && s.config.SecretKeys != nil,
		TokenPresent: func(ctx context.Context, scope hubscope.NativeScope) (bool, error) {
			status, err := s.readAccessSecretStatus(ctx, scope)
			return status.Present, err
		},
		ReadPool: func(ctx context.Context, scope hubscope.NativeScope) (coordinator.SpritePool, error) {
			view, err := s.readSpritePool(ctx, scope)
			if err != nil {
				return coordinator.SpritePool{}, err
			}
			result := coordinator.SpritePool{
				Settings: view.spritePoolSettings,
				Status: func(ctx context.Context, scope hubscope.NativeScope, connector string, project tracker.ProjectID) (any, error) {
					return s.coordinatorSpritePoolStatus(ctx, scope, view, connector, project)
				},
			}
			if len(view.Members) > 0 {
				last := view.Members[len(view.Members)-1]
				result.LastBootstrap = &coordinator.SpriteBootstrap{Name: last.Name, State: last.State, BootstrapLog: last.BootstrapLog}
			}
			return result, nil
		},
		RecordAuthority: s.spritePoolRecordAuthority,
		Update: func(ctx context.Context, scope hubscope.NativeScope, command tracker.Mutation, change coordinator.SpriteChange, resolve func(sprites.PoolSettings) (sprites.PoolSettings, *string, error)) error {
			return s.executeSpritePoolSettingsCommand(ctx, scope, command, change, resolve)
		},
		RecheckMutation: s.recheckProjectRecordMutation,
		StartPool:       s.startSpritePoolForQueue,
	}
}

func (t *coordinatorToolset) spritePreviewDeps() coordinator.SpritePreviewDeps {
	s := t.coordinator.server
	return coordinator.SpritePreviewDeps{ProposalDeps: t.proposalDeps(), Commands: s.spriteCommandDeps(), HostedPath: s.hostedPath}
}

func (s *Service) readCoordinatorProposalAgent(ctx context.Context, action chat.Action) (agent string, err error) {
	agent = coordinator.AgentName
	err = s.readConversationTenant(ctx, tracker.OrganizationID(action.OrganizationID), func(r tenantstore.Reader) error {
		agent, err = coordinator.ProposalAgent(ctx, r, action)
		return err
	})
	return agent, err
}

func (t *coordinatorToolset) projectAction(ctx context.Context, record conversationRecord, call runner.AgentToolCall, requireConfirmation ...bool) (any, error) {
	return t.projectActionDeps().Preview(ctx, record, call, requireConfirmation...)
}

func (t *coordinatorToolset) submitCoordinatorAction(ctx context.Context, record conversationRecord, call runner.AgentToolCall, action chat.Action) (any, error) {
	return coordinator.SubmitAction(ctx, record, call, action, t.proposalDeps().MessageID, t.state.model, t.coordinator.appendPreparedMessage)
}

func (s *Service) executeCoordinatorAction(ctx context.Context, action chat.Action) (chat.ActionExecution, error) {
	return s.actionDispatchDeps().Execute(ctx, action)
}

func (s *Service) authorizeCoordinatorAction(ctx context.Context, action chat.Action) (context.Context, error) {
	return s.actionAuthorizationDeps().Authorize(ctx, action)
}

func (t *coordinatorToolset) postActionRefusal(ctx context.Context, name string, failure error) error {
	return coordinator.PostRefusal(ctx, t.state.conversationID, name, failure, t.coordinator.appendPreparedMessage)
}

func (t *coordinatorToolset) projectActionDeps() coordinator.ProjectActionDeps {
	s := t.coordinator.server
	return coordinator.ProjectActionDeps{
		ProposalDeps:    t.proposalDeps(),
		GitHubAvailable: s.config.ReconcileBackend != nil,
		ReadProject:     s.readProjectRecord,
		ReadWorkflow:    s.projectRecordWorkflow,
		PreviewCommand: func(ctx context.Context, call operatortool.Call) (json.RawMessage, error) {
			return (hubProjectExecutor{s}).command(ctx, call, projectCommandPreview)
		},
		ObserveIssue:       s.nativeWorkObservation,
		ValidateMoveAnswer: s.validateCoordinatorMoveAnswer,
	}
}

func (s *Service) actionDispatchDeps() coordinator.ActionDispatchDeps {
	return coordinator.ActionDispatchDeps{
		Authorize:          s.authorizeCoordinatorAction,
		ProposalAgent:      s.coordinatorProposalAgent,
		WithAgent:          withCoordinatorAgent,
		CatalogWrite:       coordinatorCatalogWrite,
		ExecuteCatalog:     s.executeCoordinatorCatalogCall,
		ExecuteSettings:    s.executeCoordinatorSettingsAction,
		ExecuteArchive:     s.executeCoordinatorIssueArchive,
		ExecuteSplit:       s.executeCoordinatorIssueSplit,
		ExecuteRunner:      s.executeCoordinatorRunnerAction,
		PromoteSpriteToken: s.executePromoteSpritesToken,
		ExecuteSprite:      s.executeCoordinatorSpriteAction,
		ExecuteWorkflow:    s.executeCoordinatorWorkflowAction,
		ExecuteProject: func(ctx context.Context, call operatortool.Call) (json.RawMessage, error) {
			return (hubProjectExecutor{s}).command(ctx, call, projectCommandExecute)
		},
		StopRun:         s.executeCoordinatorStopRun,
		CreateIssue:     s.operatorCreateNativeWork,
		TransitionIssue: s.transitionNativeIssueCommandWithChat,
		MutateIssue:     s.operatorNativeWork,
	}
}

func (s *Service) actionAuthorizationDeps() coordinator.ActionAuthorizationDeps {
	return coordinator.ActionAuthorizationDeps{
		SettingsRequirement: coordinatorSettingsRequirement,
		CatalogWrite:        coordinatorCatalogWrite,
		CatalogRequirement:  coordinatorCatalogRequirement,
		Operator:            currentHubOperator,
		ReadConversation:    s.readPGConversation,
	}
}
