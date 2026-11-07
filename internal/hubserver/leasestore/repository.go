package leasestore

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrAttemptConflict = fmt.Errorf("attempt identity or sequence has changed: %w", mutation.ErrConflict)

type FenceError struct {
	Check string
}

func (e *FenceError) Error() string {
	return tracker.ErrStaleFencingToken.Error()
}

func (e *FenceError) Unwrap() error {
	return tracker.ErrStaleFencingToken
}

type Repository interface {
	ReadLeases(context.Context, tracker.OrganizationID, corestore.Workload, func(Reader) error) error
	WriteLeases(context.Context, tracker.OrganizationID, corestore.Workload, func(Writer) error) error
}

type Reader interface {
	AttemptDiffBodyReferenced(context.Context, tracker.ProjectID, string) (bool, bool, error)
	Machine(context.Context, string) (Machine, error)
	Lease(context.Context, string) (Lease, error)
	LeaseBySession(context.Context, string) (Lease, error)
	ActiveLeaseForIssue(context.Context, int64) (Lease, error)
	Attempt(context.Context, string) (Attempt, error)
	LatestAttempt(context.Context, int64) (Attempt, error)
	AttemptPage(context.Context, int64, AttemptCursor, int) (AttemptPage, error)
	AttemptDiff(context.Context, tracker.ProjectID, string, tracker.DiffGeneration) (AttemptDiff, error)
	LatestAttemptDiff(context.Context, tracker.ProjectID, string, string) (AttemptDiff, error)
	LatestWorkItemAttemptDiff(context.Context, tracker.ProjectID, tracker.NativeWorkItemID, string) (AttemptDiff, error)
	LatestAttemptDiffSequence(context.Context, string, string, string) (int64, error)
	Usage(context.Context, string) (int64, error)
}

type CheckpointAttemptReader interface {
	CheckpointAttemptPage(context.Context, int64, AttemptCursor, int) (AttemptPage, error)
}

type Writer interface {
	IndexAttemptDiffBodies(context.Context, tracker.ProjectID, string, []string) error
	Reader
	LockMachine(context.Context, string) (Machine, error)
	RegisterMachine(context.Context, Machine) error
	HeartbeatMachine(context.Context, Machine, time.Time) error
	Claim(context.Context, Claim) (Lease, error)
	AdvanceLeaseWorkItemRevision(context.Context, Fence, int64, int64, int64) error
	Renew(context.Context, Fence, time.Duration) (Lease, error)
	RenewWithLandingCapacity(context.Context, Fence, time.Duration, int64) (Lease, error)
	Release(context.Context, Fence, string) (Lease, error)
	Expire(context.Context, Fence) (Lease, error)
	RecordAttempt(context.Context, AttemptEvent) (Attempt, error)
	RecordAttemptEvent(context.Context, AttemptEvent) (Attempt, bool, error)
	SetAttemptRuntimeHeartbeat(context.Context, string, int64, *time.Time) error
	CreateAttemptDiff(context.Context, AttemptDiff) error
	LockAttemptDiffGeneration(context.Context, string, string, string, string, string) (int64, error)
}

type UnreleasedIssueLeaseLocker interface {
	LockUnreleasedIssueLease(context.Context, int64) (Lease, error)
}

type FencedLeaseLocker interface {
	LockLease(context.Context, string) (Lease, error)
}

type Machine struct {
	OrganizationID tracker.OrganizationID
	ID             string
	Hostname       string
	DisplayName    string
	Capacity       int64
	Version        string
	SpriteName     string
}

type MachineRegistrationWriter interface {
	RegisterOwnedMachine(context.Context, Machine, string) error
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
	LandingCapacity  int64
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
	RuntimeHeartbeatAt     *time.Time
	RuntimeSummarySequence int64
	CleanupAcknowledgedAt  *time.Time
	OrganizationID         tracker.OrganizationID
	ID                     string
	IssueID                int64
	LeaseID                string
	FencingToken           int64
	RunID                  string
	Sequence               int64
	Status                 string
	WorkItemRevision       int64
	DispatchGeneration     int64
	PayloadObjectKey       string
	CheckpointObjectKey    *string
	ArtifactsObjectKey     *string
	StartedAt              time.Time
	UpdatedAt              time.Time
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

type AttemptDiff struct {
	OrganizationID  tracker.OrganizationID
	ID              string
	AttemptID       string
	IssueID         int64
	ProjectID       tracker.ProjectID
	WorkItemID      tracker.NativeWorkItemID
	Generation      tracker.DiffGeneration
	Producer        tracker.DiffProducer
	BaseSHA         string
	HeadSHA         string
	FileCount       int
	PatchBytes      int64
	PostedBytes     int64
	Truncated       bool
	ManifestExpired bool
	ArchiveID       string
	CreatedAt       time.Time
}

type AttemptEvent struct {
	RuntimeHeartbeatAt  *time.Time                  `json:"-"`
	CheckpointReference attachment.ArchiveReference `json:"-"`
	Termination         bool                        `json:"termination,omitempty"`
	PayloadReference    attachment.ArchiveReference `json:"-"`
	Usage               []tracker.NativeUsage       `json:"-"`
	ID                  string
	Fence               Fence
	RunID               string
	Sequence            int64
	Status              string
	DispatchGeneration  int64
	StartedAt           time.Time
	PayloadObjectKey    string
	CheckpointObjectKey *string
	ArtifactsObjectKey  *string
	UsageCorrection     bool
}
