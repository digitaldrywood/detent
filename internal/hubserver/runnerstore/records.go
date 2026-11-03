package runnerstore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

var (
	ErrInvalidRecord     = errors.New("runner record is invalid")
	ErrLimitExceeded     = errors.New("runner project list exceeds its response limit")
	ErrIdentityCollision = errors.New("runner binding is already registered")
	ErrStaleObservation  = errors.New("runner project observation is stale")
)

const MaxRunnerProjectList = 2000
const MaxRunnerProjectRanks = 100
const MaxProjectDiagnosticAttempts = 256
const MaxRunnerHostPruneBatch = 5000
const MaxProjectSkills = 2000

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
	OrganizationID        tracker.OrganizationID
	RunnerID              string
	MachineID             tracker.MachineID
	TokenID               string
	EnrollmentID          string
	Scope                 string
	State                 string
	Revision              int64
	Tags                  []string
	MachineUse            string
	IsolationTier         string
	HostServices          []string
	Availability          Availability
	Operations            []string
	DisplayName           string
	CapacityLimit         int64
	ReportedCapacity      int64
	CapacityConfig        *runnerauth.CapacityConfig
	ProjectIDs            []tracker.ProjectID
	ProjectCapacityLimits map[tracker.ProjectID]int
	ProjectRanks          map[tracker.ProjectID]int
	OS                    string
	Architecture          string
	CreatedAt             time.Time
	ExpiresAt             time.Time
	LastHeartbeatAt       time.Time
	RemovedAt             *time.Time
}

type Availability struct {
	Timezone     string
	Windows      []string
	WindowSlots  map[string]int
	HardDeadline string
}

func (row Identity) Normalized() Identity {
	if row.State == "" {
		row.State = "active"
	}
	if row.MachineUse == "" {
		row.MachineUse = "shared"
	}
	if row.IsolationTier == "" {
		row.IsolationTier = "sandbox"
	}
	if row.Revision < 1 {
		row.Revision = 1
	}
	if row.Tags == nil {
		row.Tags = []string{}
	}
	if row.HostServices == nil {
		row.HostServices = []string{}
	}
	if row.Availability.Windows == nil {
		row.Availability.Windows = []string{}
	}
	if row.Availability.WindowSlots == nil {
		row.Availability.WindowSlots = map[string]int{}
	}
	if row.ProjectCapacityLimits == nil {
		row.ProjectCapacityLimits = map[tracker.ProjectID]int{}
	}
	if row.ProjectRanks == nil {
		row.ProjectRanks = map[tracker.ProjectID]int{}
	}
	return row
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

type ProjectSkill struct {
	OrganizationID tracker.OrganizationID
	RunnerID       string
	ProjectID      tracker.ProjectID
	Position       int
	Skill          skills.ProviderSkill
	ReportedAt     time.Time
}

type CheckoutReport struct {
	OrganizationID tracker.OrganizationID
	RunnerID       string
	ProjectID      tracker.ProjectID
	Repository     string
	ReportedAt     time.Time
}

func (row ProjectSkill) Validate() error {
	if row.OrganizationID == "" || row.RunnerID == "" || row.ProjectID == "" || row.Position < 0 || row.Position >= MaxProjectSkills || row.ReportedAt.IsZero() || !skills.ValidProviderSkill(row.Skill) {
		return ErrInvalidRecord
	}
	return nil
}

func (row CheckoutReport) Validate() error {
	if row.OrganizationID == "" || row.RunnerID == "" || row.ProjectID == "" || row.ReportedAt.IsZero() || row.Repository != "" && !ValidCheckoutRepository(row.Repository) {
		return ErrInvalidRecord
	}
	return nil
}

func ValidCheckoutRepository(repository string) bool {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || len(repository) > 200 || strings.TrimSpace(repository) != repository {
		return false
	}
	for _, part := range []string{owner, name} {
		if part == "." || part == ".." {
			return false
		}
		for _, char := range part {
			if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
				continue
			}
			return false
		}
	}
	return true
}

type SpriteMembership struct {
	OrganizationID       tracker.OrganizationID
	Name                 string
	ProviderOrganization string
	TokenProjectID       tracker.ProjectID
	EnrollmentID         string
	State                string
	BootstrapLog         string
	IdleSince            time.Time
	CreatedAt            time.Time
	AllocationProjectID  tracker.ProjectID
	AllocationRevision   int64
}

func (row SpriteMembership) Validate() error {
	if row.OrganizationID == "" || row.Name == "" || len(row.Name) > 200 || row.EnrollmentID == "" || row.ProviderOrganization == "" || row.CreatedAt.IsZero() || row.IdleSince.IsZero() || row.AllocationRevision < 0 || row.State != "bootstrapping" && row.State != "enrolled" && row.State != "deleting" && row.State != "deleted" {
		return ErrInvalidRecord
	}
	return nil
}

type LeaseAssignment struct {
	OrganizationID tracker.OrganizationID
	LeaseID        string
	RunnerID       string
	AssignedAt     time.Time
}

type ProviderAccountKey struct {
	RunnerID           string
	Provider           string
	Backend            string
	AccountAlias       string
	SharedAccountAlias string
}

type ProviderAccountCapacity struct {
	SelectedReport bool
	MaxConcurrent  int
	State          string
}

type ProjectObservation struct {
	OrganizationID    tracker.OrganizationID
	RunnerID          string
	ProjectID         tracker.ProjectID
	ObservedAt        time.Time
	DiagnosticsAt     *time.Time
	RuntimeObservedAt *time.Time
	SnapshotDigest    string
}

type ProjectDiagnosticAttempt struct {
	Position            int
	RecordKey           string
	IssueID             string
	LocalAttemptID      int64
	NativeAttemptID     string
	Generation          int64
	Fence               uint64
	Stage               string
	Running             bool
	NativeClaim         bool
	ProviderCompletedAt *time.Time
	HostOperation       string
	OperationPending    bool
	LeaseRenewedAt      *time.Time
}

func (row ProjectObservation) Validate(attempts []ProjectDiagnosticAttempt) error {
	if row.OrganizationID == "" || row.RunnerID == "" || row.ProjectID == "" || row.ObservedAt.IsZero() || len(attempts) > MaxProjectDiagnosticAttempts {
		return ErrInvalidRecord
	}
	digest, err := hex.DecodeString(row.SnapshotDigest)
	if err != nil || len(digest) != 32 || len(row.SnapshotDigest) != 64 || hex.EncodeToString(digest) != row.SnapshotDigest {
		return ErrInvalidRecord
	}
	for i, attempt := range attempts {
		if attempt.Position != i || len(attempt.RecordKey) == 0 || len(attempt.RecordKey) > 256 || len(attempt.IssueID) == 0 || len(attempt.IssueID) > 256 || attempt.LocalAttemptID < 0 || attempt.Generation < 0 || attempt.Fence > 1<<63-1 || len(attempt.NativeAttemptID) > 256 || len(attempt.Stage) > 256 || len(attempt.HostOperation) > 256 {
			return fmt.Errorf("%w: invalid project diagnostic attempt %d", ErrInvalidRecord, i)
		}
	}
	return nil
}

type Reader interface {
	RunnerEnrollmentByTokenHash(context.Context, string) (Enrollment, error)
	RunnerEnrollment(context.Context, string) (Enrollment, error)
	RunnerIdentity(context.Context, string) (Identity, error)
	RunnerIdentityByCredential(context.Context, string) (Identity, error)
	RunnerIdentityByMachine(context.Context, tracker.MachineID, int) ([]Identity, error)
	RunnerIdentities(context.Context, int) ([]Identity, error)
	RunnerForLease(context.Context, string) (Identity, error)
	RunnerProjects(context.Context, string, int) ([]tracker.ProjectID, error)
	RunnerProjectAuthorized(context.Context, string, tracker.ProjectID, time.Time) (bool, error)
	RunnerActiveLeaseIDs(context.Context, string, time.Time, int) ([]string, error)
	RunnerHasActiveLease(context.Context, string, time.Time) (bool, error)
	RunnerProjectUsage(context.Context, string, tracker.ProjectID, time.Time) (runnerUsed, runnerProjectUsed int64, err error)
	RunnerBindingAvailable(context.Context, string, tracker.MachineID, bool) error
	RunnerRepositoryList(context.Context, string) (RepositoryList, error)
	RunnerRepositories(context.Context, string, int) ([]Repository, error)
	RunnerBackendIsolation(context.Context, string) (isolation.Report, error)
	RunnerSpriteMembershipByEnrollment(context.Context, string) (SpriteMembership, error)
	RunnerProjectObservation(context.Context, string, tracker.ProjectID, string) (ProjectObservation, []ProjectDiagnosticAttempt, error)
	RunnerHostMetrics(context.Context, string, time.Time, time.Time) ([]hostmetrics.Summary, error)
	RunnerAdmissionObservation(context.Context, string, tracker.ProjectID) (tracker.NativeAdmissionObservation, error)
	RunnerProviderReports(context.Context, string) ([]providercapacity.Report, error)
	RunnerProblems(context.Context, string) ([]runnerauth.Problem, error)
	RunnerLocalChecks(context.Context, string, tracker.ProjectID) (runnerauth.LocalChecks, error)
	RunnerWorkspaceReport(context.Context, string) (workspacesession.Capabilities, string, error)
	RunnerProjectSkills(context.Context, tracker.ProjectID, string, time.Time, int) ([]ProjectSkill, error)
	RunnerCheckoutVerified(context.Context, tracker.ProjectID, string, time.Time) (bool, error)
	RunnerUpdateState(context.Context, string) (UpdateState, error)
	RunnerUpdateRequest(context.Context, string) (UpdateRequestRecord, error)
	RunnerUrgentUpdate(context.Context) (UrgentUpdate, error)
}

type UpdateProjection struct {
	RunnerID      string
	DisplayName   string
	Version       string
	LastHeartbeat time.Time
}

type UpdateProjectionCursor struct {
	DisplayName string
	RunnerID    string
}

type UpdateProjectionPageReader interface {
	RunnerUpdateProjectionPage(context.Context, UpdateProjectionCursor, int) ([]UpdateProjection, error)
}

type Writer interface {
	Reader
	CreateRunnerEnrollment(context.Context, Enrollment) error
	RevokeRunnerEnrollment(context.Context, string, time.Time) error
	RedeemRunnerEnrollment(context.Context, string, string, tracker.MachineID, time.Time) error
	CreateRunnerIdentity(context.Context, Identity) error
	AssignRunnerLease(context.Context, LeaseAssignment) error
	UpdateRunnerIdentity(context.Context, Identity) error
	UpdateRunnerRouting(context.Context, int64, Identity) error
	RemoveRunnerIdentity(context.Context, string, time.Time) error
	RecordRunnerIdentityEvent(context.Context, Event) error
	RequestRunnerRepositories(context.Context, string, string) error
	ReplaceRunnerRepositories(context.Context, RepositoryList, []Repository) error
	ReplaceRunnerBackendIsolation(context.Context, string, isolation.Report) error
	CreateRunnerSpriteMembership(context.Context, SpriteMembership) error
	UpdateRunnerSpriteMembership(context.Context, SpriteMembership) error
	ReplaceRunnerProjectObservation(context.Context, ProjectObservation, []ProjectDiagnosticAttempt) error
	AppendRunnerHostMetricSegments(context.Context, string, []hostmetrics.Summary, time.Time) ([]hostmetrics.Acknowledgment, error)
	ReplaceRunnerAdmissionObservation(context.Context, string, tracker.ProjectID, tracker.NativeAdmissionObservation) error
	PruneRunnerHostMetrics(context.Context, time.Time, int) error
	ReplaceRunnerProviderReports(context.Context, string, []providercapacity.Report) error
	LockRunnerProviderAccount(context.Context, ProviderAccountKey, time.Time) (ProviderAccountCapacity, error)
	UpdateRunnerCapacityConfig(context.Context, string, runnerauth.CapacityConfig) error
	ReplaceRunnerProblems(context.Context, string, []runnerauth.Problem) error
	ReplaceRunnerLocalChecks(context.Context, string, tracker.ProjectID, runnerauth.LocalChecks) error
	ReplaceRunnerWorkspaceReport(context.Context, string, workspacesession.Capabilities, string) error
	ReplaceRunnerProjectSkills(context.Context, string, tracker.ProjectID, []skills.ProviderSkill, time.Time) error
	ReplaceRunnerCheckoutReport(context.Context, CheckoutReport) error
	CreateRunnerUpdateRequest(context.Context, UpdateRequestRecord) error
	SetRunnerUpdateDesired(context.Context, string, int64, string) error
	ReplaceRunnerUpdateObservation(context.Context, string, *runnerauth.UpdateObservation) error
	SetRunnerUrgentUpdate(context.Context, int64, string, time.Time) error
}
