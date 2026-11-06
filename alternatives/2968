package projectstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type IntakeBatch struct {
	ProjectID tracker.ProjectID
	ID        string
	Revision  int64
	Status    string
	HubOwned  bool
	Payload   eventstore.Receipt
}

type IntakeBatchReader interface {
	ProjectIntakeBatch(context.Context, tracker.ProjectID) (IntakeBatch, error)
}

type IntakeBatchWriter interface {
	IntakeBatchReader
	SaveProjectIntakeBatch(context.Context, IntakeBatch, int64) error
}
