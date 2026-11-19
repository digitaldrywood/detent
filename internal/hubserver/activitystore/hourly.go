package activitystore

import (
	"context"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type HourlyFilter struct {
	Project   tracker.ProjectID
	Principal string
	From, To  time.Time
	Limit     int
}

type HourlyOutcome struct {
	Hour                                   time.Time
	EndingStage, Outcome, FailureReason    string
	Verdict, Reason, NextStep, Disposition string
	VerdictSucceeded                       *bool
	Attempts, AcceptedNoChange             int64
	UnknownNoChange                        int64
	Seconds                                float64
}

type AttemptReference struct {
	ID, WorkItemID string
	FinishedAt     time.Time
}

type LandingCohort struct {
	ID, ChangeID, AttemptID string
	LandedAt                time.Time
	LinkedAttempts          int64
	HistoryCoverage         string
}

type HourlyFacts struct {
	Outcomes []HourlyOutcome
	Landings []LandingCohort
	Attempts []AttemptReference
}

type HourlyReader interface {
	ActivityHourly(context.Context, HourlyFilter) (HourlyFacts, error)
}
