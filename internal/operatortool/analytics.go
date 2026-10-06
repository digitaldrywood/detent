package operatortool

import (
	"encoding/json"
	"time"
)

type AnalyticsRequest struct {
	ProjectID string `json:"project_id,omitempty"`
	Limit     int    `json:"limit,omitempty"`
	Offset    int    `json:"offset,omitempty"`
	RowOffset int    `json:"row_offset,omitempty"`
	Window    string `json:"window,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Timezone  string `json:"tz,omitempty"`
}

type AnalyticsWindow struct {
	From   time.Time     `json:"from"`
	To     time.Time     `json:"to"`
	Bucket time.Duration `json:"bucket_ns"`
}

type AnalyticsDuration struct {
	Count      int     `json:"count"`
	Seconds    float64 `json:"seconds"`
	P50Seconds float64 `json:"p50_seconds"`
	P90Seconds float64 `json:"p90_seconds"`
}

type AnalyticsLaneDuration struct {
	Lane  string `json:"lane"`
	Group string `json:"group"`
	AnalyticsDuration
}

type AnalyticsReworkCause struct {
	FromState    string `json:"from_state"`
	ReasonDetail string `json:"reason_detail"`
	Count        int    `json:"count"`
}

type AnalyticsResidenceSummary struct {
	Lanes        []AnalyticsLaneDuration `json:"lanes"`
	SystemTotal  AnalyticsDuration       `json:"system_total"`
	HeldTotal    AnalyticsDuration       `json:"held_total"`
	LeadTotal    AnalyticsDuration       `json:"lead_total"`
	QueueTotal   AnalyticsDuration       `json:"queue_total"`
	ReworkCauses []AnalyticsReworkCause  `json:"rework_causes"`
	Partial      bool                    `json:"partial"`
}

type AnalyticsIssueResidence struct {
	WorkItemID      string             `json:"work_item_id"`
	SystemStartedAt *time.Time         `json:"system_started_at,omitempty"`
	LaneSeconds     map[string]float64 `json:"lane_seconds"`
	SystemSeconds   float64            `json:"system_seconds"`
	HeldSeconds     float64            `json:"held_seconds"`
	QueueSeconds    float64            `json:"queue_seconds"`
	LeadSeconds     *float64           `json:"lead_seconds,omitempty"`
	Partial         bool               `json:"partial"`
}

type AnalyticsAging struct {
	WorkItemID   string    `json:"work_item_id"`
	Lane         string    `json:"lane"`
	EnteredAt    time.Time `json:"entered_at"`
	Hours        float64   `json:"hours"`
	LaneP90Hours *float64  `json:"lane_p90_hours,omitempty"`
}

type AnalyticsQueue struct {
	Source   string            `json:"source"`
	Coverage string            `json:"coverage"`
	Seconds  float64           `json:"seconds"`
	Duration AnalyticsDuration `json:"duration"`
	Partial  bool              `json:"partial"`
}

type AnalyticsResidence struct {
	AnalyticsResidenceSummary
	Source         string                            `json:"source"`
	Coverage       string                            `json:"coverage"`
	IssuesObserved int                               `json:"issues_observed"`
	EventsObserved int                               `json:"events_observed"`
	Issues         ReadPage[AnalyticsIssueResidence] `json:"issues"`
	Aging          ReadPage[AnalyticsAging]          `json:"aging"`
}

func DecodeAnalytics(name string, raw json.RawMessage, now time.Time) (AnalyticsRequest, AnalyticsWindow, error) {
	r := AnalyticsRequest{}
	w := AnalyticsWindow{From: now.Add(-7 * 24 * time.Hour), To: now, Bucket: 24 * time.Hour}
	if ValidateFleetArguments(name, raw) != nil || DecodeArguments(raw, &r) != nil {
		return r, w, ErrInvalidArguments
	}
	if r.Limit == 0 {
		r.Limit = DefaultItemLimit
	}
	var err error
	if name == TimeSeries {
		duration := 10 * time.Minute
		w.Bucket = time.Minute
		if r.Window != "" {
			duration, err = time.ParseDuration(r.Window)
		}
		if err != nil || duration <= 0 || duration > 24*time.Hour {
			return r, w, ErrInvalidArguments
		}
		w.From = now.Add(-duration)
	}
	for _, field := range []struct {
		value  string
		target *time.Time
	}{{r.From, &w.From}, {r.To, &w.To}} {
		if field.value == "" {
			continue
		}
		*field.target, err = time.Parse(time.RFC3339Nano, field.value)
		if err != nil {
			*field.target, err = time.Parse(time.DateOnly, field.value)
		}
		if err != nil {
			return r, w, ErrInvalidArguments
		}
	}
	if r.Bucket != "" {
		w.Bucket, err = time.ParseDuration(r.Bucket)
	}
	if err != nil || w.Bucket <= 0 || w.Bucket > w.To.Sub(w.From) || !w.From.Before(w.To) || w.To.After(now) || w.To.Sub(w.From) > 90*24*time.Hour || (w.To.Sub(w.From)-1)/w.Bucket+1 > 240 {
		return r, w, ErrInvalidArguments
	}
	if name == TimeSeries && w.To.Sub(w.From)/w.Bucket+1 > 240 {
		return r, w, ErrInvalidArguments
	}
	if name == Reports && w.Bucket < time.Hour {
		return r, w, ErrInvalidArguments
	}
	if r.Timezone != "" {
		if _, err := time.LoadLocation(r.Timezone); err != nil {
			return r, w, ErrInvalidArguments
		}
	}
	return r, w, nil
}
