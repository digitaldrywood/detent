package hubclient

import (
	"bytes"
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) PublishValidationEvidence(ctx context.Context, files []runner.ValidationEvidence) error {
	if len(files) == 0 {
		return nil
	}
	if err := e.Validate(ctx); err != nil {
		return err
	}
	return e.claim.source.client.PublishValidationEvidence(ctx, e.claim.lease.WorkItemID, tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":validation-evidence", LeaseID: e.claim.lease.ID, FencingToken: e.claim.lease.FencingToken}, files)
}

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
