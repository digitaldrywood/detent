package conversationstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type WorkItemReference struct {
	ConversationID   string
	MessageID        string
	CreatedAt        time.Time
	Visibility       conversation.Visibility
	OwnerPrincipalID string
	ReadAudience     string
}

type WorkItemReferenceReader interface {
	WorkItemReferences(context.Context, tracker.ProjectID, string, int) ([]WorkItemReference, error)
}

type RemovalWriter interface {
	DeleteConversation(context.Context, tracker.ProjectID, string, time.Time) error
}
