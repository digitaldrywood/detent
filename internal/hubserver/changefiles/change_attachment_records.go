package changefiles

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/digitaldrywood/detent/internal/gate"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ContentField struct {
	Name  string
	Value string
}
type AttachmentReferences func(string, hubscope.NativeScope) ([]attachment.Reference, error)

func ValidateRecordContent(ctx context.Context, r tenantstore.Reader, scope hubscope.NativeScope, fields []ContentField, artifacts []tracker.ChangeArtifact, artifactName string, locked bool, references AttachmentReferences) ([]string, error) {
	project, err := r.ProjectMetadata(ctx, scope.Project)
	if err != nil {
		return nil, err
	}
	if !project.Settings.RequireVisualAttachments || scope.ImportedContent {
		return nil, nil
	}
	var ids []string
	for _, field := range fields {
		refs, err := references(field.Value, scope)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			ids = append(ids, ref.ID)
		}
	}
	for _, artifact := range artifacts {
		if ref, ok := attachment.URIReference(artifact.URI, string(scope.Organization), string(scope.Project)); ok {
			ids = append(ids, ref.ID)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	metadata := make(map[string]attachment.Metadata, len(ids))
	for _, id := range ids {
		var row attachmentstore.Record
		if locked {
			locker, ok := r.(attachmentstore.LockingReader)
			if !ok {
				return nil, hubapi.NativeWorkItemStorageUnavailable()
			}
			row, err = locker.LockAttachment(ctx, scope.Project, id)
		} else {
			row, err = r.Attachment(ctx, scope.Project, id)
		}
		if errors.Is(err, corestore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if row.Kind == "attachment" && row.Metadata.DeletedAt == nil && row.ObjectDeletedAt == nil && row.PendingContent == nil {
			metadata[id] = row.Metadata
		}
	}
	resolve := func(id string) (attachment.Metadata, error) { return metadata[id], nil }
	for _, field := range fields {
		err := attachment.ValidateVisualReferences(field.Value, string(scope.Organization), string(scope.Project), resolve)
		var refusal *attachment.VisualReferenceError
		if errors.As(err, &refusal) {
			return nil, hubapi.NativePublicInvalid(fmt.Sprintf("%s block %d: %s", field.Name, refusal.Block, attachment.VisualReferenceMessage))
		}
		if err != nil {
			return nil, err
		}
	}
	err = changerequest.ValidateVisualArtifacts(artifacts, string(scope.Organization), string(scope.Project), resolve)
	var refusal *attachment.VisualReferenceError
	if errors.As(err, &refusal) {
		return nil, hubapi.NativePublicInvalid(fmt.Sprintf("%s[%d]: %s", artifactName, refusal.Block-1, attachment.VisualReferenceMessage))
	}
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if metadata[id].ID != id {
			return nil, hubapi.NativePublicInvalid(attachment.VisualReferenceMessage)
		}
	}
	return ids, nil
}

func BindRecordContent(ctx context.Context, w tenantstore.Writer, scope hubscope.NativeScope, item string, fields []ContentField, artifacts []tracker.ChangeArtifact, artifactName string, references AttachmentReferences) error {
	ids, err := ValidateRecordContent(ctx, w, scope, fields, artifacts, artifactName, true, references)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.Project, AttachmentID: id, WorkItemID: tracker.NativeWorkItemID(item), RetainWithWorkItem: true}); err != nil {
			return err
		}
	}
	return nil
}

func ReviewContentFields(request tracker.ReviewChange) []ContentField {
	fields := []ContentField{{Name: "body", Value: request.Body}}
	for _, result := range []struct {
		name  string
		value *gate.ValidatorResult
	}{{"validator", request.Validator}, {"implementation_review", request.ImplementationReview}} {
		if result.value == nil {
			continue
		}
		for i, evidence := range result.value.CriteriaEvidence {
			fields = append(fields, ContentField{Name: fmt.Sprintf("%s.criteria_evidence[%d].reference", result.name, i), Value: evidence.Reference})
		}
	}
	return fields
}
