package runnerstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type UpdateState struct {
	UrgentPolicyChecked bool
	UrgentAllowed       bool
	TargetVersion       string
	TargetHeld          bool
	OrganizationID      tracker.OrganizationID
	RunnerID            string
	Desired             *runnerauth.UpdateRequest
	Observation         *runnerauth.UpdateObservation
}

type UpdateRequestRecord struct {
	OrganizationID           tracker.OrganizationID
	RunnerID                 string
	Request                  runnerauth.UpdateRequest
	HoldRevision             int64
	HoldOrganizationRevision int64
	OrganizationHold         bool
}

type AutomaticOfferPolicy struct {
	OrganizationRevision int64
	RunnerRevision       int64
}

type AutomaticOfferWriter interface {
	EnsureAutomaticRunnerOffer(context.Context, UpdateRequestRecord, AutomaticOfferPolicy) (string, error)
}

type UrgentUpdate struct {
	OrganizationID tracker.OrganizationID
	Revision       int64
	Request        *runnerauth.UpdateRequest
	UpdatedAt      time.Time
}
