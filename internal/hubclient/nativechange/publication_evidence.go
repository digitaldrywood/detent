package nativechange

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (e *nativeExecution) publicationEvidence(ctx context.Context) ([]tracker.ChangeArtifact, error) {
	client := e.claim.source.NativeClient()
	attempt, err := client.NativeAttempt(ctx, e.claim.lease.WorkItemID, e.data.AttemptID)
	if err != nil {
		return nil, fmt.Errorf("read publication attempt: %w", err)
	}
	if attempt.AttemptID != e.data.AttemptID || attempt.LeaseID != e.claim.lease.ID || attempt.FencingToken != e.claim.lease.FencingToken {
		return nil, errors.New("publication evidence attempt authority mismatch")
	}
	artifacts := []tracker.ChangeArtifact{}
	for _, evidence := range attempt.Evidence {
		ref, ok := attachment.URIReference("attachment:"+evidence.AttachmentID, string(client.OrganizationID()), string(client.ProjectID()))
		if !ok {
			return nil, attachment.ErrInvalid
		}
		var metadata attachment.Metadata
		if err := client.Transport().Request(ctx, http.MethodGet, client.AttachmentPath("/attachments/"+ref.ID+"/metadata"), nil, &metadata); err != nil {
			return nil, fmt.Errorf("read publication attachment: %w", err)
		}
		if metadata.Validate() != nil || metadata.ID != ref.ID || metadata.ProjectID != string(client.ProjectID()) || metadata.DeletedAt != nil || metadata.AuthorizedPrincipal != "" {
			return nil, attachment.ErrInvalid
		}
		kind := "log"
		if metadata.ContentType == "image/png" || metadata.ContentType == "image/jpeg" || metadata.ContentType == "application/pdf" {
			kind = "artifact"
		}
		artifact := tracker.ChangeArtifact{Kind: kind, URI: "attachment:" + ref.ID, SHA256: metadata.SHA256, Availability: "available"}
		if !slices.Contains(artifacts, artifact) {
			artifacts = append(artifacts, artifact)
		}
	}
	return artifacts, nil
}

func mergePublicationArtifacts(previous, supplied []tracker.ChangeArtifact) []tracker.ChangeArtifact {
	result := append([]tracker.ChangeArtifact{}, previous...)
	for _, artifact := range supplied {
		if !slices.Contains(result, artifact) {
			result = append(result, artifact)
		}
	}
	return result
}
