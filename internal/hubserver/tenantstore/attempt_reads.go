package tenantstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type AttemptMetadata struct {
	Leases      map[string]leasestore.Lease
	Events      map[string]eventstore.Event
	Archives    map[string]artifactstore.ArchiveObject
	Attachments map[string]attachmentstore.Record
	Evidence    map[string][]attachmentstore.Record
}

type AttemptMetadataReader interface {
	AttemptMetadata(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, []leasestore.Attempt, bool) (AttemptMetadata, error)
}
