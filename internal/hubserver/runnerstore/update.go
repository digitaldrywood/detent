package runnerstore

import (
	"context"
	"strings"
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
	DesiredAutomatic    bool
	Observation         *runnerauth.UpdateObservation
}

func (s UpdateState) AutomaticOfferIsObsolete(target string) bool {
	return s.DesiredAutomatic && s.Desired != nil && s.Desired.Version != strings.TrimPrefix(target, "v")
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
	InvalidateAutomaticRunnerOffer(context.Context, string, string) error
}

type UrgentUpdate struct {
	OrganizationID tracker.OrganizationID
	Revision       int64
	Request        *runnerauth.UpdateRequest
	UpdatedAt      time.Time
}
