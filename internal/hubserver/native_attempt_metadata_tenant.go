package hubserver

import (
	"context"
	"path"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"

	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type attemptMetadataSnapshotReader struct {
	tenantstore.Reader
	metadata     tenantstore.AttemptMetadata
	organization tracker.OrganizationID
}

func readAttemptMetadataSnapshot(ctx context.Context, r tenantstore.Reader, scope nativeScope, item string, attempts []leasestore.Attempt, evidence bool) (tenantstore.Reader, error) {
	reader, ok := r.(tenantstore.AttemptMetadataReader)
	if !ok {
		return nil, nativeRunEventStorageUnavailable()
	}
	metadata, err := reader.AttemptMetadata(ctx, scope.project, tracker.NativeWorkItemID(item), attempts, evidence)
	if err != nil {
		return nil, err
	}
	return attemptMetadataSnapshotReader{Reader: r, metadata: metadata, organization: scope.organization}, nil
}

func (r attemptMetadataSnapshotReader) Lease(_ context.Context, id string) (leasestore.Lease, error) {
	value, ok := r.metadata.Leases[id]
	if !ok {
		return value, corestore.ErrNotFound
	}
	return value, nil
}

func (r attemptMetadataSnapshotReader) LatestRunEvent(_ context.Context, _ tracker.ProjectID, id string) (eventstore.Event, error) {
	value, ok := r.metadata.Events[id]
	if !ok {
		return value, corestore.ErrNotFound
	}
	return value, nil
}

func (r attemptMetadataSnapshotReader) ArchiveObjectByKey(_ context.Context, key string) (artifactstore.ArchiveObject, error) {
	expected, err := attachment.ArchiveKey(string(r.organization), path.Base(key))
	if err != nil || expected != key {
		return artifactstore.ArchiveObject{}, artifact.ErrIntegrity
	}
	value, ok := r.metadata.Archives[key]
	if !ok {
		return value, corestore.ErrNotFound
	}
	return value, nil
}

func (r attemptMetadataSnapshotReader) Attachment(_ context.Context, _ tracker.ProjectID, id string) (attachmentstore.Record, error) {
	value, ok := r.metadata.Attachments[id]
	if !ok {
		return value, corestore.ErrNotFound
	}
	return value, nil
}

func (r attemptMetadataSnapshotReader) AttachmentEvidence(_ context.Context, _ tracker.ProjectID, _ tracker.NativeWorkItemID, id string, _ int) ([]attachmentstore.Record, error) {
	return r.metadata.Evidence[id], nil
}
