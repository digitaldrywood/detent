package runnerstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type FleetName struct {
	RunnerID    string
	DisplayName string
	Hostname    string
	Removed     bool
}

type FleetLease struct {
	LandingRequired    *bool
	WorkflowState      string
	CapacityProject    string
	RuntimeAttemptID   string
	RuntimeHeartbeatAt *time.Time
	RenewedAt          time.Time
	PolicyID           string
	CurrentPolicyID    string
	RunnerID           string
	MachineID          tracker.MachineID
	SessionID          string
	LandingCapacity    int64
	Lease              runnerauth.RunnerLease
	ActorPrincipalID   string
	Reservation        *providercapacity.Reservation
}

type FleetRecord struct {
	Lifecycle           *runnerauth.Lifecycle
	HostRevision        int64
	OwnerPrincipalID    string
	OwnerUserID         string
	CredentialRevoked   bool
	CredentialCreatedAt time.Time
	CredentialExpiresAt *time.Time
	Identity            Identity
	Machine             leasestore.Machine
	Reports             []providercapacity.Report
	Problems            []runnerauth.Problem
	Checks              map[tracker.ProjectID]runnerauth.LocalChecks
	Isolation           isolation.Report
	Update              UpdateState
}

type FleetReader interface {
	FleetProjectCount(context.Context, int) (int, error)
	FleetRecords(context.Context, int) ([]FleetRecord, error)
	FleetNames(context.Context, int) ([]FleetName, error)
	FleetLeases(context.Context, time.Time, int) ([]FleetLease, error)
}

type RoutingRecord struct {
	Record    FleetRecord
	Leases    []FleetLease
	Providers []providercapacity.View
}

type RoutingPageReader interface {
	RunnerRoutingRecords(context.Context, int, int, time.Time) ([]RoutingRecord, error)
	RunnerProjectRoutingRecords(context.Context, tracker.ProjectID, int, int, time.Time) ([]RoutingRecord, error)
}

type RoutingReader interface {
	RunnerRoutingRecord(context.Context, string, time.Time) (RoutingRecord, error)
}

const SourceRecoveryPageLimit = 32

type SourceRecoveryCandidate struct {
	Record      FleetRecord
	RunnerUsed  int
	ProjectUsed int
	HostUsed    int
	Providers   []providercapacity.View
}

type SourceRecoveryPage struct {
	Items []SourceRecoveryCandidate
	Next  string
}

type SourceRecoveryReader interface {
	ProjectSourceRecoveryPage(context.Context, tracker.ProjectID, string, int, time.Time) (SourceRecoveryPage, error)
}

type OnboardingReader interface {
	FleetReader
	ProjectRunnerChecks(context.Context, tracker.ProjectID, int) (map[string]runnerauth.LocalChecks, error)
	RunnerRepositoryLists(context.Context, int) (map[string]runnerauth.RepositoryList, error)
}

type SourceRecoveryDestinationReader interface {
	SourceRecoveryDestination(context.Context, tracker.ProjectID, string, time.Time) (SourceRecoveryCandidate, error)
}

type RuntimeProviderReport struct {
	CapacityOwner bool
	RunnerID      string
	Report        providercapacity.Report
}

type RuntimeProviderSnapshot struct {
	Reports             []RuntimeProviderReport
	Leases              []FleetLease
	Candidates          int
	OwnersTruncated     bool
	CandidatesTruncated bool
}

type RuntimeCapacityQuery struct {
	ProjectID   tracker.ProjectID
	RunnerID    string
	At          time.Time
	Explanation bool
}

type RuntimeCapacitySnapshot struct {
	Records   []RuntimeCapacityRunner
	Leases    []FleetLease
	Providers RuntimeProviderSnapshot
	Truncated bool
}

type RuntimeCapacityReader interface {
	RuntimeCapacitySnapshot(context.Context, RuntimeCapacityQuery) (RuntimeCapacitySnapshot, error)
}

type RuntimeCapacityRunner struct {
	Runner         runnerauth.Runner
	MachineVersion string
	Reports        []providercapacity.Report
}
