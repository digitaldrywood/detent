package reportdata

import (
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

type ResidenceSample struct {
	ID          string
	Created     time.Time
	At          time.Time
	AtNS        int64
	Initial     string
	State       string
	Entered     time.Time
	Started     *time.Time
	Partial     bool
	Events      int
	Transitions int
	Lanes       map[string]float64
	Queue       float64
	QueueTo     *time.Time
	LastDone    *time.Time
	MergeClaim  *time.Time
	Sequence    int64
	LanesNS     map[string]int64
	QueueNS     int64
	Causes      map[string]operatortool.AnalyticsReworkCause
}
