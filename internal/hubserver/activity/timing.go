package activity

import (
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func activityTypicalDurations(project string, attempts []activityDurationSample, partial bool) []operatortool.ActivityTypicalDuration {
	values := map[string][]float64{}
	for _, a := range attempts {
		if a.Role == "" || a.Status != "succeeded" && a.Status != "failed" {
			continue
		}
		if a.Seconds > 0 {
			values[a.Role] = append(values[a.Role], a.Seconds)
		}
	}
	out := []operatortool.ActivityTypicalDuration{}
	for role, seconds := range values {
		out = append(out, operatortool.ActivityTypicalDuration{ProjectID: project, Stage: role, AnalyticsDuration: reportdata.Duration(seconds), Partial: partial})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Stage < out[j].Stage })
	return out
}

func activityStageTiming(a *operatortool.ActivityAttempt, phases []tracker.NativePhase, now time.Time, running bool) {
	if len(phases) > 0 {
		start := phases[len(phases)-1].StartedAt
		if !start.IsZero() && !start.After(now) {
			a.StageStartedAt = &start
			if running {
				seconds := now.Sub(start).Seconds()
				a.StageElapsedSeconds = &seconds
			}
		}
	}
	if !running {
		if seconds := reportdata.ClosedAttemptSeconds(phases); seconds > 0 {
			a.StageDurationSeconds = &seconds
		}
	}
	if a.Stage == "" || a.StageStartedAt == nil || !running && a.StageDurationSeconds == nil {
		a.Partial = true
	}
}
