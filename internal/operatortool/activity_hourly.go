package operatortool

import "time"

const MaxHourlyActivityWindow = 24 * time.Hour

type ActivityHourlyOutcome struct {
	EndingStage           string  `json:"ending_stage"`
	Outcome               string  `json:"outcome"`
	FailureReason         string  `json:"failure_reason"`
	Verdict               string  `json:"verdict"`
	VerdictSucceeded      *bool   `json:"verdict_succeeded"`
	Reason                string  `json:"reason"`
	NextStep              string  `json:"next_step"`
	Disposition           string  `json:"disposition"`
	Attempts              int64   `json:"attempts"`
	ElapsedAttemptSeconds float64 `json:"elapsed_attempt_seconds"`
}

type ActivityLandingHistory struct {
	LinkedAttempts  int64  `json:"linked_attempts"`
	HistoryCoverage string `json:"history_coverage"`
	Landings        int64  `json:"landings"`
}

type ActivityHour struct {
	Hour                  time.Time                `json:"hour"`
	TerminalAttempts      int64                    `json:"terminal_attempts"`
	ElapsedAttemptSeconds float64                  `json:"elapsed_attempt_seconds"`
	Outcomes              []ActivityHourlyOutcome  `json:"outcomes"`
	PendingVerdicts       int64                    `json:"pending_verdicts"`
	UnavailableVerdicts   int64                    `json:"unavailable_verdicts"`
	VerifiedLandings      int64                    `json:"verified_landings"`
	UnverifiedLandings    int64                    `json:"unverified_landings"`
	AcceptedNoChange      int64                    `json:"accepted_no_change"`
	UnknownNoChange       int64                    `json:"unknown_no_change"`
	LandingHistory        []ActivityLandingHistory `json:"landing_history"`
}

type ActivityAttemptReference struct {
	AttemptID  string    `json:"attempt_id"`
	WorkItemID string    `json:"work_item_id"`
	FinishedAt time.Time `json:"finished_at"`
}

type ActivityLandingReference struct {
	LandingID        string    `json:"landing_id"`
	ChangeID         string    `json:"change_id"`
	LandingAttemptID string    `json:"landing_attempt_id"`
	LandedAt         time.Time `json:"landed_at"`
	LinkedAttempts   int64     `json:"linked_attempts"`
	HistoryCoverage  string    `json:"history_coverage"`
}

type ActivityHourlyReport struct {
	UndatedLandingHistory bool                       `json:"undated_landing_history"`
	OrganizationID        string                     `json:"organization_id"`
	ProjectID             string                     `json:"project_id"`
	ObservedAt            time.Time                  `json:"observed_at"`
	Window                AnalyticsWindow            `json:"window"`
	Hours                 []ActivityHour             `json:"hours"`
	AttemptsPerLanding    *float64                   `json:"attempts_per_landing"`
	Attempts              []ActivityAttemptReference `json:"attempts"`
	Landings              []ActivityLandingReference `json:"landings"`
	MoreAttempts          bool                       `json:"more_attempts"`
	MoreLandings          bool                       `json:"more_landings"`
}
