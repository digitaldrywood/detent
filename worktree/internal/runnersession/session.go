package runnersession

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/policy"
	sessionmodel "github.com/digitaldrywood/detent/internal/runnersession/model"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

const HistoryLimit = 200

type Snapshot struct {
	Finalization *Finalization `json:",omitempty"`
	ReceiptID    int64
	ID           int64
	Revision     int64
	Start        store.SessionStart
	Finish       *store.SessionFinish
	Identity     agentidentity.Identity
	Provider     store.SessionProviderIdentity
	Resume       store.SessionResumeState
	Progress     *store.SessionProgress
	Policy       policy.Descriptor
	Worker       *store.WorkerProcessRegistration
	Reaped       *store.WorkerProcessReap
	Authority    string
}

type Finalization struct {
	Input         sessionmodel.Finalization
	Finished      bool
	UsageRecorded bool
	PhaseRecorded bool
}

type Mutation struct {
	Finalization     *sessionmodel.Finalization `json:",omitempty"`
	Key              string
	LeaseID          tracker.LeaseID
	FencingToken     tracker.FencingToken
	SessionID        int64
	ExpectedRevision int64
	Start            *store.SessionStart
	Finish           *store.SessionFinish
	Identity         *agentidentity.Identity
	Provider         *store.SessionProviderIdentity
	Resume           *store.SessionResumeState
	Progress         *store.SessionProgress
	Phase            *store.WorkflowPhaseEvent
	Profile          *Activity
	Usage            *store.UsageEvent
	Worker           *store.WorkerProcessRegistration
	Reaped           *store.WorkerProcessReap
	Policy           *policy.Descriptor
	ProcessKey       string
}

type Activity struct {
	Phase   store.WorkflowPhaseEvent
	Profile workflowmetrics.ActivityProfile
}

type Record struct {
	ReceiptID         int64
	ProviderThreadID  string
	ProviderSessionID string
	PRNumber          int64
	PRHeadSHA         string
	PRBaseSHA         string
	FinalState        string
	Organization      tracker.OrganizationID
	Project           tracker.ProjectID
	IssueID           string
	AttemptID         string
	ID                int64
	Revision          int64
	StartKey          string
	StartedAt         time.Time
	CompletedAt       *time.Time
	BackendID         string
	BackendKind       string
	Role              string
	Model             string
	PolicyID          string
	WorkerPID         int
	WorkerGroupID     int
	WorkerStartedAt   *time.Time
	WorkerReapedAt    *time.Time
	Reference         attachment.ArchiveReference
}

type History struct {
	ID        int64
	SessionID int64
	Revision  int64
	Key       string
	Reference attachment.ArchiveReference
}

type Processes struct {
	Workers    []store.WorkerProcess
	Unobserved bool
	More       bool
}

type Reader interface {
	RunnerSessionTime(context.Context) (time.Time, error)
	RunnerSession(context.Context, tracker.ProjectID, int64) (Record, error)
	FindRunnerSession(context.Context, tracker.ProjectID, string, string, store.AgentResumeLookup) (Record, error)
	RunnerSessionHistory(context.Context, tracker.ProjectID, int64, int64, int) ([]History, error)
	RunnerSessionProcesses(context.Context, tracker.ProjectID, string, string, int) ([]Record, error)
}

type ResumeLookup struct {
	CurrentAttempt bool
	Lookup         store.AgentResumeLookup
	LeaseID        tracker.LeaseID
	FencingToken   tracker.FencingToken
}

type Writer interface {
	Reader
	CreateRunnerSession(context.Context, Record, History) (Record, error)
	UpdateRunnerSession(context.Context, Record, History) (int64, error)
}
