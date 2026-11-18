package healthstore

import "time"

type Evidence struct {
	Productivity        *ProductivityEvidence    `json:"productivity,omitempty"`
	ScheduledValidation *ScheduledEvidence       `json:"scheduled_validation,omitempty"`
	EventIDs            []string                 `json:"event_ids,omitempty"`
	AttemptIDs          []string                 `json:"attempt_ids,omitempty"`
	WorkItemIDs         []string                 `json:"work_item_ids,omitempty"`
	Counts              map[string]int           `json:"counts,omitempty"`
	Values              map[string]float64       `json:"values,omitempty"`
	Queues              map[string]QueueEvidence `json:"queues,omitempty"`
	Signatures          []string                 `json:"signatures,omitempty"`
	Reason              string                   `json:"reason,omitempty"`
}

type ScheduledEvidence struct {
	LastReportAt   time.Time  `json:"last_report_at"`
	LastRunURL     string     `json:"last_run_url"`
	LastGreenAt    *time.Time `json:"last_green_at,omitempty"`
	CadenceSeconds int        `json:"cadence_seconds"`
	WindowSeconds  int        `json:"window_seconds"`
}

type ProductivityEvidence struct {
	WindowFrom         time.Time `json:"window_from"`
	WindowTo           time.Time `json:"window_to"`
	LatestFailureStage string    `json:"latest_failure_stage"`
}

type QueueEvidence struct {
	QueueDepth  int     `json:"queue_depth"`
	ShortP50    float64 `json:"todo_p50_seconds_1h"`
	LongP50     float64 `json:"todo_p50_seconds_6h"`
	BaselineP90 float64 `json:"baseline_p90_seconds"`
}
