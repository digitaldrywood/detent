package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Attachments. An upload belongs to the actor that
// made it and to one conversation. It is unsent until a message command binds
// it, and only while it is unsent may its owner delete it or the sweep expire
// it. Reads follow the conversation's own read rule, so an attachment is
// never more visible than the chat it was dropped into.

const (
	// conversationAttachmentRequestBytes bounds the whole multipart body: the
	// file limit plus room for the framing and the idempotency key.
	conversationAttachmentRequestBytes = conversation.MaxAttachmentBytes + (1 << 20)
	// conversationAttachmentFieldBytes bounds a non-file form field.
	conversationAttachmentFieldBytes = 4096
	// conversationAttachmentSweepBatch bounds one abandoned-upload sweep.
	conversationAttachmentSweepBatch = 200
)

// conversationAttachmentRecord is one stored attachment. MessageID is empty
// while the upload is unsent; ExpiresAt is set only then.
type conversationAttachmentRecord struct {
	ID             string
	ContentID      string
	ConversationID string
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	MessageID      string
	PrincipalID    string
	Name           string
	MIME           string
	Size           int64
	ArtifactRef    string
	CreatedAt      time.Time
	ExpiresAt      *time.Time
	DeletedAt      *time.Time
}

// conversationAttachmentURL is the hub path that streams an attachment. It is
// the `url` every attachment resource carries, so the client and the runner
// both read the blob through the conversation's audience.
func conversationAttachmentURL(organization tracker.OrganizationID, project tracker.ProjectID, conversationID, attachmentID string) string {
	return "/api/v2/organizations/" + string(organization) + "/projects/" + string(project) +
		"/conversations/" + conversationID + "/attachments/" + attachmentID
}

func (r conversationAttachmentRecord) resource() conversation.Attachment {
	return conversation.Attachment{
		ID: r.ID, Name: r.Name, MIME: r.MIME, Size: r.Size,
		URL: conversationAttachmentURL(r.OrganizationID, r.ProjectID, r.ConversationID, r.ID),
	}
}

// conversationAttachmentRef is the storage key of an upload. It is derived
// from the conversation, the uploading principal and the idempotency key, so
// a retried upload lands on the row it already created instead of a second
// copy, and the unique constraint on the column is the idempotency record.
func conversationAttachmentRef(conversationID, principalID, key string) string {
	sum := sha256.Sum256([]byte(conversationID + "\x00" + principalID + "\x00" + key))
	return "conv:" + conversationID + ":" + hex.EncodeToString(sum[:])
}

func projectAttachments(records []conversationAttachmentRecord) []conversation.Attachment {
	projected := make([]conversation.Attachment, 0, len(records))
	for _, record := range records {
		projected = append(projected, record.resource())
	}
	return projected
}

// Store.

// Service.

// sweepAttachments deletes abandoned uploads and their bytes, and reports how
// many it removed.
func (c *conversationService) sweepAttachments(ctx context.Context) (int, error) {
	return c.sweepAttachmentsPG(ctx)
}

// HTTP.

type conversationAttachmentUpload struct {
	name    string
	media   string
	key     string
	content []byte
}

func (s *Service) commandDeleteAttachment(ctx context.Context, scope nativeScope, conversationID, id string) (json.RawMessage, error) {
	return s.commandDeleteAttachmentPG(ctx, scope, conversationID, id)
}

func (s *Service) commandUploadAttachment(ctx context.Context, scope nativeScope, id string, upload conversationAttachmentUpload) (json.RawMessage, error) {
	return s.commandUploadAttachmentPG(ctx, scope, id, upload)
}
