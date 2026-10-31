package hubclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

func (c *NativeClient) UploadAttachment(ctx context.Context, reader io.Reader, name, contentType string) (attachment.Metadata, error) {
	return c.uploadAttachment(ctx, reader, name, contentType, nil, "/attachments")
}

func (c *NativeClient) uploadAttachment(ctx context.Context, reader io.Reader, name, contentType string, headers http.Header, suffix string) (attachment.Metadata, error) {
	var result attachment.Metadata
	content, err := io.ReadAll(io.LimitReader(reader, attachment.MaxBytes+1))
	if err != nil {
		return result, err
	}
	if len(content) > attachment.MaxBytes {
		return result, attachment.ErrTooLarge
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(name))
		if contentType == "" {
			contentType = http.DetectContentType(content)
		}
	}
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", contentType)
	headers.Set("X-Attachment-Name", filepath.Base(name))
	path := c.attachmentPath(suffix)
	err = c.client.requestWithHeaders(ctx, http.MethodPost, path, headers, bytes.NewReader(content), &result)
	if err != nil {
		return result, err
	}
	if result.Validate() != nil || result.ProjectID != string(c.project) || result.Reference != result.Markdown(string(c.organization)) {
		return attachment.Metadata{}, errors.New("invalid attachment upload response")
	}
	return result, nil
}

func (c *NativeClient) attachmentPath(suffix string) string {
	if c.client.baseURL.Path == "/organizations/"+string(c.organization) {
		return "/api/v2/projects/" + string(c.project) + suffix
	}
	return c.base() + suffix
}

func (c *NativeClient) readAgentAttachment(ctx context.Context, name string, input operatortool.AttachmentOperationArguments) (any, error) {
	path := c.attachmentPath("/attachments/" + url.PathEscape(input.AttachmentID))
	if name == operatortool.ReadAttachmentMetadata {
		path += "/metadata"
	} else {
		params := url.Values{"offset": {strconv.FormatInt(input.Offset, 10)}, "length": {strconv.Itoa(input.Length)}}
		path += "?" + params.Encode()
	}
	raw, _, err := c.client.download(ctx, path, operatortool.MaxResultBytes)
	if err != nil {
		return nil, err
	}
	var metadata attachment.Metadata
	var result operatortool.AttachmentContentResult
	if name == operatortool.ReadAttachmentMetadata {
		if json.Unmarshal(raw, &metadata) != nil {
			return nil, operatortool.ErrReadUnavailable
		}
	} else {
		if json.Unmarshal(raw, &result) != nil {
			return nil, operatortool.ErrReadUnavailable
		}
		metadata = result.Metadata
		content, err := base64.StdEncoding.Strict().DecodeString(result.ContentBase64)
		if err != nil || result.Offset != input.Offset || input.Offset > metadata.Size || result.ReturnedBytes != len(content) || result.ReturnedBytes != int(min(int64(input.Length), metadata.Size-input.Offset)) || result.MaxContentBytes != operatortool.AttachmentContentBytes || result.EOF != (input.Offset+int64(len(content)) == metadata.Size) {
			return nil, operatortool.ErrReadUnavailable
		}
	}
	if metadata.Validate() != nil || metadata.ID != input.AttachmentID || metadata.ProjectID != input.ProjectID || metadata.DeletedAt != nil || metadata.AuthorizedPrincipal != "" {
		return nil, operatortool.ErrReadUnavailable
	}
	if name == operatortool.ReadAttachmentMetadata {
		return metadata, nil
	}
	return result, nil
}

func (c *NativeClient) UploadAttachmentFile(ctx context.Context, path, name, contentType string) (result attachment.Metadata, resultErr error) {
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, attachment.ErrInvalid
	}
	if info.Size() > attachment.MaxBytes {
		return result, attachment.ErrTooLarge
	}
	if name == "" {
		name = filepath.Base(path)
	}
	return c.UploadAttachment(ctx, file, name, contentType)
}
