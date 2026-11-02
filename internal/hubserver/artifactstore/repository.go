package artifactstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader interface {
	ArtifactReference(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string, int64) (artifact.Reference, error)
	ArtifactReferences(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, int) ([]artifact.Reference, error)
}

type Writer interface {
	Reader
	AppendArtifactReference(context.Context, artifact.Reference) (artifact.Reference, error)
}
