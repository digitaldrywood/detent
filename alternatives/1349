package hubclient

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/runner"
)

// Attachment download (decisions section 17.1). The hub names an
// attachment on the control with the path that streams it; the worker
// fetches it with its own token, so the same read rule that governs the
// conversation governs the file. Nothing else about the path is trusted:
// the client only ever follows a path on its own Hub.

const (
	// maxConversationAttachmentBytes matches the hub's per-file limit.
	maxConversationAttachmentBytes = 20 << 20
	// maxConversationAttachmentsPerTurn bounds how many files one turn
	// downloads, across every message it carries.
	maxConversationAttachmentsPerTurn = 10
	// conversationAttachmentTimeout bounds one download.
	conversationAttachmentTimeout = 30 * time.Second
)

// ConversationAttachment is one file the hub named on a control.
type ConversationAttachment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
	URL  string `json:"url"`
}

// DownloadConversationAttachment fetches one attachment's bytes.
func (c *NativeClient) DownloadConversationAttachment(ctx context.Context, path string) ([]byte, error) {
	content, _, err := c.client.download(ctx, path, maxConversationAttachmentBytes)
	return content, err
}

// fetchAttachments downloads what a control named. A file that cannot be
// fetched is left out with a warning rather than failing the turn: the user
// still gets an answer, and the log says what the model did not see.
func (s *conversationSession) fetchAttachments(ctx context.Context, attachments []ConversationAttachment) []runner.AgentAttachment {
	if len(attachments) == 0 {
		return nil
	}
	if len(attachments) > maxConversationAttachmentsPerTurn {
		s.logger.Warn("conversation.attachments_truncated", "named", len(attachments), "limit", maxConversationAttachmentsPerTurn)
		attachments = attachments[:maxConversationAttachmentsPerTurn]
	}
	fetched := make([]runner.AgentAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		download, cancel := context.WithTimeout(ctx, conversationAttachmentTimeout)
		content, err := s.client.DownloadConversationAttachment(download, attachment.URL)
		cancel()
		if err != nil {
			s.logger.Warn("conversation.attachment_unavailable", "attachment", attachment.ID, "name", attachment.Name, "error", err)
			continue
		}
		fetched = append(fetched, runner.AgentAttachment{
			ID: attachment.ID, Name: attachment.Name, MIME: attachment.MIME, Size: attachment.Size, Content: content,
		})
	}
	return fetched
}

// PendingAttachments are the files attached to the messages the hub handed
// over at bind time, already fetched.
func (s *conversationSession) PendingAttachments() []runner.AgentAttachment {
	return append([]runner.AgentAttachment(nil), s.pendingAttachments...)
}
