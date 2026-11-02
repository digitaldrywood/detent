package billingstore

import (
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

type CostObservation struct {
	OrganizationID tracker.OrganizationID
	ProjectID      tracker.ProjectID
	Observation    usagecost.Observation
	Reference      AuditReference
	ReceivedAt     time.Time
}

type CostSource struct {
	Provider        string
	ProviderAccount string
	SourceID        string
	From            time.Time
	To              time.Time
}

type CostRevision struct {
	ProjectID tracker.ProjectID
	Revision  int64
	Reference AuditReference
}
