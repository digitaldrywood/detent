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

type ReadGrant struct {
	ProjectID                                                  tracker.ProjectID
	TokenHash, PrincipalID, ServiceID, ArtifactID, SessionHash string
	Revision                                                   int64
	ExpiresAt                                                  time.Time
}

type Reader interface {
	ArtifactVersionAttemptMatches(context.Context, tracker.ProjectID, string, string) (bool, error)
	ArtifactReadGrant(context.Context, tracker.ProjectID, string) (ReadGrant, error)
	ArtifactService(context.Context, tracker.ProjectID, string) (artifact.Binding, error)
	ArtifactServices(context.Context, tracker.ProjectID) ([]artifact.Binding, error)
	ArtifactReference(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string, int64) (artifact.Reference, error)
	ArtifactReferences(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int) ([]artifact.Reference, error)
	ArchiveObject(context.Context, string) (ArchiveObject, error)
	ArchiveObjectByReference(context.Context, attachment.ArchiveReference) (ArchiveObject, error)
	ArchiveBytes(context.Context) (int64, error)
}

type Writer interface {
	CreateArtifactReadGrant(context.Context, ReadGrant) error
	BindArtifactService(context.Context, tracker.ProjectID, artifact.Binding) error
	Reader
	AppendArtifactReference(context.Context, artifact.Reference) (artifact.Reference, error)
	RegisterArchiveObject(context.Context, ArchiveObject) (ArchiveObject, error)
	AddArchiveOwner(context.Context, ArchiveOwner) error
	RemoveArchiveOwner(context.Context, ArchiveOwner) error
}
