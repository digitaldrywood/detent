package conversationstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type AttachmentReader interface {
	ExpiredConversationAttachments(context.Context, time.Time, int) ([]Attachment, error)
	MessageAttachments(context.Context, tracker.ProjectID, string, []string) (map[string][]Attachment, error)
	MessageSequences(context.Context, string, []string) (map[string]int64, error)
	AttachmentConversation(context.Context, tracker.ProjectID, string) (Conversation, error)
	ConversationAttachment(context.Context, tracker.ProjectID, string, string) (Attachment, error)
	ConversationAttachmentByRef(context.Context, tracker.ProjectID, string, string) (Attachment, error)
}

type AttachmentWriter interface {
	BindConversationAttachment(context.Context, tracker.ProjectID, string, string, string, string, time.Time) error
	LockAttachmentConversation(context.Context, tracker.ProjectID, string) (Conversation, error)
	AttachmentReader
	CreateConversationAttachment(context.Context, Attachment, ContentObject) error
	DeleteConversationAttachment(context.Context, tracker.ProjectID, string, string, time.Time) error
}

type AttachmentMaintenanceWriter interface {
	LockRetainedAttachmentConversation(context.Context, tracker.ProjectID, string) (Conversation, error)
}
