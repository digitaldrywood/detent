package hubserver

import (
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubconversation"
)

type conversationQueryMetadata = hubconversation.QueryMetadata

type queryMessageData = hubconversation.QueryMessageData

func executionQueryMetadata(execution conversation.Execution) conversationQueryMetadata {
	return hubconversation.ExecutionQueryMetadata(execution)
}

func messageQueryMetadata(raw []byte, actor conversation.ActorKind) (conversationQueryMetadata, error) {
	return hubconversation.MessageQueryMetadata(raw, actor)
}

func eventQueryMetadata(raw []byte) (conversationQueryMetadata, error) {
	return hubconversation.EventQueryMetadata(raw)
}

func activityQueryMetadata(raw []byte) (conversationQueryMetadata, error) {
	return hubconversation.ActivityQueryMetadata(raw)
}
