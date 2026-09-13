// Package operations defines the portable operations report shared by the API and exports.
package operations

import "time"

type Report struct {
	DataTime       time.Time  `json:"data_time"`
	Instance       string     `json:"instance"`
	Stats          []Window   `json:"stats"`
	QueueDepth     int        `json:"queue_depth"`
	MergeGroupSize *int       `json:"merge_group_size"`
	MergeGroupWait *int64     `json:"merge_group_wait_seconds"`
	Actions        []Action   `json:"actions"`
	Decisions      []Decision `json:"decisions"`
}

type Window struct {
	Label                   string    `json:"label"`
	From                    time.Time `json:"from"`
	To                      time.Time `json:"to"`
	Merges                  int64     `json:"merges"`
	Closes                  int64     `json:"closes"`
	MergesPerDay            float64   `json:"merges_per_day"`
	ClosesPerDay            float64   `json:"closes_per_day"`
	CycleMedianSeconds      *float64  `json:"cycle_median_seconds"`
	CycleP75Seconds         *float64  `json:"cycle_p75_seconds"`
	CycleSamples            int       `json:"cycle_samples"`
	CleanAttemptRate        *float64  `json:"clean_attempt_rate"`
	TokensPerCompletedIssue *float64  `json:"tokens_per_completed_issue"`
	BlockedNights           []Night   `json:"blocked_nights"`
	Projects                []Project `json:"projects"`
}

type Night struct {
	Date   string `json:"date"`
	Issues int    `json:"issues"`
}

type Project struct {
	ID          string   `json:"id"`
	Dispatches  int64    `json:"dispatches"`
	SkipReasons []Reason `json:"skip_reasons"`
}

type Reason struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

type Action struct {
	ID          int64     `json:"id"`
	ProjectID   string    `json:"project_id"`
	Issue       string    `json:"issue"`
	Kind        string    `json:"kind"`
	Reason      string    `json:"reason"`
	EvidenceURL string    `json:"evidence_url"`
	At          time.Time `json:"at"`
}

type Decision struct {
	// WorkFingerprint is internal evidence used to exclude superseded questions.
	WorkFingerprint string `json:"-"`

	Kind         string        `json:"kind,omitempty"`
	ProjectID    string        `json:"project_id"`
	Issue        string        `json:"issue"`
	Title        string        `json:"title,omitempty"`
	Question     string        `json:"question"`
	URL          string        `json:"url"`
	Prerequisite *Prerequisite `json:"prerequisite,omitempty"`
}

type Prerequisite struct {
	Issue    string `json:"issue"`
	Title    string `json:"title,omitempty"`
	URL      string `json:"url,omitempty"`
	Evidence string `json:"evidence"`
}
