package releaseauditstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/releaseaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Reader interface {
	ReleaseAudit(context.Context, tracker.ProjectID, string) (releaseaudit.Checkpoint, error)
	ReleaseAudits(context.Context, tracker.ProjectID, string) (releaseaudit.CheckpointPage, error)
}

type Writer interface {
	Reader
	PutReleaseAudit(context.Context, tracker.ProjectID, releaseaudit.Checkpoint, time.Time) error
}
