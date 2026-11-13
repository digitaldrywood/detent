package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/conversations"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type conversationContentsKey = conversations.ConversationContentsKey
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
type conversationScheduling = conversations.ConversationScheduling
type conversationStore = conversations.ConversationStore
type conversationStream = conversations.ConversationStream
type conversationWorkItemReferencePage = conversations.ConversationWorkItemReferencePage
type conversationWorkItemResource = conversations.ConversationWorkItemResource
type errConversationLinked = conversations.ErrConversationLinked
type errConversationRevision = conversations.ErrConversationRevision

const (
	conversationDefaultTitle          = conversations.ConversationDefaultTitle
	conversationDispatchLater         = conversations.ConversationDispatchLater
	conversationDispatchNow           = conversations.ConversationDispatchNow
	conversationReferenceExcerptRunes = conversations.ConversationReferenceExcerptRunes
	conversationSnapshotQuestions     = conversations.ConversationSnapshotQuestions
)

func conversationAlreadyLinked(p0 string) error {
	return fromFeatureError(conversations.ConversationAlreadyLinked(p0))
}

func conversationArchiveCache(p0 context.Context) *conversations.ConversationArchiveReads {
	return conversations.ConversationArchiveCache(p0)
}

func conversationAttachmentURL(p0 tracker.OrganizationID, p1 tracker.ProjectID, p2 string, p3 string) string {
	return conversations.ConversationAttachmentURL(p0, p1, p2, p3)
}

func conversationIssueResult(p0 conversations.ConversationRecord) map[string]any {
	return conversations.ConversationIssueResult(p0)
}

func conversationModelFromReport(p0 providercapacity.Report, p1 string) conversations.ConversationModel {
	return conversations.ConversationModelFromReport(p0, p1)
}

func conversationQueueFull() error { return fromFeatureError(conversations.ConversationQueueFull()) }

func conversationStaleOwner(p0 string, p1 string, p2 string) error {
	return fromFeatureError(conversations.ConversationStaleOwner(p0, p1, p2))
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
	return r0, fromFeatureError(r1)
}
