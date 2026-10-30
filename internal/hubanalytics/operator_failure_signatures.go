package hubanalytics

import (
	"time"
)

type FailureSignature struct {
	Signature        string    `json:"signature"`
	Example          string    `json:"example"`
	Class            string    `json:"class"`
	Count            int       `json:"count"`
	WorkItems        []string  `json:"work_items"`
	WorkItemsCount   int       `json:"work_items_count"`
	WorkItemsPartial bool      `json:"work_items_partial"`
	FirstSeen        time.Time `json:"first_seen"`
	LastSeen         time.Time `json:"last_seen"`
	Cost             float64   `json:"cost_usd"`
	Tokens           int64     `json:"tokens"`
	SlotMinutes      float64   `json:"slot_minutes"`
}

type Failure struct {
	AttemptID string    `json:"-"`
	WorkItem  string    `json:"-"`
	At        time.Time `json:"-"`
	Started   time.Time `json:"-"`
	Finished  time.Time `json:"-"`
	Example   string    `json:"-"`
	Class     string    `json:"-"`
	Cost      float64   `json:"-"`
	Tokens    int64     `json:"-"`
}
