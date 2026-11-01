package leasestore

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrAttemptConflict = fmt.Errorf("attempt identity or sequence has changed: %w", mutation.ErrConflict)

type Repository interface {
	ReadLeases(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
	WriteLeases(context.Context, tracker.OrganizationID, corestore.Workload, func(Writer) error) error
}

type Reader interface {
	Lease(context.Context, string) (Lease, error)
	ActiveLeaseForIssue(context.Context, int64) (Lease, error)
	Attempt(context.Context, string) (Attempt, error)
	LatestAttempt(context.Context, int64) (Attempt, error)
	AttemptPage(context.Context, int64, AttemptCursor, int) (AttemptPage, error)
	Usage(context.Context, string) (int64, error)
}

type Writer interface {
	Reader
	RegisterMachine(context.Context, Machine) error
	Claim(context.Context, Claim) (Lease, error)
	Renew(context.Context, Fence, time.Duration) (Lease, error)
	RenewWithLandingCapacity(context.Context, Fence, time.Duration, int64) (Lease, error)
	Release(context.Context, Fence, string) (Lease, error)
	Expire(context.Context, Fence) (Lease, error)
	RecordAttempt(context.Context, AttemptEvent) (Attempt, error)
	RecordAttemptEvent(context.Context, AttemptEvent) (Attempt, bool, error)
}

type Machine struct {
	OrganizationID tracker.OrganizationID
	ID             string
	Hostname       string
	DisplayName    string
	Capacity       int64
	Version        string
}

type Fence struct {
	LeaseID      string
	FencingToken int64
}

type Claim struct {
	IssueID          int64
	ExpectedRevision int64
	MachineID        string
	SessionID        string
	LeaseID          string
	TTL              time.Duration
}

type Lease struct {
	OrganizationID   tracker.OrganizationID
	LeaseID          string
	FencingToken     int64
	IssueID          int64
	MachineID        string
	SessionID        string
	WorkItemRevision int64
	LandingCapacity  int64
	AcquiredAt       time.Time
	RenewedAt        time.Time
	ExpiresAt        time.Time
	ReleasedAt       *time.Time
	ReleaseReason    string
}

type Attempt struct {
	OrganizationID      tracker.OrganizationID
	ID                  string
	IssueID             int64
	LeaseID             string
	FencingToken        int64
	RunID               string
	Sequence            int64
	Status              string
	WorkItemRevision    int64
	DispatchGeneration  int64
	PayloadObjectKey    string
	CheckpointObjectKey *string
	ArtifactsObjectKey  *string
	StartedAt           time.Time
	UpdatedAt           time.Time
}

type AttemptCursor struct {
	IssueID     int64
	BeforeFence int64
	AfterFence  int64
	Oldest      bool
}

type AttemptPage struct {
	Items []Attempt
	Next  AttemptCursor
}

type AttemptEvent struct {
	ID                  string
	Fence               Fence
	RunID               string
	Sequence            int64
	Status              string
	DispatchGeneration  int64
	PayloadObjectKey    string
	CheckpointObjectKey *string
	ArtifactsObjectKey  *string
}
