package searchstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type DuplicateSourceState struct {
	Sequence int64
	Ready    bool
}

type DuplicateSourceQuery struct {
	ProjectID tracker.ProjectID
	Request   *Text
	Text      string
	Exact     bool
	Machine   bool
	Sequence  *int64
	Reporter  *string
	AfterID   string
	Limit     int
	Now       time.Time
}

type DuplicateSource struct {
	Parent     Document
	Document   Document
	Statistics Statistics
	Revision   tracker.Revision
	Number     int64
	State      string
	Terminal   bool
}

type DuplicateSourcePage struct {
	Sources   []DuplicateSource
	NextID    string
	Exhausted bool
	State     DuplicateSourceState
}

type DuplicateSourceReader interface {
	DuplicateSourceState(context.Context, tracker.ProjectID) (DuplicateSourceState, error)
	DuplicateSources(context.Context, DuplicateSourceQuery) (DuplicateSourcePage, error)
}

type DuplicateSourceWriter interface {
	LockDuplicateSource(context.Context, tracker.ProjectID, int64) error
}
