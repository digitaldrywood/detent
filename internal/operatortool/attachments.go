package operatortool

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/conversation"
)

const UploadAttachment = "upload_attachment"

type AttachmentArguments struct {
	ProjectID     string `json:"project_id"`
	Name          string `json:"name"`
	ContentType   string `json:"content_type,omitempty"`
	ContentBase64 string `json:"content_base64"`
}

func AttachmentCatalog() []Definition {
	return []Definition{{Name: UploadAttachment, Description: "Upload a Cloud issue attachment from base64 content. Returns id and reference; embed reference in file_issue description (create_issue in the API) or add_comment body. Larger files use detent attach or the upload API. Requires project write authority.", InputSchema: json.RawMessage(`{"type":"object","required":["project_id","name","content_base64"],"properties":{"project_id":{"type":"string","minLength":1,"maxLength":256},"name":{"type":"string","minLength":1,"maxLength":256},"content_type":{"type":"string","maxLength":128},"content_base64":{"type":"string","minLength":1,"maxLength":60000}},"additionalProperties":false}`), Annotations: Annotations{OpenWorld: true}, Meta: ToolMetadata{Toolset: "work"}}}
}

func DecodeAttachmentArguments(raw json.RawMessage) (AttachmentArguments, []byte, error) {
	var input AttachmentArguments
	if DecodeArguments(raw, &input) != nil || !strings.HasPrefix(input.ProjectID, "prj_") || len(input.ProjectID) > 256 || len(input.Name) > 256 || len(input.ContentType) > 128 || strings.ContainsAny(input.ProjectID+input.ContentType, "\r\n") || len(input.ContentBase64) > 60000 {
		return input, nil, ErrInvalidArguments
	}
	if _, err := conversation.SanitizeAttachmentName(input.Name); err != nil {
		return input, nil, ErrInvalidArguments
	}
	content, err := base64.StdEncoding.Strict().DecodeString(input.ContentBase64)
	if err != nil || len(content) == 0 {
		return input, nil, ErrInvalidArguments
	}
	return input, content, nil
}
