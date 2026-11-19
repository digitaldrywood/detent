package hubserver

import "github.com/digitaldrywood/detent/internal/hubserver/conversations"

const attachmentMetadataBase = conversations.AttachmentMetadataBase

type cloudContentField struct {
	name  string
	value string
}

var cloudContentAttachmentReferences = conversations.CloudContentAttachmentReferences
