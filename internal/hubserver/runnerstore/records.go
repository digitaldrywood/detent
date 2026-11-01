package runnerstore

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

var (
	ErrInvalidRecord     = errors.New("runner record is invalid")
	ErrLimitExceeded     = errors.New("runner project list exceeds its response limit")
	ErrIdentityCollision = errors.New("runner binding is already registered")
)

const MaxRunnerProjectList = 2000

type Enrollment struct {
	OrganizationID tracker.OrganizationID
	ID             string
	RunnerID       string
	MachineID      tracker.MachineID
	TokenHash      string
	Scope          string
	SharedMachine  bool
	Operations     []string
	ProjectIDs     []tracker.ProjectID
	CreatedAt      time.Time
	ExpiresAt      time.Time
	CreatedBy      string
	RedeemedAt     *time.Time
	RevokedAt      *time.Time
}

type Identity struct {
	OrganizationID   tracker.OrganizationID
	RunnerID         string
	MachineID        tracker.MachineID
	TokenID          string
	EnrollmentID     string
	Scope            string
	Operations       []string
	DisplayName      string
	CapacityLimit    int64
	ReportedCapacity int64
	OS               string
	Architecture     string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	LastHeartbeatAt  time.Time
	RemovedAt        *time.Time
}

type Event struct {
	OrganizationID tracker.OrganizationID
	RunnerID       string
	ActorID        string
	Kind           string
	OccurredAt     time.Time
}

type RepositoryList struct {
	OrganizationID tracker.OrganizationID
	RunnerID       string
	RequestID      string
	Pending        bool
	Reason         string
	ListedAt       *time.Time
}

type Repository struct {
	OrganizationID tracker.OrganizationID
	RunnerID       string
	Owner          string
	Name           string
	Private        bool
	PushedAt       *time.Time
	ListedAt       time.Time
}

type LeaseAssignment struct {
	OrganizationID tracker.OrganizationID
	LeaseID        string
	RunnerID       string
	AssignedAt     time.Time
}

type Reader interface {
	RunnerEnrollmentByTokenHash(context.Context, string) (Enrollment, error)
	RunnerEnrollment(context.Context, string) (Enrollment, error)
	RunnerIdentity(context.Context, string) (Identity, error)
	RunnerIdentityByCredential(context.Context, string) (Identity, error)
	RunnerIdentityByMachine(context.Context, tracker.MachineID, int) ([]Identity, error)
	RunnerForLease(context.Context, string) (Identity, error)
	RunnerProjects(context.Context, string, int) ([]tracker.ProjectID, error)
	RunnerProjectAuthorized(context.Context, string, tracker.ProjectID, time.Time) (bool, error)
	RunnerActiveLeaseIDs(context.Context, string, time.Time, int) ([]string, error)
	RunnerBindingAvailable(context.Context, string, tracker.MachineID, bool) error
	RunnerRepositoryList(context.Context, string) (RepositoryList, error)
	RunnerRepositories(context.Context, string, int) ([]Repository, error)
}

type Writer interface {
	Reader
	CreateRunnerEnrollment(context.Context, Enrollment) error
	RevokeRunnerEnrollment(context.Context, string, time.Time) error
	RedeemRunnerEnrollment(context.Context, string, string, tracker.MachineID, time.Time) error
	CreateRunnerIdentity(context.Context, Identity) error
	AssignRunnerLease(context.Context, LeaseAssignment) error
	UpdateRunnerIdentity(context.Context, Identity) error
	RemoveRunnerIdentity(context.Context, string, time.Time) error
	RecordRunnerIdentityEvent(context.Context, Event) error
	RequestRunnerRepositories(context.Context, string, string) error
	ReplaceRunnerRepositories(context.Context, RepositoryList, []Repository) error
}
