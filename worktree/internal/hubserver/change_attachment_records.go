package hubserver

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func validateChangeRecordContent(ctx context.Context, r tenantstore.Reader, scope nativeScope, fields []cloudContentField, artifacts []tracker.ChangeArtifact, locked bool) ([]string, error) {
	project, err := r.ProjectRecord(ctx, scope.project)
	if err != nil {
		return nil, err
	}
	if !project.Settings.RequireVisualAttachments || scope.importedContent {
		return nil, nil
	}
	var ids []string
	for _, field := range fields {
		refs, err := cloudContentAttachmentReferences(field.value, scope)
		if err != nil {
			return nil, err
		}
		for _, ref := range refs {
			ids = append(ids, ref.ID)
		}
	}
	for _, artifact := range artifacts {
		if ref, ok := attachment.URIReference(artifact.URI, string(scope.organization), string(scope.project)); ok {
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
				return nil, workItemStorageUnavailable()
			}
			row, err = locker.LockAttachment(ctx, scope.project, id)
		} else {
			row, err = r.Attachment(ctx, scope.project, id)
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
		err := attachment.ValidateVisualReferences(field.value, string(scope.organization), string(scope.project), resolve)
		var refusal *attachment.VisualReferenceError
		if errors.As(err, &refusal) {
			return nil, nativePublicInvalid(fmt.Sprintf("%s block %d: %s", field.name, refusal.Block, attachment.VisualReferenceMessage))
		}
		if err != nil {
			return nil, err
		}
	}
	err = changerequest.ValidateVisualArtifacts(artifacts, string(scope.organization), string(scope.project), resolve)
	var refusal *attachment.VisualReferenceError
	if errors.As(err, &refusal) {
		return nil, nativePublicInvalid(fmt.Sprintf("artifacts[%d]: %s", refusal.Block-1, attachment.VisualReferenceMessage))
	}
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if metadata[id].ID != id {
			return nil, nativePublicInvalid(attachment.VisualReferenceMessage)
		}
	}
	return ids, nil
}

func bindChangeRecordContent(ctx context.Context, w tenantstore.Writer, scope nativeScope, item string, fields []cloudContentField, artifacts []tracker.ChangeArtifact) error {
	ids, err := validateChangeRecordContent(ctx, w, scope, fields, artifacts, true)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := w.BindAttachment(ctx, attachmentstore.Reference{ProjectID: scope.project, AttachmentID: id, WorkItemID: tracker.NativeWorkItemID(item), RetainWithWorkItem: true}); err != nil {
			return err
		}
	}
	return nil
}
