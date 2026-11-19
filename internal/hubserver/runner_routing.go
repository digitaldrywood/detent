package hubserver

import (
	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const releasedExecutionsPageSize = runnerfleet.ReleasedExecutionsPageSize

type releasedExecutionCursorKey struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	runner       string
}
