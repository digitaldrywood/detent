package reportstore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const RunnerCapacityFactLimit = 10000

type RunnerCapacityQuery struct {
	RunnerID  string
	MachineID tracker.MachineID
	From      time.Time
	To        time.Time
	Limit     int
}

type RunnerOccupancy struct {
	LeaseID string
	From    time.Time
	To      time.Time
}

type RunnerProcessUsage struct {
	AttemptID string
	StartedAt time.Time
	Usage     procgroup.Usage
}

type RunnerCapacityFacts struct {
	Occupancy        []RunnerOccupancy
	Runs             []RunnerProcessUsage
	EligibleRuns     int
	UnknownRuns      int
	OccupancyPartial bool
	RunsPartial      bool
}

type RunnerCapacityReader interface {
	ReadRunnerCapacityFacts(context.Context, RunnerCapacityQuery) (RunnerCapacityFacts, error)
}
