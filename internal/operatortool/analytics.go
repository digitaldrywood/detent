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
