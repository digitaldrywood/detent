package operatortool

import (
	"encoding/json"
	"time"
)

type ActivityRequest struct {
	ProjectID string `json:"project_id,omitempty"`
	RunnerID  string `json:"runner_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
}

type ActivityAttempt struct {
	WorkItemID           string     `json:"work_item_id"`
	Number               int64      `json:"number"`
	Title                string     `json:"title"`
	ProjectID            string     `json:"project_id"`
	ProjectName          string     `json:"project_name"`
	RunnerID             string     `json:"runner_id"`
	AttemptID            string     `json:"attempt_id"`
	Stage                string     `json:"stage,omitempty"`
	Phase                string     `json:"phase,omitempty"`
	StageStartedAt       *time.Time `json:"stage_started_at,omitempty"`
	StageElapsedSeconds  *float64   `json:"stage_elapsed_seconds,omitempty"`
	StartedAt            time.Time  `json:"started_at"`
	SessionID            string     `json:"session_id,omitempty"`
	WorkspaceIDs         []string   `json:"workspace_ids"`
	ChangeID             string     `json:"change_id,omitempty"`
	PullRequestURL       string     `json:"pull_request_url,omitempty"`
	Outcome              string     `json:"outcome,omitempty"`
	FinishedAt           *time.Time `json:"finished_at,omitempty"`
	StageDurationSeconds *float64   `json:"stage_duration_seconds,omitempty"`
	Partial              bool       `json:"partial"`
}

type ActivityTypicalDuration struct {
	ProjectID string `json:"project_id"`
	Stage     string `json:"stage"`
	AnalyticsDuration
	Partial bool `json:"partial"`
}

type ActivityReport struct {
	OrganizationID   string                    `json:"organization_id"`
	ObservedAt       time.Time                 `json:"observed_at"`
	Window           AnalyticsWindow           `json:"window"`
	Running          []ActivityAttempt         `json:"running"`
	Finished         []ActivityAttempt         `json:"finished"`
	TypicalDurations []ActivityTypicalDuration `json:"typical_durations"`
	PopulationLimit  int                       `json:"population_limit"`
	Partial          bool                      `json:"partial"`
}

func DecodeActivity(raw json.RawMessage, now time.Time) (ActivityRequest, AnalyticsWindow, error) {
	r := ActivityRequest{}
	w := AnalyticsWindow{From: now.Add(-7 * 24 * time.Hour), To: now, Bucket: 24 * time.Hour}
	if ValidateFleetArguments(Activity, raw) != nil || DecodeArguments(raw, &r) != nil {
		return r, w, ErrInvalidArguments
	}
	if r.Limit == 0 {
		r.Limit = DefaultItemLimit
	}
	for _, field := range []struct {
		value  string
		target *time.Time
	}{{r.From, &w.From}, {r.To, &w.To}} {
		if field.value == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, field.value)
		if err != nil {
			at, err = time.Parse(time.DateOnly, field.value)
		}
		if err != nil {
			return r, w, ErrInvalidArguments
		}
		*field.target = at.UTC()
	}
	if !w.From.Before(w.To) || w.To.After(now) || w.To.Sub(w.From) > 90*24*time.Hour {
		return r, w, ErrInvalidArguments
	}
	return r, w, nil
}
