package billingstore

import (
	"context"
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

type MonthlyCostReader interface {
	ReadMonthlyCosts(context.Context, MonthlyCostQuery) (MonthlyCostSnapshot, error)
}
