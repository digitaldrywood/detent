package tenantstore

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type BoardChangeRequest struct {
	WorkItemID tracker.NativeWorkItemID
	ChangeID   string
}
type BoardChangeLink struct {
	ProjectID        tracker.ProjectID
	WorkItemID       tracker.NativeWorkItemID
	ActorPrincipalID *string
	ItemExists       bool
	VersionExists    bool
}
type BoardChangeRows struct {
	Request             BoardChangeRequest
	Change              *eventstore.Change
	Version             *eventstore.Version
	Links               []BoardChangeLink
	Evidence            []eventstore.Evidence
	AuditDispositions   map[int64][]eventstore.AuditDisposition
	VersionObservations *ChangeVersionRows
}

type ChangeVersionRequest struct {
	WorkItemID tracker.NativeWorkItemID
	Reference  tracker.NativeChangeReference
}

type ChangeVersionRows struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	Request        ChangeVersionRequest
	Publication    *eventstore.NativePRPublication
	Landing        *eventstore.LandingObservation
}

type ChangeVersionRowsReader interface {
	ChangeVersionRows(context.Context, tracker.ProjectID, []ChangeVersionRequest) ([]ChangeVersionRows, error)
}
type BoardChangeRowsReader interface {
	BoardChangeRows(context.Context, tracker.ProjectID, []BoardChangeRequest) ([]BoardChangeRows, error)
}

type ClaimChangeRowsReader interface {
	ClaimChangeRows(context.Context, tracker.ProjectID, []string) (map[string]BoardChangeRows, error)
}

type ClaimAuditDispositionRowsReader interface {
	ClaimAuditDispositionRows(context.Context, tracker.ProjectID, []int64) (map[int64][]eventstore.AuditDisposition, error)
}
