package hubclient

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"github.com/digitaldrywood/detent/internal/attachment"
)

func (c *NativeClient) UploadAttachment(ctx context.Context, reader io.Reader, name, contentType string) (attachment.Metadata, error) {
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
	headers := http.Header{"Content-Type": {contentType}, "X-Attachment-Name": {filepath.Base(name)}}
	path := c.base() + "/attachments"
	if c.client.baseURL.Path == "/organizations/"+string(c.organization) {
		path = "/api/v2/projects/" + string(c.project) + "/attachments"
	}
	err = c.client.requestWithHeaders(ctx, http.MethodPost, path, headers, bytes.NewReader(content), &result)
	if err != nil {
		return result, err
	}
	if result.Validate() != nil || result.ProjectID != string(c.project) || result.Reference != result.Markdown(string(c.organization)) {
		return attachment.Metadata{}, errors.New("invalid attachment upload response")
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
