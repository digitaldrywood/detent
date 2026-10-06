package operatortool

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
)

const (
	UploadAttachment       = "upload_attachment"
	ReadAttachmentMetadata = "read_attachment_metadata"
	ReadAttachment         = "read_attachment"
	ReferenceAttachment    = "reference_attachment"
	DeleteAttachment       = "delete_attachment"
	AttachmentContentBytes = 32768
)

type AttachmentOperationArguments struct {
	ProjectID    string `json:"project_id"`
	AttachmentID string `json:"attachment_id"`
	RequestID    string `json:"request_id,omitempty"`
	WorkItemID   string `json:"work_item_id,omitempty"`
	CommentID    string `json:"comment_id,omitempty"`
	Offset       int64  `json:"offset,omitempty"`
	Length       int    `json:"length,omitempty"`
}

type AttachmentContentResult struct {
	Metadata        attachment.Metadata `json:"metadata"`
	ContentBase64   string              `json:"content_base64"`
	Offset          int64               `json:"offset"`
	ReturnedBytes   int                 `json:"returned_bytes"`
	EOF             bool                `json:"eof"`
	MaxContentBytes int                 `json:"max_content_bytes"`
}

func IsAttachmentTool(name string) bool {
	switch name {
	case UploadAttachment, ReadAttachmentMetadata, ReadAttachment, ReferenceAttachment, DeleteAttachment:
		return true
	default:
		return false
	}
}

func DecodeAttachmentOperation(name string, raw json.RawMessage) (AttachmentOperationArguments, error) {
	var input AttachmentOperationArguments
	if DecodeArguments(raw, &input) != nil || !strings.HasPrefix(input.ProjectID, "prj_") || len(input.ProjectID) > 256 || strings.ContainsAny(input.ProjectID, "/\\\r\n") || conversation.ValidateAttachmentID(input.AttachmentID) != nil {
		return input, ErrInvalidArguments
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return input, ErrInvalidArguments
	}
	for field := range fields {
		allowed := field == "project_id" || field == "attachment_id"
		switch name {
		case ReadAttachment:
			allowed = allowed || field == "offset" || field == "length"
		case ReferenceAttachment:
			allowed = allowed || field == "request_id" || field == "work_item_id" || field == "comment_id"
		case DeleteAttachment:
			allowed = allowed || field == "request_id"
		case ReadAttachmentMetadata:
		default:
			return input, ErrInvalidArguments
		}
		if !allowed {
			return input, ErrInvalidArguments
		}
	}
	if name == ReadAttachment {
		if input.Offset < 0 || input.Offset > attachment.MaxBytes || input.Length < 0 || input.Length > AttachmentContentBytes {
			return input, ErrInvalidArguments
		}
		if input.Length == 0 {
			if _, present := fields["length"]; present {
				return input, ErrInvalidArguments
			}
			input.Length = AttachmentContentBytes
		}
	}
	if name == DeleteAttachment || name == ReferenceAttachment {
		if strings.TrimSpace(input.RequestID) == "" || len(input.RequestID) > 128 {
			return input, ErrInvalidArguments
		}
	}
	if name == ReferenceAttachment && (!strings.HasPrefix(input.WorkItemID, "wi_") || len(input.WorkItemID) > 256 || len(input.CommentID) > 256 || strings.ContainsAny(input.WorkItemID+input.CommentID, "/\\\r\n")) {
		return input, ErrInvalidArguments
	}
	return input, nil
}

type AttachmentArguments struct {
	RequestID     string `json:"request_id"`
	ProjectID     string `json:"project_id"`
	Name          string `json:"name"`
	ContentType   string `json:"content_type,omitempty"`
	ContentBase64 string `json:"content_base64"`
}

func uploadAttachmentCatalog() []Definition {
	return []Definition{{Name: UploadAttachment, Description: "Upload a Cloud issue attachment from base64 content. Reuse request_id only for identical upload retries. Returns id and reference; embed reference in file_issue description (create_issue in the API) or add_comment body. Larger files use detent attach or the upload API. Requires project write authority.", InputSchema: json.RawMessage(`{"type":"object","required":["project_id","name","content_base64","request_id"],"properties":{"request_id":{"type":"string","minLength":1,"maxLength":128},"project_id":{"type":"string","minLength":1,"maxLength":256},"name":{"type":"string","minLength":1,"maxLength":256},"content_type":{"type":"string","maxLength":128},"content_base64":{"type":"string","minLength":1,"maxLength":60000}},"additionalProperties":false}`), Annotations: Annotations{Idempotent: true, OpenWorld: true}, Meta: ToolMetadata{Toolset: "work"}}}
}

func AttachmentCatalog() []Definition {
	definitions := uploadAttachmentCatalog()
	selector := `"project_id":{"type":"string","minLength":1,"maxLength":256},"attachment_id":{"type":"string","minLength":1,"maxLength":256}`
	for _, operation := range []struct {
		name, description, properties, required string
		readOnly, destructive                   bool
	}{
		{ReadAttachmentMetadata, "Read authenticated Cloud attachment metadata, including type, size, SHA-256, dimensions and item/comment references. No storage URL or credentials. Results are bounded to 256 KiB; oversized metadata is unavailable.", "", "", true, false},
		{ReadAttachment, "Read up to 32768 bytes of authenticated Cloud attachment content as base64 with recorded metadata, offset, returned_bytes and eof. Offset is bounded by the recorded file size (at most 20 MiB). No storage URL or credentials.", `,"offset":{"type":"integer","minimum":0,"maximum":20971520},"length":{"type":"integer","minimum":1,"maximum":32768}`, "", true, false},
		{ReferenceAttachment, "Bind a Cloud attachment to an authorized work item and optional comment in the same project. Repeating a binding does not duplicate references. Requires project write authority.", `,"request_id":{"type":"string","minLength":1,"maxLength":128},"work_item_id":{"type":"string","minLength":1,"maxLength":256},"comment_id":{"type":"string","minLength":1,"maxLength":256}`, `,"request_id","work_item_id"`, false, false},
		{DeleteAttachment, "Explicitly delete a Cloud attachment using current write authority. Hides metadata before entry deletes bytes; quota remains charged until object deletion is confirmed. Poll action_result for the receipt and object_deletion state.", `,"request_id":{"type":"string","minLength":1,"maxLength":128}`, `,"request_id"`, false, true},
	} {
		definitions = append(definitions, Definition{Name: operation.name, Description: operation.description, InputSchema: json.RawMessage(`{"type":"object","required":["project_id","attachment_id"` + operation.required + `],"properties":{` + selector + operation.properties + `},"additionalProperties":false}`), Annotations: Annotations{ReadOnly: operation.readOnly, Destructive: operation.destructive, Idempotent: true, OpenWorld: true}, Meta: ToolMetadata{Toolset: "work"}})
	}
	return definitions
}

func DecodeAttachmentArguments(raw json.RawMessage) (AttachmentArguments, []byte, error) {
	var input AttachmentArguments
	if DecodeArguments(raw, &input) != nil || strings.TrimSpace(input.RequestID) == "" || len(input.RequestID) > 128 || strings.ContainsAny(input.RequestID, "\r\n") || !strings.HasPrefix(input.ProjectID, "prj_") || len(input.ProjectID) > 256 || len(input.Name) > 256 || len(input.ContentType) > 128 || strings.ContainsAny(input.ProjectID+input.ContentType, "\r\n") || len(input.ContentBase64) > 60000 {
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
