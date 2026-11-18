package api

import (
	"bytes"
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (c *NativeClient) PublishValidationEvidence(ctx context.Context, item tracker.NativeWorkItemID, mutation tracker.Mutation, files []runner.ValidationEvidence) error {
	if len(files) == 0 {
		return nil
	}
	var body strings.Builder
	body.WriteString("## Validation evidence\n\n")
	for _, file := range files {
		upload, err := c.UploadAttachment(ctx, bytes.NewReader(file.Content), file.Name, file.ContentType)
		if err != nil {
			return err
		}
		body.WriteString(upload.Reference)
		body.WriteString("\n\n")
	}
	_, err := c.CreateComment(ctx, item, tracker.CreateComment{Mutation: mutation, Body: body.String()})
	return err
}
