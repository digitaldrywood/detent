package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

type (
	ConversationConfig          = conversations.ConversationConfig
	ConversationUsage           = conversations.ConversationUsage
	ConversationUsageSink       = conversations.ConversationUsageSink
	ConversationWorkerAuthority = conversations.ConversationWorkerAuthority
	ConversationWorkerIdentity  = conversations.ConversationWorkerIdentity
)

func conversationInput(scope nativeScope) hubauth.Input {
	return hubauth.Input{Organization: scope.organization, Project: scope.project, Credential: authCredential(scope.credential), Admin: scope.requireHostedAdmin, Action: scope.action, IntegrationRepository: scope.integrationRepository, IntegrationOwned: scope.integrationOwned}
}

func conversationSubscribe(broker *eventbroker.Broker, key string) (*notificationSubscription, func()) {
	return (&notificationBroker{broker}).subscribe(key)
}

func toFeatureError(err error) error {
	var native *nativeError
	if !errors.As(err, &native) || native == nil {
		return err
	}
	failure := &hubapi.Error{Code: native.Code, Message: native.Message, CurrentRevision: native.CurrentRevision, Details: native.Details, Status: native.status, Public: native.publicMessage}
	return failure.WithLogAttributes(native.logAttributes...)
}

type conversationFeatureWorkerAuthority struct{ inner ConversationWorkerAuthority }

func (a conversationFeatureWorkerAuthority) VerifyConversationWorker(ctx context.Context, reader tenantstore.Reader, worker ConversationWorkerIdentity) error {
	return toFeatureError(a.inner.VerifyConversationWorker(ctx, reader, worker))
}

type conversationLinkMutation struct {
	inner *conversations.ConversationLinkMutation
}

func conversationLinkedWorkItem(plan workItemRecordPlan) conversations.LinkedWorkItem {
	return conversations.LinkedWorkItem{Existing: plan.existing, Create: plan.create, Issue: plan.issue, ProjectName: plan.project.Core.Name}
}

func (m *conversationLinkMutation) Prepare(ctx context.Context, plan workItemRecordPlan) error {
	return fromFeatureError(m.inner.Prepare(ctx, conversationLinkedWorkItem(plan)))
}

func (m *conversationLinkMutation) Write(ctx context.Context, writer tenantstore.Writer, plan workItemRecordPlan) error {
	return fromFeatureError(m.inner.Write(ctx, writer, conversationLinkedWorkItem(plan)))
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
			cfg := conversations.Config{TenantRepository: s.config.TenantRepository, Now: s.config.now, ConversationDispatch: s.config.ConversationDispatch}
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
		NewControls: func(service *conversationService) conversationControlRouter {
			return newConversationControlRouter(s, service)
		},
		Ready:        s.ready.Load,
		Organization: s.appOrganizationID,
		Authorize: func(ctx context.Context, reader accessstore.Reader, scope hubauth.Input, write bool) error {
			return toFeatureError(s.authorizeAccessScope(ctx, reader, nativeAuthInput(scope), write))
		},
		AuthorizeWorkItem: func(ctx context.Context, reader accessstore.Reader, scope hubauth.Input) error {
			return toFeatureError(s.authorizeWorkItemRecordScope(ctx, reader, nativeAuthInput(scope)))
		},
		BrowserLink: func(ctx context.Context, link string) (string, error) {
			value, err := s.canonicalBrowserLink(ctx, link)
			return value, toFeatureError(err)
		},
		ArchiveContext: func(ctx context.Context, scope hubauth.Input, presign bool) context.Context {
			return s.archiveContext(ctx, nativeAuthInput(scope), presign)
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
			return &hostedLimitError{Resource: resource, Allowance: allowance, Consumption: consumption}
		},
		ReadProject: func(ctx context.Context, scope hubauth.Input) (tracker.NativeProject, error) {
			record, err := s.readProjectRecord(ctx, nativeAuthInput(scope))
			if err != nil {
				return tracker.NativeProject{}, toFeatureError(err)
			}
			project, err := s.projectRecordNative(ctx, record)
			return project, toFeatureError(err)
		},
		ReadWorkItem: func(ctx context.Context, scope hubauth.Input, id string) (tracker.NativeIssue, error) {
			issue, err := s.readWorkItem(ctx, nativeAuthInput(scope), id)
			return issue, toFeatureError(err)
		},
		ReadNativeRuntime: func(ctx context.Context, scope hubauth.Input, item string) (tracker.NativeRuntimeEvidence, error) {
			runtime, err := s.readNativeRuntime(ctx, nativeAuthInput(scope), item, "")
			return runtime, toFeatureError(err)
		},
		WorkItemReceipt: func(ctx context.Context, scope hubauth.Input, operation, key, hash string) (bool, error) {
			_, found, err := s.workItemRecordReceipt(ctx, nativeAuthInput(scope), operation, key, hash)
			return found, toFeatureError(err)
		},
		LinkWorkItem: func(ctx context.Context, scope hubauth.Input, operation string, request tracker.CreateIssue, link *conversations.ConversationLinkMutation) error {
			_, err := s.executeWorkItemRecordMutationWithConversationLink(ctx, nativeAuthInput(scope), nativeCommandOptions{OperationID: operation, Feature: "collaboration"}, request.Mutation, request, "", &conversationLinkMutation{inner: link})
			return toFeatureError(err)
		},
		PreferenceIssue: func(ctx context.Context, scope hubauth.Input, record conversationRecord, preferences conversation.Preferences) (conversations.PreferenceIssue, error) {
			issue, err := s.prepareConversationPreferenceIssue(ctx, nativeAuthInput(scope), record, preferences)
			return issue, toFeatureError(err)
		},
		GrantedProjects: func(ctx context.Context, reader accessstore.Reader, organization tracker.OrganizationID, credential hubauth.Credential, projects []accessstore.ProjectAccess) ([]accessstore.ProjectAccess, error) {
			granted, err := s.grantedAccessProjects(ctx, reader, organization, nativeCredential(credential), projects)
			return granted, toFeatureError(err)
		},
		ProjectParticipantsUnavailable: ErrProjectParticipants,
		RequireFeature: func(ctx context.Context, reader tenantstore.Reader, feature string, now time.Time) error {
			return toFeatureError(s.database.requireHostedFeatureRecord(ctx, reader, feature, now))
		},
		ModelChoiceGranted: func(ctx context.Context, reader tenantstore.Reader, organization tracker.OrganizationID, now time.Time) (bool, error) {
			if s.database.plans() == nil || s.database.hostedOrganization != organization {
				return false, nil
			}
			entitlement, err := s.database.hostedEntitlementRecord(ctx, reader, now)
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
	return conversationFeatureWorkerAuthority{tenantRunnerAuthority{now: func() time.Time { return now }}}
}

func conversationLeaseCurrentPG(ctx context.Context, reader tenantstore.Reader, record conversationRecord, now time.Time) (bool, error) {
	server := conversations.NewServer(conversations.Deps{CheckMonthlyLease: conversationCheckMonthlyLease, WorkerAuthorityAt: conversationWorkerAuthorityAt})
	return server.ConversationLeaseCurrentPG(ctx, reader, record, now)
}

type conversationArchiveReads = conversations.ConversationArchiveReads
type conversationAttachmentRecord = conversations.ConversationAttachmentRecord
type conversationAttachmentUpload = conversations.ConversationAttachmentUpload
type conversationContent = conversations.ConversationContent
type conversationControlRouter = conversations.ConversationControlRouter
type conversationCoordinator = conversations.ConversationCoordinator
type conversationCreateRequest = conversations.ConversationCreateRequest
type conversationDeltaFact = conversations.ConversationDeltaFact
type conversationExecutionResource = conversations.ConversationExecutionResource
type conversationLinkRequest = conversations.ConversationLinkRequest
type conversationListQuery = conversations.ConversationListQuery
type conversationMessageRecord = conversations.ConversationMessageRecord
type conversationMessageReference = conversations.ConversationMessageReference
type conversationMessageResource = conversations.ConversationMessageResource
type conversationMessagesPage = conversations.ConversationMessagesPage
type conversationModel = conversations.ConversationModel
type conversationPatchRequest = conversations.ConversationPatchRequest
type conversationPublicationKey = conversations.ConversationPublicationKey
type conversationPublicationPlan = conversations.ConversationPublicationPlan
type conversationQuestionRecord = conversations.ConversationQuestionRecord
type conversationRecord = conversations.ConversationRecord
type conversationResumeDecision = conversations.ConversationResumeDecision
type conversationResumeResource = conversations.ConversationResumeResource
type conversationService = conversations.ConversationService
type conversationSnapshot = conversations.ConversationSnapshot
type conversationTranscriptEntry = conversations.ConversationTranscriptEntry
type conversationWorkItem = conversations.ConversationWorkItem
type conversationWorkerIdentity = conversations.WorkerIdentity
type pgWorkerPublicationKey = conversations.PgWorkerPublicationKey
type preparedConversationContents = conversations.PreparedConversationContents
type queryMessageData = conversations.QueryMessageData

const (
	conversationAttachmentFieldBytes   = conversations.ConversationAttachmentFieldBytes
	conversationAttachmentRequestBytes = conversations.ConversationAttachmentRequestBytes
	conversationAuthorizeInterval      = conversations.ConversationAuthorizeInterval
	conversationClosedAccessRevoked    = conversations.ConversationClosedAccessRevoked
	conversationClosedCursorExpired    = conversations.ConversationClosedCursorExpired
	conversationClosedServerError      = conversations.ConversationClosedServerError
	conversationClosedShutdown         = conversations.ConversationClosedShutdown
	conversationControlMaxWait         = conversations.ConversationControlMaxWait
	conversationHeaderAttempt          = conversations.ConversationHeaderAttempt
	conversationHeaderFencingToken     = conversations.ConversationHeaderFencingToken
	conversationHeaderLease            = conversations.ConversationHeaderLease
	conversationHeartbeatInterval      = conversations.ConversationHeartbeatInterval
	conversationLeaseLostError         = conversations.ConversationLeaseLostError
	conversationLifecycleBatchSize     = conversations.ConversationLifecycleBatchSize
	conversationMaxDeltaBytes          = conversations.ConversationMaxDeltaBytes
	conversationMaxItemDataBytes       = conversations.ConversationMaxItemDataBytes
	conversationMaxSummaryBytes        = conversations.ConversationMaxSummaryBytes
	conversationMaxTurnEvents          = conversations.ConversationMaxTurnEvents
	conversationMessagePage            = conversations.ConversationMessagePage
	conversationOriginUser             = conversations.ConversationOriginUser
	conversationOriginWorker           = conversations.ConversationOriginWorker
	conversationQuestionPrompt         = conversations.ConversationQuestionPrompt
	conversationTranscriptMessages     = conversations.ConversationTranscriptMessages
	conversationTranscriptRunes        = conversations.ConversationTranscriptRunes
)

var (
	errConversationCursorExpired = conversations.ErrConversationCursorExpired
)

func appendPGConversationEvent(p0 context.Context, p1 tenantstore.Writer, p2 tracker.OrganizationID, p3 string, p4 conversation.EventType, p5 any, p6 time.Time) error {
	return fromFeatureError(conversations.AppendPGConversationEvent(p0, p1, p2, p3, p4, p5, p6))
}

func authorizeConversationLinkedItemPG(p0 context.Context, p1 tenantstore.Reader, p2 nativeScope, p3 conversations.ConversationRecord) error {
	return fromFeatureError(conversations.AuthorizeConversationLinkedItemPG(p0, p1, conversationInput(p2), p3))
}

func boundRunes(p0 string, p1 int) string { return conversations.BoundRunes(p0, p1) }

func contentArchiveID(p0 string, p1 attachment.ArchiveReference) string {
	return conversations.ContentArchiveID(p0, p1)
}

func conversationActorFor(p0 nativeScope) conversation.Actor {
	return conversations.ConversationActorFor(conversationInput(p0))
}

func conversationAgentOverrideBody(p0 string, p1 conversation.Preferences) string {
	return conversations.ConversationAgentOverrideBody(p0, p1)
}

func conversationAttachmentFromStore(p0 conversationstore.Attachment) conversations.ConversationAttachmentRecord {
	return conversations.ConversationAttachmentFromStore(p0)
}

func conversationBetweenTurns(p0 conversation.Execution) bool {
	return conversations.ConversationBetweenTurns(p0)
}

func conversationContentUnavailable() error {
	return fromFeatureError(conversations.ConversationContentUnavailable())
}

func conversationDeliveryForExecution(p0 conversation.ExecutionStatus) conversation.Delivery {
	return conversations.ConversationDeliveryForExecution(p0)
}

func conversationDispatchUnavailable() error {
	return fromFeatureError(conversations.ConversationDispatchUnavailable())
}

func conversationMessageFromStore(p0 conversationstore.Message) conversations.ConversationMessageRecord {
	return conversations.ConversationMessageFromStore(p0)
}

func conversationMessageToStore(p0 tracker.OrganizationID, p1 conversations.ConversationMessageRecord) conversationstore.Message {
	return conversations.ConversationMessageToStore(p0, p1)
}

func conversationOlderCursor(p0 []conversations.ConversationMessageResource) (bool, *string) {
	return conversations.ConversationOlderCursor(p0)
}

func conversationOptional(p0 string) *string { return conversations.ConversationOptional(p0) }

func conversationQuestionID(p0 context.Context) string {
	return conversations.ConversationQuestionID(p0)
}

func conversationReadAudienceRecord(p0 context.Context, p1 tenantstore.Reader, p2 tracker.OrganizationID, p3 apiCredential) (string, error) {
	value, err := conversations.ConversationReadAudienceRecord(p0, p1, p2, authCredential(p3))
	return value, fromFeatureError(err)
}

func conversationRecordFromStore(p0 conversationstore.Conversation) conversations.ConversationRecord {
	return conversations.ConversationRecordFromStore(p0)
}

func conversationRecordToStore(p0 conversations.ConversationRecord) conversationstore.Conversation {
	return conversations.ConversationRecordToStore(p0)
}

func conversationResumeKind(p0 conversations.ConversationResumeResource) string {
	return conversations.ConversationResumeKind(p0)
}

func conversationStale(p0 string) error { return fromFeatureError(conversations.ConversationStale(p0)) }

func conversationStaleExecutionError(p0 string, p1 string, p2 error) error {
	return fromFeatureError(conversations.ConversationStaleExecutionError(p0, p1, toFeatureError(p2)))
}

func conversationTranscriptNotice(p0 int) string {
	return conversations.ConversationTranscriptNotice(p0)
}

func conversationUnsupported(p0 string) error {
	return fromFeatureError(conversations.ConversationUnsupported(p0))
}

func conversationWorkerAuthorityUnavailable() error {
	return fromFeatureError(conversations.ConversationWorkerAuthorityUnavailable())
}

func deriveConversationTitle(p0 string) string { return conversations.DeriveConversationTitle(p0) }

func firstDispatchableState(p0 tracker.NativeProject) (string, bool) {
	return conversations.FirstDispatchableState(p0)
}

func messageContent(p0 conversations.ConversationMessageRecord) ([]byte, error) {
	r0, r1 := conversations.MessageContent(p0)
	return r0, fromFeatureError(r1)
}

func newConversationArchiveReads(p0 context.Context) context.Context {
	return conversations.NewConversationArchiveReads(p0)
}

func newConversationStream(p0 echo.Context) *conversations.ConversationStream {
	return conversations.NewConversationStream(p0)
}

func preparePGConversationMessage(p0 context.Context, p1 *conversations.ConversationPublicationPlan, p2 *conversations.ConversationMessageRecord) error {
	return fromFeatureError(conversations.PreparePGConversationMessage(p0, p1, p2))
}

func preparedPGConversationQuestion(p0 context.Context, p1 conversationstore.Question) (conversationstore.Question, error) {
	r0, r1 := conversations.PreparedPGConversationQuestion(p0, p1)
	return r0, fromFeatureError(r1)
}

func projectAttachments(p0 []conversations.ConversationAttachmentRecord) []conversation.Attachment {
	return conversations.ProjectAttachments(p0)
}

func projectConversation(p0 conversations.ConversationRecord) conversations.ConversationResource {
	return conversations.ProjectConversation(p0)
}

func projectExecution(p0 conversation.Execution) conversations.ConversationExecutionResource {
	return conversations.ProjectExecution(p0)
}

func projectMessage(p0 conversations.ConversationMessageRecord) conversations.ConversationMessageResource {
	return conversations.ProjectMessage(p0)
}

func readConversationRecordFromTenant(p0 context.Context, p1 tenantstore.Reader, p2 tracker.ProjectID, p3 string) (conversations.ConversationRecord, error) {
	r0, r1 := conversations.ReadConversationRecordFromTenant(p0, p1, p2, p3)
	return r0, fromFeatureError(r1)
}

func recordPayload(p0 conversations.ConversationRecord) ([]byte, error) {
	r0, r1 := conversations.RecordPayload(p0)
	return r0, fromFeatureError(r1)
}

func registerPreparedConversationObjects(p0 context.Context, p1 tenantstore.Writer, p2 tracker.OrganizationID, p3 conversations.PreparedConversationContents, p4 time.Time) error {
	return fromFeatureError(conversations.RegisterPreparedConversationObjects(p0, p1, p2, p3, p4))
}

func requireConversationRecordOwner(p0 conversations.ConversationRecord, p1 conversations.WorkerIdentity, p2 bool) error {
	return fromFeatureError(conversations.RequireConversationRecordOwner(p0, p1, p2))
}

func requireConversationRemoval(p0 conversations.ConversationRecord) error {
	return fromFeatureError(conversations.RequireConversationRemoval(p0))
}

func staleExecutionAttempts(p0 error) (string, string, bool) {
	return conversations.StaleExecutionAttempts(toFeatureError(p0))
}

func translateConversationError(p0 error) error {
	return fromFeatureError(conversations.TranslateConversationError(p0))
}

func updatePGWorkerMessage(p0 context.Context, p1 tenantstore.Writer, p2 conversationstore.Message, p3 time.Time, p4 *conversation.ReceiptError) error {
	return fromFeatureError(conversations.UpdatePGWorkerMessage(p0, p1, p2, p3, p4))
}
func (s *Service) authorizeConversationRemoval(ctx context.Context, r tenantstore.Reader, scope nativeScope, record conversations.ConversationRecord) error {
	r0 := s.conversationServer().AuthorizeConversationRemoval(ctx, r, s.authInput(scope), record)
	return fromFeatureError(r0)
}

func (s *Service) authorizeConversationScopePG(ctx context.Context, scope nativeScope, write bool) error {
	r0 := s.conversationServer().AuthorizeConversationScopePG(ctx, s.authInput(scope), write)
	return fromFeatureError(r0)
}

func (s *Service) authorizeConversationWorkerLeasePG(ctx context.Context, reader tenantstore.Reader, scope nativeScope, itemID string, identity conversations.WorkerIdentity, runID string, allowReleased bool, allowUnstarted bool) (leasestore.Lease, leasestore.Attempt, error) {
	r0, r1, r2 := s.conversationServer().AuthorizeConversationWorkerLeasePG(ctx, reader, s.authInput(scope), itemID, identity, runID, allowReleased, allowUnstarted)
	return r0, r1, fromFeatureError(r2)
}

func (s *Service) authorizeConversationWorkerPG(ctx context.Context, reader tenantstore.Reader, scope nativeScope, record conversations.ConversationRecord, identity conversations.WorkerIdentity, runID string, allowReleased bool, allowUnstarted bool) (leasestore.Lease, error) {
	r0, r1 := s.conversationServer().AuthorizeConversationWorkerPG(ctx, reader, s.authInput(scope), record, identity, runID, allowReleased, allowUnstarted)
	return r0, fromFeatureError(r1)
}

func (s *Service) commandCreateConversation(ctx context.Context, scope nativeScope, request conversations.ConversationCreateRequest) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandCreateConversation(ctx, s.authInput(scope), request)
	return r0, fromFeatureError(r1)
}

func (s *Service) commandDeleteAttachment(ctx context.Context, scope nativeScope, conversationID string, id string) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandDeleteAttachment(ctx, s.authInput(scope), conversationID, id)
	return r0, fromFeatureError(r1)
}

func (s *Service) commandLinkConversation(ctx context.Context, scope nativeScope, id string, request conversations.ConversationLinkRequest) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandLinkConversation(ctx, s.authInput(scope), id, request)
	return r0, fromFeatureError(r1)
}

func (s *Service) commandPatchConversation(ctx context.Context, scope nativeScope, id string, request conversations.ConversationPatchRequest) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandPatchConversation(ctx, s.authInput(scope), id, request)
	return r0, fromFeatureError(r1)
}

func (s *Service) commandPatchConversationPG(ctx context.Context, scope nativeScope, id string, request conversations.ConversationPatchRequest, title string) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandPatchConversationPG(ctx, s.authInput(scope), id, request, title)
	return r0, fromFeatureError(r1)
}

func (s *Service) commandPostConversation(ctx context.Context, scope nativeScope, id string, command conversation.Command) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandPostConversation(ctx, s.authInput(scope), id, command)
	return r0, fromFeatureError(r1)
}

func (s *Service) commandUploadAttachment(ctx context.Context, scope nativeScope, id string, upload conversations.ConversationAttachmentUpload) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandUploadAttachment(ctx, s.authInput(scope), id, upload)
	return r0, fromFeatureError(r1)
}

func (s *Service) conversationDefaultModel() string {
	r0 := s.conversationServer().ConversationDefaultModel()
	return r0
}

func (s *Service) conversationModelChoiceGranted(ctx context.Context, reader tenantstore.Reader, organization tracker.OrganizationID, now time.Time) (bool, error) {
	r0, r1 := s.conversationServer().ConversationModelChoiceGranted(ctx, reader, organization, now)
	return r0, fromFeatureError(r1)
}

func (s *Service) conversationModelChoices(ctx context.Context, reader tenantstore.Reader, projects []tracker.ProjectID, now time.Time) ([]conversations.ConversationModel, error) {
	r0, r1 := s.conversationServer().ConversationModelChoices(ctx, reader, projects, now)
	return r0, fromFeatureError(r1)
}

func (s *Service) hasLunaCoordinator() bool {
	r0 := s.conversationServer().HasLunaCoordinator()
	return r0
}

func (s *Service) hydratePGConversationQuestion(ctx context.Context, organization tracker.OrganizationID, question conversationstore.Question) (conversationstore.Question, error) {
	r0, r1 := s.conversationServer().HydratePGConversationQuestion(ctx, organization, question)
	return r0, fromFeatureError(r1)
}

func (s *Service) readAttachmentPG(ctx context.Context, scope nativeScope, conversationID string, id string) (conversations.ConversationAttachmentRecord, []byte, error) {
	r0, r1, r2 := s.conversationServer().ReadAttachmentPG(ctx, s.authInput(scope), conversationID, id)
	return r0, r1, fromFeatureError(r2)
}

func (s *Service) readConversationMessages(ctx context.Context, scope nativeScope, id string, before int64, limit int) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadConversationMessages(ctx, s.authInput(scope), id, before, limit)
	return r0, fromFeatureError(r1)
}

func (s *Service) readConversationObject(ctx context.Context, organization string, ref attachment.ArchiveReference, target any) error {
	r0 := s.conversationServer().ReadConversationObject(ctx, organization, ref, target)
	return fromFeatureError(r0)
}

func (s *Service) readConversationSnapshot(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadConversationSnapshot(ctx, s.authInput(scope), id)
	return r0, fromFeatureError(r1)
}

func (s *Service) readConversationSnapshotFor(ctx context.Context, scope nativeScope, id string, item string, diagnoseMissing bool) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadConversationSnapshotFor(ctx, s.authInput(scope), id, item, diagnoseMissing)
	return r0, fromFeatureError(r1)
}

func (s *Service) readConversationsPage(ctx context.Context, scope nativeScope, filter conversations.ConversationListQuery) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadConversationsPage(ctx, s.authInput(scope), filter)
	return r0, fromFeatureError(r1)
}

func (s *Service) readPGConversation(ctx context.Context, scope nativeScope, id string) (conversations.ConversationRecord, error) {
	r0, r1 := s.conversationServer().ReadPGConversation(ctx, s.authInput(scope), id)
	return r0, fromFeatureError(r1)
}

func (s *Service) readPGConversationPage(ctx context.Context, scope nativeScope, id string, before int64, limit int) (conversations.ConversationPageRead, error) {
	r0, r1 := s.conversationServer().ReadPGConversationPage(ctx, s.authInput(scope), id, before, limit)
	return r0, fromFeatureError(r1)
}

func (s *Service) readWorkItemConversationSnapshot(ctx context.Context, scope nativeScope, item string) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadWorkItemConversationSnapshot(ctx, s.authInput(scope), item)
	return r0, fromFeatureError(r1)
}

func (s *Service) readWorkItemReferences(ctx context.Context, scope nativeScope, id string) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().ReadWorkItemReferences(ctx, s.authInput(scope), id)
	return r0, fromFeatureError(r1)
}

func (s *Service) readableConversationProjects(ctx context.Context, scope nativeScope) ([]tracker.ProjectID, bool, error) {
	r0, r1, r2 := s.conversationServer().ReadableConversationProjects(ctx, s.authInput(scope))
	return r0, r1, fromFeatureError(r2)
}

func (s *Service) replayPGConversationEvents(ctx context.Context, scope nativeScope, stream *conversations.ConversationStream, id string, cursor int64, through int64) (int64, error) {
	r0, r1 := s.conversationServer().ReplayPGConversationEvents(ctx, s.authInput(scope), stream, id, cursor, through)
	return r0, fromFeatureError(r1)
}

func expirePGOutstandingQuestions(ctx context.Context, writer conversationstore.Writer, conversationID, attemptID, turnID string, expire func(conversationstore.Question) error) error {
	return fromFeatureError(conversations.ExpirePGOutstandingQuestions(ctx, writer, conversationID, attemptID, turnID, expire))
}

func settlePGWorkerMessages(ctx context.Context, writer tenantstore.Writer, record *conversationRecord, status conversation.ExecutionStatus, now time.Time, limit int) error {
	return fromFeatureError(conversations.SettlePGWorkerMessages(ctx, writer, record, status, now, limit))
}

const conversationPGWorkerPageSize = conversations.ConversationPGWorkerPageSize

var _ conversations.CommandDeliverer = (*conversationTurnCoordinator)(nil)
