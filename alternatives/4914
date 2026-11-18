package runner

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

type validatorAttachmentResult struct {
	Metadata        attachment.Metadata  `json:"metadata"`
	ReadReceipt     *gate.AttachmentRead `json:"read_receipt,omitempty"`
	Content         string               `json:"content,omitempty"`
	Truncated       bool                 `json:"truncated,omitempty"`
	InspectionError string               `json:"inspection_error,omitempty"`
}

func validatorEvidenceTools(tools []AgentTool) []AgentTool {
	var result []AgentTool
	for _, tool := range tools {
		switch tool.Name {
		case operatortool.WorkItem, operatortool.WorkComments, operatortool.ReadAttachmentMetadata, operatortool.ReadAttachment:
			if tool.Name == operatortool.ReadAttachment {
				tool.Description += " During native acceptance validation, reading an image or text/plain attachment from offset 0 verifies its complete digest. Only complete content displayed in this review returns a host read_receipt bound to the reviewed head and tree. Unsupported MIME or truncated text returns inspection_error and no read_receipt. Images require a backend that can display them."
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
	var first operatortool.AttachmentContentResult
	if err := json.Unmarshal([]byte(result.Content), &first); err != nil {
		return AgentToolResult{}, err
	}
	if err := first.Metadata.Validate(); err != nil || first.Metadata.ID != input.AttachmentID || first.Metadata.ProjectID != input.ProjectID {
		return AgentToolResult{}, errors.New("invalid attachment metadata")
	}
	switch first.Metadata.ContentType {
	case "image/png", "image/jpeg":
		if !v.images {
			return result, nil
		}
	case "text/plain":
	default:
		data, err := json.Marshal(validatorAttachmentResult{
			Metadata:        first.Metadata,
			InspectionError: fmt.Sprintf("MIME %s is unsupported for acceptance inspection; content was not displayed or digest-verified and no read_receipt was issued. ", first.Metadata.ContentType) + operatortool.AttachmentEvidencePublication,
		})
		return AgentToolResult{Success: err == nil, Content: string(data)}, err
	}
	if input.Offset != 0 {
		return result, nil
	}
	limit := first.Metadata.Size
	if first.Metadata.ContentType == "text/plain" {
		limit = min(limit, operatortool.AttachmentContentBytes)
	}
	content := make([]byte, 0, limit)
	digest := sha256.New()
	var readBytes int64
	part := first
	for {
		chunk, err := base64.StdEncoding.Strict().DecodeString(part.ContentBase64)
		if err != nil || part.Metadata.ID != first.Metadata.ID || part.Metadata.ProjectID != first.Metadata.ProjectID || part.Metadata.ContentType != first.Metadata.ContentType || part.Metadata.SHA256 != first.Metadata.SHA256 || part.Metadata.Size != first.Metadata.Size || part.Offset != readBytes || len(chunk) == 0 || len(chunk) != part.ReturnedBytes || len(chunk) > input.Length || readBytes+int64(len(chunk)) > first.Metadata.Size {
			return AgentToolResult{}, errors.New("attachment content differs from its metadata")
		}
		if _, err := digest.Write(chunk); err != nil {
			return AgentToolResult{}, err
		}
		readBytes += int64(len(chunk))
		content = append(content, chunk[:min(int64(len(chunk)), limit-int64(len(content)))]...)
		if part.EOF {
			break
		}
		input.Offset = readBytes
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
	if readBytes != first.Metadata.Size || hex.EncodeToString(digest.Sum(nil)) != first.Metadata.SHA256 {
		return AgentToolResult{}, errors.New("attachment digest does not match")
	}
	truncated := readBytes > int64(len(content))
	var text, imageURL string
	if first.Metadata.ContentType == "text/plain" {
		if truncated {
			end := len(content)
			for end > 0 && !utf8.RuneStart(content[end-1]) {
				end--
			}
			if end > 0 && !utf8.FullRune(content[end-1:]) {
				content = content[:end-1]
			}
		}
		if !utf8.Valid(content) {
			return AgentToolResult{}, errors.New("attachment text is not valid UTF-8")
		}
		text = string(content)
	} else {
		imageURL = "data:" + first.Metadata.ContentType + ";base64," + base64.StdEncoding.EncodeToString(content)
	}
	read := &gate.AttachmentRead{
		ReceiptID:    "attachment:" + first.Metadata.ID + ":" + first.Metadata.SHA256,
		AttachmentID: first.Metadata.ID, SHA256: first.Metadata.SHA256,
		ProjectID: first.Metadata.ProjectID, WorkItemID: first.Metadata.WorkItemID, CommentID: first.Metadata.CommentID,
		HeadSHA: v.head, TreeSHA: v.tree,
	}
	var inspectionError string
	if truncated {
		read = nil
		inspectionError = "Text exceeds the 32768-byte inspection bound; the complete digest was verified but only a preview was displayed, so no read_receipt was issued. " + operatortool.AttachmentEvidencePublication
	}
	data, err := json.Marshal(validatorAttachmentResult{Metadata: first.Metadata, ReadReceipt: read, Content: text, Truncated: truncated, InspectionError: inspectionError})
	if err != nil {
		return AgentToolResult{}, err
	}
	v.mu.Lock()
	if read != nil && !slices.Contains(v.reads, *read) {
		v.reads = append(v.reads, *read)
	}
	v.mu.Unlock()
	return AgentToolResult{Success: true, Content: string(data), ImageURL: imageURL}, nil
}

func (v *validatorAttachments) evidence() []gate.AttachmentRead {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.reads)
}
