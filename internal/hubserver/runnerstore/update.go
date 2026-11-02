package runnerstore

import (
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type UpdateState struct {
	OrganizationID tracker.OrganizationID
	RunnerID       string
	Desired        *runnerauth.UpdateRequest
	Observation    *runnerauth.UpdateObservation
}

type UpdateRequestRecord struct {
	OrganizationID tracker.OrganizationID
	RunnerID       string
	Request        runnerauth.UpdateRequest
}

type UrgentUpdate struct {
	OrganizationID tracker.OrganizationID
	Revision       int64
	Request        *runnerauth.UpdateRequest
	UpdatedAt      time.Time
}
