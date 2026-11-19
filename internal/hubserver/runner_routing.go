package hubserver

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

const releasedExecutionsPageSize = 100

type releasedExecutionCursorKey struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	runner       string
}
