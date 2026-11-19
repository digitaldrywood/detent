package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/hubserver/conversationstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventbroker"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationContentsKey = conversations.ConversationContentsKey
type conversationWorkerRouter = conversations.WorkerRouter
type conversationBindResponse = conversations.WorkerBindResponse
type conversationControlsResponse = conversations.WorkerControlsResponse
type conversationTurnEventsResponse = conversations.WorkerTurnEventsResponse
type conversationCreatedResponse = conversations.ConversationCreatedResponse
type conversationFirstMessage = conversations.ConversationFirstMessage
type conversationLinkIssue = conversations.ConversationLinkIssue
type conversationLinkResult = conversations.ConversationLinkResult
type conversationLinkedIssue = conversations.ConversationLinkedIssue
type conversationListPage = conversations.ConversationListPage
type conversationQuestionContent = conversations.ConversationQuestionContent
type conversationRecordContent = conversations.ConversationRecordContent
type conversationReferenceResource = conversations.ConversationReferenceResource
type conversationResource = conversations.ConversationResource
type conversationStore = conversations.ConversationStore
type conversationStream = conversations.ConversationStream
type conversationWorkItemReferencePage = conversations.ConversationWorkItemReferencePage
type conversationWorkItemResource = conversations.ConversationWorkItemResource

const (
	conversationDefaultTitle          = conversations.ConversationDefaultTitle
	conversationDispatchLater         = conversations.ConversationDispatchLater
	conversationDispatchNow           = conversations.ConversationDispatchNow
	conversationReferenceExcerptRunes = conversations.ConversationReferenceExcerptRunes
	conversationSnapshotQuestions     = conversations.ConversationSnapshotQuestions
)

func conversationArchiveCache(p0 context.Context) *conversations.ConversationArchiveReads {
	return conversations.ConversationArchiveCache(p0)
}

func conversationAttachmentURL(p0 tracker.OrganizationID, p1 tracker.ProjectID, p2 string, p3 string) string {
	return conversations.ConversationAttachmentURL(p0, p1, p2, p3)
}

func conversationModelFromReport(p0 providercapacity.Report, p1 string) conversations.ConversationModel {
	return conversations.ConversationModelFromReport(p0, p1)
}

func preparePGWorkerEvent(p0 context.Context, p1 *conversations.ConversationPublicationPlan, p2 any) error {
	return hubapi.FromFeatureError(conversations.PreparePGWorkerEvent(p0, p1, p2))
}

func conversationTime(p0 time.Time) string { return conversations.ConversationTime(p0) }

func coordinatorConnectionData(p0 context.Context) json.RawMessage {
	return conversations.CoordinatorConnectionData(p0)
}

func newConversationStore(p0 *sql.DB) *conversations.ConversationStore {
	return conversations.NewConversationStore(p0)
}

func questionPayload(p0 conversations.ConversationQuestionRecord) ([]byte, error) {
	r0, r1 := conversations.QuestionPayload(p0)
	return r0, hubapi.FromFeatureError(r1)
}

func conversationSubscribe(broker *eventbroker.Broker, key string) (*notificationSubscription, func()) {
	return (&notificationBroker{broker}).subscribe(key)
}

func (s *Service) registerConversationAPIRoutes(e *echo.Echo) {
	conversations.RegisterAPIRoutes(e.Group(""), s.conversationHTTPDeps())
}

func (s *Service) streamConversationEvents(c echo.Context) error {
	return s.conversationHTTP().StreamEvents(c)
}

func (s *Service) archiveConversation(c echo.Context) error {
	return s.conversationHTTP().Archive(c)
}

func (s *Service) deleteConversation(c echo.Context) error {
	return s.conversationHTTP().Delete(c)
}

func (s *Service) getConversation(c echo.Context) error {
	return s.conversationHTTP().Get(c)
}

func (s *Service) reauthorizeConversationStream(c echo.Context, scope *hubscope.NativeScope, conversationID string) (conversationRecord, error) {
	record, err := s.conversationHTTP().ReauthorizeStream(c, scope, conversationID)
	return record, hubapi.FromFeatureError(err)
}

func conversationStreamCloseReason(err error) string { return conversations.StreamCloseReason(err) }

func (s *Service) conversationHTTP() *conversations.HTTP {
	return conversations.NewHTTP(s.conversationHTTPDeps())
}

func conversationLeaseCurrentPG(ctx context.Context, reader tenantstore.Reader, record conversationRecord, now time.Time) (bool, error) {
	server := conversations.NewServer(conversations.Deps{CheckMonthlyLease: conversationCheckMonthlyLease, WorkerAuthorityAt: conversationWorkerAuthorityAt})
	return server.ConversationLeaseCurrentPG(ctx, reader, record, now)
}

type conversationControlRouter = conversations.ConversationControlRouter

type conversationQuestionRecord = conversations.ConversationQuestionRecord

type conversationWorkerIdentity = conversations.WorkerIdentity

type pgWorkerPublicationKey = conversations.PgWorkerPublicationKey

type preparedConversationContents = conversations.PreparedConversationContents

const conversationClosedAccessRevoked = conversations.ConversationClosedAccessRevoked

const conversationClosedCursorExpired = conversations.ConversationClosedCursorExpired

const conversationClosedServerError = conversations.ConversationClosedServerError

const conversationClosedShutdown = conversations.ConversationClosedShutdown

const conversationHeaderAttempt = conversations.ConversationHeaderAttempt

const conversationHeaderFencingToken = conversations.ConversationHeaderFencingToken

const conversationHeaderLease = conversations.ConversationHeaderLease

const conversationLeaseLostError = conversations.ConversationLeaseLostError

const conversationMaxDeltaBytes = conversations.ConversationMaxDeltaBytes

const conversationOriginUser = conversations.ConversationOriginUser

const conversationOriginWorker = conversations.ConversationOriginWorker

var errConversationCursorExpired = conversations.ErrConversationCursorExpired

func appendPGConversationEvent(p0 context.Context, p1 tenantstore.Writer, p2 tracker.OrganizationID, p3 string, p4 conversation.EventType, p5 any, p6 time.Time) error {
	return hubapi.FromFeatureError(conversations.AppendPGConversationEvent(p0, p1, p2, p3, p4, p5, p6))
}

func conversationTranscriptNotice(p0 int) string {
	return conversations.ConversationTranscriptNotice(p0)
}

func deriveConversationTitle(p0 string) string { return conversations.DeriveConversationTitle(p0) }

func newConversationStream(p0 echo.Context) *conversations.ConversationStream {
	return conversations.NewConversationStream(p0)
}

func preparePGConversationMessage(p0 context.Context, p1 *conversations.ConversationPublicationPlan, p2 *conversations.ConversationMessageRecord) error {
	return hubapi.FromFeatureError(conversations.PreparePGConversationMessage(p0, p1, p2))
}

func preparedPGConversationQuestion(p0 context.Context, p1 conversationstore.Question) (conversationstore.Question, error) {
	r0, r1 := conversations.PreparedPGConversationQuestion(p0, p1)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) authorizeConversationWorkerLeasePG(ctx context.Context, reader tenantstore.Reader, scope hubscope.NativeScope, itemID string, identity conversations.WorkerIdentity, runID string, allowReleased bool, allowUnstarted bool) (leasestore.Lease, leasestore.Attempt, error) {
	r0, r1, r2 := s.conversationServer().AuthorizeConversationWorkerLeasePG(ctx, reader, s.authInput(scope), itemID, identity, runID, allowReleased, allowUnstarted)
	return r0, r1, hubapi.FromFeatureError(r2)
}

func (s *Service) commandPatchConversationPG(ctx context.Context, scope hubscope.NativeScope, id string, request conversations.ConversationPatchRequest, title string) (json.RawMessage, error) {
	r0, r1 := s.conversationServer().CommandPatchConversationPG(ctx, s.authInput(scope), id, request, title)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) readPGConversationPage(ctx context.Context, scope hubscope.NativeScope, id string, before int64, limit int) (conversations.ConversationPageRead, error) {
	r0, r1 := s.conversationServer().ReadPGConversationPage(ctx, s.authInput(scope), id, before, limit)
	return r0, hubapi.FromFeatureError(r1)
}

func (s *Service) replayPGConversationEvents(ctx context.Context, scope hubscope.NativeScope, stream *conversations.ConversationStream, id string, cursor int64, through int64) (int64, error) {
	r0, r1 := s.conversationServer().ReplayPGConversationEvents(ctx, s.authInput(scope), stream, id, cursor, through)
	return r0, hubapi.FromFeatureError(r1)
}

const conversationPGWorkerPageSize = conversations.ConversationPGWorkerPageSize
