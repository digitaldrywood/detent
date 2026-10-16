package runner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type validatorAttachments struct {
	mu     sync.Mutex
	reads  []gate.AttachmentRead
	head   string
	tree   string
	images bool
	handle AgentToolHandler
}

func validatorEvidenceTools(tools []AgentTool) []AgentTool {
	var result []AgentTool
	for _, tool := range tools {
		switch tool.Name {
		case operatortool.WorkItem, operatortool.WorkComments, operatortool.ReadAttachmentMetadata, operatortool.ReadAttachment:
			if tool.Name == operatortool.ReadAttachment {
				tool.Description += " During native validation, reading an image or text/plain attachment from offset 0 also displays its complete digest-verified content and returns a host read_receipt for observed attachment evidence. Images require a backend that can display them."
			}
			result = append(result, tool)
		}
	}
	return result
}

func (v *validatorAttachments) call(ctx context.Context, call AgentToolCall) (AgentToolResult, error) {
	result, err := v.handle(ctx, call)
	if err != nil || !result.Success || call.Name != operatortool.ReadAttachment {
		return result, err
	}
	input, err := operatortool.DecodeAttachmentOperation(call.Name, call.Arguments)
	if err != nil {
		return AgentToolResult{}, err
	}
	if input.Offset != 0 {
		return result, nil
	}
	var first operatortool.AttachmentContentResult
	if err := json.Unmarshal([]byte(result.Content), &first); err != nil {
		return AgentToolResult{}, err
	}
	switch first.Metadata.ContentType {
	case "image/png", "image/jpeg":
		if !v.images {
			return result, nil
		}
	case "text/plain":
	default:
		return result, nil
	}
	if err := first.Metadata.Validate(); err != nil || first.Metadata.ID != input.AttachmentID || first.Metadata.ProjectID != input.ProjectID {
		return AgentToolResult{}, errors.New("invalid attachment metadata")
	}
	content := make([]byte, 0, first.Metadata.Size)
	part := first
	for {
		chunk, err := base64.StdEncoding.Strict().DecodeString(part.ContentBase64)
		if err != nil || part.Metadata.ID != first.Metadata.ID || part.Metadata.ProjectID != first.Metadata.ProjectID || part.Metadata.ContentType != first.Metadata.ContentType || part.Metadata.SHA256 != first.Metadata.SHA256 || part.Metadata.Size != first.Metadata.Size || part.Offset != int64(len(content)) || len(chunk) == 0 || len(chunk) != part.ReturnedBytes || len(chunk) > input.Length || int64(len(content)+len(chunk)) > first.Metadata.Size {
			return AgentToolResult{}, errors.New("attachment content differs from its metadata")
		}
		content = append(content, chunk...)
		if part.EOF {
			break
		}
		input.Offset = int64(len(content))
		input.Length = operatortool.AttachmentContentBytes
		arguments, err := json.Marshal(input)
		if err != nil {
			return AgentToolResult{}, err
		}
		next, err := v.handle(ctx, AgentToolCall{Name: call.Name, Arguments: arguments})
		if err != nil || !next.Success {
			return next, err
		}
		if err := json.Unmarshal([]byte(next.Content), &part); err != nil {
			return AgentToolResult{}, err
		}
	}
	digest := sha256.Sum256(content)
	if int64(len(content)) != first.Metadata.Size || hex.EncodeToString(digest[:]) != first.Metadata.SHA256 {
		return AgentToolResult{}, errors.New("attachment digest does not match")
	}
	var text, imageURL string
	if first.Metadata.ContentType == "text/plain" {
		if !utf8.Valid(content) {
			return AgentToolResult{}, errors.New("attachment text is not valid UTF-8")
		}
		text = string(content)
	} else {
		imageURL = "data:" + first.Metadata.ContentType + ";base64," + base64.StdEncoding.EncodeToString(content)
	}
	read := gate.AttachmentRead{
		ReceiptID:    "attachment:" + first.Metadata.ID + ":" + first.Metadata.SHA256,
		AttachmentID: first.Metadata.ID, SHA256: first.Metadata.SHA256,
		ProjectID: first.Metadata.ProjectID, WorkItemID: first.Metadata.WorkItemID, CommentID: first.Metadata.CommentID,
		HeadSHA: v.head, TreeSHA: v.tree,
	}
	data, err := json.Marshal(struct {
		Metadata    attachment.Metadata `json:"metadata"`
		ReadReceipt gate.AttachmentRead `json:"read_receipt"`
		Content     string              `json:"content,omitempty"`
	}{first.Metadata, read, text})
	if err != nil {
		return AgentToolResult{}, err
	}
	v.mu.Lock()
	if !slices.Contains(v.reads, read) {
		v.reads = append(v.reads, read)
	}
	v.mu.Unlock()
	return AgentToolResult{Success: true, Content: string(data), ImageURL: imageURL}, nil
}

func (v *validatorAttachments) evidence() []gate.AttachmentRead {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.reads)
}
