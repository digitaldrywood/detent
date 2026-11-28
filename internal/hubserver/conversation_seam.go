package hubserver

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type (
	ConversationConfig          = conversations.ConversationConfig
	ConversationUsage           = conversations.ConversationUsage
	ConversationUsageSink       = conversations.ConversationUsageSink
	ConversationWorkerAuthority = conversations.ConversationWorkerAuthority
	ConversationWorkerIdentity  = conversations.ConversationWorkerIdentity
)

func conversationInput(scope hubscope.NativeScope) hubauth.Input {
	return conversations.ScopeInput(scope)
}

func toFeatureError(err error) error { return hubapi.ToFeatureError(err) }

type conversationFeatureWorkerAuthority struct{ inner ConversationWorkerAuthority }

func (a conversationFeatureWorkerAuthority) VerifyConversationWorker(ctx context.Context, reader tenantstore.Reader, worker ConversationWorkerIdentity) error {
	return toFeatureError(a.inner.VerifyConversationWorker(ctx, reader, worker))
}

type conversationLinkMutation struct {
	inner *conversations.ConversationLinkMutation
}

func conversationLinkedWorkItem(plan hubrecords.WorkItemRecordPlan) conversations.LinkedWorkItem {
	return conversations.LinkedWorkItem{Existing: plan.Existing, Create: plan.Create, Issue: plan.Issue, ProjectName: plan.Project.Core.Name}
}

func (m *conversationLinkMutation) Prepare(ctx context.Context, plan hubrecords.WorkItemRecordPlan) error {
	return hubapi.FromFeatureError(m.inner.Prepare(ctx, conversationLinkedWorkItem(plan)))
}

func (m *conversationLinkMutation) Write(ctx context.Context, writer tenantstore.Writer, plan hubrecords.WorkItemRecordPlan) error {
	return hubapi.FromFeatureError(m.inner.Write(ctx, writer, conversationLinkedWorkItem(plan)))
}

func newConversationService(server *Service, cfg ConversationConfig) *conversationService {
	return conversations.NewConversationService(server.conversationServer(), cfg)
}

func (s *Service) conversationServer() *conversations.Server {
	return conversations.NewServer(s.conversationDeps())
}

func (s *Service) conversationContents(contents attachment.ArchiveContentStore) attachment.ArchiveContentStore {
	if s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
		contents = s.config.Hosted.SharedEntry.ArchiveContents
	}
	if contents != nil {
		return contents
	}
	call := s.config.Content
	if call == nil && s.config.Hosted != nil && s.config.Hosted.SharedEntry != nil {
		call = s.config.Hosted.SharedEntry.Content
	}
	if call != nil {
		return conversationArchiveStore{Call: call}
	}
	return nil
}

func (s *Service) conversationDeps() conversations.Deps {
	deps := conversations.Deps{
		Config: func() conversations.Config {
			cfg := conversations.Config{TenantRepository: s.config.TenantRepository, Now: s.config.Clock, ConversationDispatch: s.config.ConversationDispatch}
			if s.config.ConversationWorkerAuthority != nil {
				cfg.ConversationWorkerAuthority = conversationFeatureWorkerAuthority{s.config.ConversationWorkerAuthority}
			}
			return cfg
		},
		Conversations: func() *conversations.ConversationService { return s.conversations },
		Logger:        s.config.Logger,
		Contents:      s.conversationContents,
		NewCoordinator: func(service *conversationService) conversationCoordinator {
			return newConversationCoordinator(s, service)
		},
		NewControls:     conversations.NewControlRouter,
		NewControlTimer: s.config.Deps().NewConversationControlTimer,
		Ready:           s.ready.Load,
		Organization:    s.appOrganizationID,
		Authorization: hubscope.Authorization{
			Access: func(ctx context.Context, reader accessstore.Reader, scope hubscope.NativeScope, write bool) error {
				return toFeatureError(s.authorizeAccessScope(ctx, reader, scope, write))
			},
			WorkItem: func(ctx context.Context, reader accessstore.Reader, scope hubscope.NativeScope) error {
				return toFeatureError(s.authorizeWorkItemRecordScope(ctx, reader, scope))
			},
		},
		BrowserLink: func(ctx context.Context, link string) (string, error) {
			value, err := s.canonicalBrowserLink(ctx, link)
			return value, toFeatureError(err)
		},
		ArchiveContext: func(ctx context.Context, scope hubauth.Input, presign bool) context.Context {
			return s.archiveContext(ctx, hubscope.ScopeFromAuthInput(scope), presign)
		},
		WithArchive: func(ctx context.Context, raw []byte) ([]byte, error) {
			return withNativeArchive(raw, nativeArchiveDescriptor(ctx))
		},
		BindCoordinatorConnection: func(ctx context.Context, key string) (context.Context, error) {
			bound, err := s.bindCoordinatorConnection(ctx, key)
			return bound, toFeatureError(err)
		},
		AttachmentQuota: func(ctx context.Context, reader tenantstore.Reader) (int64, error) {
			limit, err := s.attachmentQuotaLimit(ctx, reader)
			return limit, toFeatureError(err)
		},
		LimitError: func(resource string, allowance, consumption int64) error {
			return &hostedplans.LimitError{Resource: resource, Allowance: allowance, Consumption: consumption}
		},
		ReadProject: func(ctx context.Context, scope hubauth.Input) (tracker.NativeProject, error) {
			record, err := s.readProjectRecord(ctx, hubscope.ScopeFromAuthInput(scope))
			if err != nil {
				return tracker.NativeProject{}, toFeatureError(err)
			}
			project, err := s.projectRecordNative(ctx, record)
			return project, toFeatureError(err)
		},
		ReadWorkItem: func(ctx context.Context, scope hubauth.Input, id string) (tracker.NativeIssue, error) {
			issue, err := s.readWorkItem(ctx, hubscope.ScopeFromAuthInput(scope), id)
			return issue, toFeatureError(err)
		},
		ReadNativeRuntime: func(ctx context.Context, scope hubauth.Input, item string) (tracker.NativeRuntimeEvidence, error) {
			runtime, err := s.readNativeRuntime(ctx, hubscope.ScopeFromAuthInput(scope), item, "")
			return runtime, toFeatureError(err)
		},
		WorkItemReceipt: func(ctx context.Context, scope hubauth.Input, operation, key, hash string) (bool, error) {
			_, found, err := s.workItemRecordReceipt(ctx, hubscope.ScopeFromAuthInput(scope), operation, key, hash)
			return found, toFeatureError(err)
		},
		LinkWorkItem: func(ctx context.Context, scope hubauth.Input, operation string, request tracker.CreateIssue, link *conversations.ConversationLinkMutation) error {
			_, err := s.executeWorkItemRecordMutationWithConversationLink(ctx, hubscope.ScopeFromAuthInput(scope), hubapi.NativeCommandOptions{OperationID: operation, Feature: "collaboration"}, request.Mutation, request, "", &conversationLinkMutation{inner: link})
			return toFeatureError(err)
		},
		PreferenceIssue: func(ctx context.Context, scope hubauth.Input, record conversationRecord, preferences conversation.Preferences) (conversations.PreferenceIssue, error) {
			issue, err := s.prepareConversationPreferenceIssue(ctx, hubscope.ScopeFromAuthInput(scope), record, preferences)
			return issue, toFeatureError(err)
		},
		GrantedProjects: func(ctx context.Context, reader accessstore.Reader, organization tracker.OrganizationID, credential hubauth.Credential, projects []accessstore.ProjectAccess) ([]accessstore.ProjectAccess, error) {
			granted, err := s.grantedAccessProjects(ctx, reader, organization, hubscope.NativeCredential(credential), projects)
			return granted, toFeatureError(err)
		},
		ProjectParticipantsUnavailable: hubapi.ErrProjectParticipants,
		RequireFeature: func(ctx context.Context, reader tenantstore.Reader, feature string, now time.Time) error {
			return toFeatureError(hubapi.FromFeatureError(s.database.plansService().RequireFeatureRecord(ctx, reader, feature, now)))
		},
		ModelChoiceGranted: func(ctx context.Context, reader tenantstore.Reader, organization tracker.OrganizationID, now time.Time) (bool, error) {
			if s.database.plansService().Plans() == nil || s.database.hostedOrganization != organization {
				return false, nil
			}
			entitlement, err := hubapi.FeatureResult(s.database.plansService().EntitlementRecord(ctx, reader, now))
			if err != nil {
				return false, toFeatureError(err)
			}
			return slices.Contains(entitlement.Features, "model_choice"), nil
		},
		CheckMonthlyLease: conversationCheckMonthlyLease,
		WorkerAuthorityAt: conversationWorkerAuthorityAt,
		IssueBrowserPath: func(ctx context.Context, query conversations.Queryer, id string) (string, error) {
			return readIssueBrowserPath(ctx, query, id)
		},
		ResolveWorkItems: func(ctx context.Context, query conversations.Queryer, records []*conversationRecord) error {
			return resolveConversationWorkItems(ctx, query, records)
		},
		APIError: func(c echo.Context, err error) error {
			return s.nativeAPIError(c, err)
		},
		InvalidRequest: s.invalidAPIRequest,
	}
	if s.notifications != nil {
		deps.Broker = s.notifications.Broker
	}
	if s.database != nil {
		deps.UsageSink = s.database
	}
	return deps
}

func conversationCheckMonthlyLease(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, lease string, now time.Time) error {
	return toFeatureError(checkMonthlyLeasePG(ctx, reader, project, lease, now))
}

func conversationWorkerAuthorityAt(now time.Time) ConversationWorkerAuthority {
	return conversationFeatureWorkerAuthority{conversations.NewTenantWorkerAuthority(func() time.Time { return now })}
}

type conversationAttachmentRecord = conversations.ConversationAttachmentRecord
type conversationContent = conversations.ConversationContent
type conversationCoordinator = conversations.ConversationCoordinator
type conversationCreateRequest = conversations.ConversationCreateRequest
type conversationDeltaFact = conversations.ConversationDeltaFact
type conversationListQuery = conversations.ConversationListQuery
type conversationMessageRecord = conversations.ConversationMessageRecord
type conversationMessageResource = conversations.ConversationMessageResource
type conversationMessagesPage = conversations.ConversationMessagesPage
type conversationModel = conversations.ConversationModel
type conversationPatchRequest = conversations.ConversationPatchRequest
type conversationPublicationPlan = conversations.ConversationPublicationPlan
type conversationRecord = conversations.ConversationRecord
type conversationService = conversations.ConversationService
type conversationSnapshot = conversations.ConversationSnapshot

const (
	conversationAuthorizeInterval  = conversations.ConversationAuthorizeInterval
	conversationHeartbeatInterval  = conversations.ConversationHeartbeatInterval
	conversationLifecycleBatchSize = conversations.ConversationLifecycleBatchSize
)

func contentArchiveID(p0 string, p1 attachment.ArchiveReference) string {
	return conversations.ContentArchiveID(p0, p1)
}

func conversationContentUnavailable() error {
	return hubapi.FromFeatureError(conversations.ConversationContentUnavailable())
}

func conversationDispatchUnavailable() error {
	return hubapi.FromFeatureError(conversations.ConversationDispatchUnavailable())
}

func conversationMessageFromStore(p0 conversationstore.Message) conversations.ConversationMessageRecord {
	return conversations.ConversationMessageFromStore(p0)
}

func conversationMessageToStore(p0 tracker.OrganizationID, p1 conversations.ConversationMessageRecord) conversationstore.Message {
	return conversations.ConversationMessageToStore(p0, p1)
}

func conversationOptional(p0 string) *string { return conversations.ConversationOptional(p0) }

func conversationReadAudienceRecord(p0 context.Context, p1 tenantstore.Reader, p2 tracker.OrganizationID, p3 hubapi.APICredential) (string, error) {
	value, err := conversations.ConversationReadAudienceRecord(p0, p1, p2, hubscope.AuthCredential(p3))
	return value, hubapi.FromFeatureError(err)
}

func conversationRecordFromStore(p0 conversationstore.Conversation) conversations.ConversationRecord {
	return conversations.ConversationRecordFromStore(p0)
}

func conversationRecordToStore(p0 conversations.ConversationRecord) conversationstore.Conversation {
	return conversations.ConversationRecordToStore(p0)
}

func conversationStale(p0 string) error {
	return hubapi.FromFeatureError(conversations.ConversationStale(p0))
}

func firstDispatchableState(p0 tracker.NativeProject) (string, bool) {
	return conversations.FirstDispatchableState(p0)
}

func messageContent(p0 conversations.ConversationMessageRecord) ([]byte, error) {
	r0, r1 := conversations.MessageContent(p0)
	return r0, hubapi.FromFeatureError(r1)
}

func newConversationArchiveReads(p0 context.Context) context.Context {
	return conversations.NewConversationArchiveReads(p0)
}

func projectConversation(p0 conversations.ConversationRecord) conversations.ConversationResource {
	return conversations.ProjectConversation(p0)
}

func projectMessage(p0 conversations.ConversationMessageRecord) conversations.ConversationMessageResource {
	return conversations.ProjectMessage(p0)
}

func recordPayload(p0 conversations.ConversationRecord) ([]byte, error) {
	r0, r1 := conversations.RecordPayload(p0)
	return r0, hubapi.FromFeatureError(r1)
}

func registerPreparedConversationObjects(p0 context.Context, p1 tenantstore.Writer, p2 tracker.OrganizationID, p3 conversations.PreparedConversationContents, p4 time.Time) error {
	return hubapi.FromFeatureError(conversations.RegisterPreparedConversationObjects(p0, p1, p2, p3, p4))
}

func staleExecutionAttempts(p0 error) (string, string, bool) {
	return conversations.StaleExecutionAttempts(toFeatureError(p0))
}

func translateConversationError(p0 error) error {
	return hubapi.FromFeatureError(conversations.TranslateConversationError(p0))
}

func (s *Service) authorizeConversationScopePG(ctx context.Context, scope hubscope.NativeScope, write bool) error {
	r0 := s.conversationServer().AuthorizeConversationScopePG(ctx, s.authInput(scope), write)
	return hubapi.FromFeatureError(r0)
}

func (s *Service) commandCreateConversation(ctx context.Context, scope hubscope.NativeScope, request conversations.ConversationCreateRequest) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandCreateConversation(ctx, s.authInput(scope), request)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) commandDeleteAttachment(ctx context.Context, scope hubscope.NativeScope, conversationID string, id string) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandDeleteAttachment(ctx, s.authInput(scope), conversationID, id)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) commandLinkConversation(ctx context.Context, scope hubscope.NativeScope, id string, request conversations.ConversationLinkRequest) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandLinkConversation(ctx, s.authInput(scope), id, request)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) commandPatchConversation(ctx context.Context, scope hubscope.NativeScope, id string, request conversations.ConversationPatchRequest) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandPatchConversation(ctx, s.authInput(scope), id, request)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) commandPostConversation(ctx context.Context, scope hubscope.NativeScope, id string, command conversation.Command) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandPostConversation(ctx, s.authInput(scope), id, command)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) commandUploadAttachment(ctx context.Context, scope hubscope.NativeScope, id string, upload conversations.ConversationAttachmentUpload) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandUploadAttachment(ctx, s.authInput(scope), id, upload)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) conversationDefaultModel() string {
	r0 := s.conversationServer().ConversationDefaultModel()
	return r0
}

func (s *Service) conversationModelChoiceGranted(ctx context.Context, reader tenantstore.Reader, organization tracker.OrganizationID, now time.Time) (bool, error) {
	r0, r1 := s.conversationServer().ConversationModelChoiceGranted(ctx, reader, organization, now)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) conversationModelChoices(ctx context.Context, reader tenantstore.Reader, projects []tracker.ProjectID, now time.Time) ([]conversations.ConversationModel, error) {
	r0, r1 := s.conversationServer().ConversationModelChoices(ctx, reader, projects, now)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) hasLunaCoordinator() bool {
	r0 := s.conversationServer().HasLunaCoordinator()
	return r0
}

func (s *Service) readAttachmentPG(ctx context.Context, scope hubscope.NativeScope, conversationID string, id string) (conversations.ConversationAttachmentRecord, []byte, error) {
	r0, r1, r2 := s.conversationServer().ReadAttachmentPG(ctx, s.authInput(scope), conversationID, id)
	return r0, r1, hubapi.FromFeatureError(r2)
}

func (s *Service) readConversationMessages(ctx context.Context, scope hubscope.NativeScope, id string, before int64, limit int) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadConversationMessages(ctx, s.authInput(scope), id, before, limit)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) readConversationObject(ctx context.Context, organization string, ref attachment.ArchiveReference, target any) error {
	r0 := s.conversationServer().ReadConversationObject(ctx, organization, ref, target)
	return hubapi.FromFeatureError(r0)
}

func (s *Service) readConversationSnapshot(ctx context.Context, scope hubscope.NativeScope, id string) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadConversationSnapshot(ctx, s.authInput(scope), id)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) readConversationSnapshotFor(ctx context.Context, scope hubscope.NativeScope, id string, item string, diagnoseMissing bool) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadConversationSnapshotFor(ctx, s.authInput(scope), id, item, diagnoseMissing)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) readConversationsPage(ctx context.Context, scope hubscope.NativeScope, filter conversations.ConversationListQuery) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadConversationsPage(ctx, s.authInput(scope), filter)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) readPGConversation(ctx context.Context, scope hubscope.NativeScope, id string) (conversations.ConversationRecord, error) {
	r0, r1 := s.conversationServer().ReadPGConversation(ctx, s.authInput(scope), id)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) readWorkItemReferences(ctx context.Context, scope hubscope.NativeScope, id string) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadWorkItemReferences(ctx, s.authInput(scope), id)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) readableConversationProjects(ctx context.Context, scope hubscope.NativeScope) ([]tracker.ProjectID, bool, error) {
	r0, r1, r2 := s.conversationServer().ReadableConversationProjects(ctx, s.authInput(scope))
	return r0, r1, hubapi.FromFeatureError(r2)
}

var _ conversations.CommandDeliverer = (*conversationTurnCoordinator)(nil)
