package hubserver

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releasedExecutionCursorKey struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
	runner       string
}
