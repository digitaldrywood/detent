package hubserver

import (
	"context"
	"encoding/json"
	"time"

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
