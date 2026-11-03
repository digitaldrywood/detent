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
	RunnerID         string
	MachineID        tracker.MachineID
	SessionID        string
	LandingCapacity  int64
	Lease            runnerauth.RunnerLease
	ActorPrincipalID string
	Reservation      *providercapacity.Reservation
}

type FleetRecord struct {
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
