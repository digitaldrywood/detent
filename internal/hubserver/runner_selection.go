package hubserver

import (
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerSelectionCursor struct {
	ID                       tracker.WorkItemID
	Priority, Aging, Landing int
	Created, Identifier      string
}
