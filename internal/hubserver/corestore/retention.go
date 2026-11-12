package corestore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type WorkItemRetentionCursor struct {
	TerminalEnteredAt time.Time
	ID                int64
}
type WorkItemRetentionCandidate struct {
	ProjectID               tracker.ProjectID
	WorkItemID              tracker.NativeWorkItemID
	Revision, EventSequence int64
	Cursor                  WorkItemRetentionCursor
}
type WorkItemRetentionPage struct {
	Items []WorkItemRetentionCandidate
	Next  WorkItemRetentionCursor
}
type WorkItemRetentionReader interface {
	WorkItemRetentionCursor(context.Context) (WorkItemRetentionCursor, error)
	WorkItemRetentionCandidates(context.Context, time.Time, WorkItemRetentionCursor, int) (WorkItemRetentionPage, error)
	WorkItemRetentionPolicy(context.Context, WorkItem, int64, time.Time) (int64, bool, error)
}
type WorkItemRetentionWriter interface {
	WorkItemRetentionReader
	AdvanceWorkItemRetention(context.Context, WorkItemRetentionCursor, WorkItemRetentionCursor) (bool, error)
}
