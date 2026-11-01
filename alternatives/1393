package accessstore

import (
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Audit struct {
	OrganizationID tracker.OrganizationID
	ID             int64
	SessionID      string
	ActualActor    string
	EffectiveUser  string
	Reason         string
	Event          string
	Route          string
	ProjectID      tracker.ProjectID
	Status         int
	StartedAt      time.Time
	ExpiresAt      time.Time
	RecordedAt     time.Time
	RecordedAtNS   int64
	Mutation       *attachment.ArchiveReference
	AccountKind    string
	AccountRetry   string
	KeyID          string
	KeyKind        string
	KeyOwner       string
	Resource       string
}
