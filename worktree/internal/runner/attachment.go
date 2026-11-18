package runner

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/runner/prompt"
)

// AgentAttachment is one file a user attached to the message that starts or
// steers a turn, with its bytes already fetched.
type AgentAttachment struct {
	ID      string
	Name    string
	MIME    string
	Size    int64
	Content []byte
}

// Image reports whether the attachment is provider image input rather than a
// text data block.
func (a AgentAttachment) Image() bool { return strings.HasPrefix(a.MIME, "image/") }

// AttachmentImages returns the attachments the provider takes as image
// input. An image with no bytes is dropped: the download failed and an empty
// image is worse than none.
func AttachmentImages(attachments []AgentAttachment) []AgentAttachment {
	images := make([]AgentAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Image() && len(attachment.Content) > 0 {
			images = append(images, attachment)
		}
	}
	if len(images) == 0 {
		return nil
	}
	return images
}

// AttachmentDataBlock renders the text attachments as one delimited data
// block, for a transport that builds a steer's input itself.
func AttachmentDataBlock(attachments []AgentAttachment) string {
	return attachmentDataBlock(attachments)
}

// attachmentDataBlock renders the text attachments as one delimited data
// block. Attachments with no text content produce no block at all.
func attachmentDataBlock(attachments []AgentAttachment) string {
	text := make([]prompt.TextAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		if !attachment.Image() && len(attachment.Content) > 0 {
			text = append(text, prompt.TextAttachment{Name: attachment.Name, MIME: attachment.MIME, Content: attachment.Content})
		}
	}
	return prompt.AttachmentDataBlock(text)
}
