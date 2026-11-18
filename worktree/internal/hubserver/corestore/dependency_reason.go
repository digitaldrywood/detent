package corestore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const MaxDependencyReasonEdges = 10000

type DependencyReason struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	WorkItemID     tracker.NativeWorkItemID
	Kind           string
	Reason         tracker.DependencyReason
}

type DependencyReasonEdge struct {
	ProjectID        tracker.ProjectID
	WorkItemID       tracker.NativeWorkItemID
	ActorPrincipalID *string
}

type DependencyReasonReader interface {
	DependencyReasonRecords(context.Context, tracker.ProjectID, []tracker.NativeWorkItemID) ([]DependencyReason, error)
	DependencyReasonEdges(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) ([]DependencyReasonEdge, error)
}

type DependencyReasonWriter interface {
	DependencyReasonReader
	CompareAndSwapDependencyReason(context.Context, DependencyReason, int64) error
}
