package artifactstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ArchiveObject struct {
	OrganizationID tracker.OrganizationID
	ID             string
	Reference      attachment.ArchiveReference
	CreatedAt      time.Time
}

type ArchiveOwner struct {
	OrganizationID tracker.OrganizationID
	ArchiveID      string
	Kind           string
	ID             string
	CreatedAt      time.Time
}

type Reader interface {
	ArtifactReference(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string, int64) (artifact.Reference, error)
	ArtifactReferences(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int) ([]artifact.Reference, error)
	ArchiveObject(context.Context, string) (ArchiveObject, error)
	ArchiveBytes(context.Context) (int64, error)
}

type Writer interface {
	Reader
	AppendArtifactReference(context.Context, artifact.Reference) (artifact.Reference, error)
	RegisterArchiveObject(context.Context, ArchiveObject) (ArchiveObject, error)
	AddArchiveOwner(context.Context, ArchiveOwner) error
	RemoveArchiveOwner(context.Context, ArchiveOwner) error
}
