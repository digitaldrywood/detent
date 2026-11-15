package billingstore

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

var ErrCostOverlap = fmt.Errorf("cost period overlaps another source: %w", mutation.ErrConflict)

type CostObservation struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	Observation    usagecost.Observation
	Reference      AuditReference
	ReceivedAt     time.Time
	Stored         attachment.VerifiedDetail `json:"-"`
}

type CostSource struct {
	Provider        string
	ProviderAccount string
	SourceID        string
	From            time.Time
	To              time.Time
}

type CostRevision struct {
	ReceivedAt time.Time
	ProjectID  tracker.ProjectID
	Revision   int64
	Reference  AuditReference
}

type MonthlyCostQuery struct {
	Projects []string
	From     time.Time
	To       time.Time
}

type MonthlyCostRevision struct {
	CostRevision
	SourceKey string
}

type MonthlyCostResource struct {
	ProjectID       tracker.ProjectID
	ProviderAccount string
	Name            string
}

type MonthlyCostSnapshot struct {
	Revisions          []MonthlyCostRevision
	Conversations      []ConversationUsage
	Resources          []MonthlyCostResource
	ActiveAttempts     int64
	UnreportedAttempts int64
}

type CostConflictReader interface {
	BillingCostConflict(context.Context, []CostObservation) (bool, error)
}

type MonthlyCostReader interface {
	ReadMonthlyCosts(context.Context, MonthlyCostQuery) (MonthlyCostSnapshot, error)
}
