package hubanalytics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type PipelineStage struct {
	Stage    string                         `json:"stage"`
	Duration operatortool.AnalyticsDuration `json:"duration"`
	Executed int                            `json:"executed"`
	Reused   int                            `json:"reused"`
	Outcomes map[string]int                 `json:"outcomes"`
}

type PipelineReport struct {
	Stages  []PipelineStage `json:"stages"`
	Partial bool            `json:"partial"`
	Source  string          `json:"source"`
}

func SummarizePipelineTimings(timings []gate.PipelineTiming, w operatortool.AnalyticsWindow) PipelineReport {
	out := PipelineReport{Stages: []PipelineStage{}, Source: "recorded_receipts_and_workflow_timestamps; intervals_clipped_to_window; unique_receipts; quantiles_linear_interpolation; reused_duration_excluded"}
	seen := map[string]bool{}
	stages := map[string]*PipelineStage{}
	durations := map[string][]float64{}
	for _, timing := range timings {
		if timing.Execution != "" && (timing.HeadSHA == "" || timing.TreeSHA == "") {
			out.Partial = true
		}
		if timing.StartedAt.IsZero() || timing.FinishedAt.Before(timing.StartedAt) {
			out.Partial = true
			continue
		}
		if !timing.StartedAt.Before(w.To) || timing.FinishedAt.Before(w.From) || seen[timing.ReceiptID] {
			continue
		}
		seen[timing.ReceiptID] = true
		stage := stages[timing.Stage]
		if stage == nil {
			stage = &PipelineStage{Stage: timing.Stage, Outcomes: map[string]int{}}
			stages[timing.Stage] = stage
		}
		if timing.Execution == "reused" {
			stage.Reused++
			continue
		}
		if timing.Execution == "executed" {
			stage.Executed++
		}
		if timing.Outcome != "" {
			stage.Outcomes[timing.Outcome]++
		}
		seconds := max(0, minTime(timing.FinishedAt, w.To).Sub(maxTime(timing.StartedAt, w.From)).Seconds())
		durations[timing.Stage] = append(durations[timing.Stage], seconds)
	}
	for name, stage := range stages {
		stage.Duration = Duration(durations[name])
		out.Stages = append(out.Stages, *stage)
	}
	sort.Slice(out.Stages, func(i, j int) bool { return out.Stages[i].Stage < out.Stages[j].Stage })
	return out
}

func UniquePipelineTimings(timings []gate.PipelineTiming, w operatortool.AnalyticsWindow) []gate.PipelineTiming {
	seen := map[string]bool{}
	out := []gate.PipelineTiming{}
	for _, timing := range timings {
		if timing.StartedAt.IsZero() || !timing.StartedAt.Before(w.To) || timing.FinishedAt.Before(w.From) || seen[timing.ReceiptID] {
			continue
		}
		seen[timing.ReceiptID] = true
		out = append(out, timing.Public())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].StartedAt.Before(out[j].StartedAt)
		}
		return out[i].ReceiptID < out[j].ReceiptID
	})
	return out
}

func ResidencePipelineTimings(timeline ResidenceTimeline, attempts []Attempt) []gate.PipelineTiming {
	out := []gate.PipelineTiming{}
	if timeline.Issue.Partial {
		return out
	}
	for _, span := range timeline.Spans {
		if span.Lane != "Merging" {
			continue
		}
		var claimed time.Time
		for _, event := range timeline.Issue.Events {
			if event.Kind == "scheduler.decision" && !event.At.Before(span.From) && !event.At.After(span.To) {
				claimed = event.At
				break
			}
		}
		if !claimed.IsZero() {
			timing := gate.Interval("merging_queue", span.From, claimed, "")
			digest := sha256.Sum256([]byte(timing.ReceiptID + "\x00" + timeline.Issue.ID))
			timing.ReceiptID = "timing_" + hex.EncodeToString(digest[:16])
			out = append(out, timing)
		}
		for _, attempt := range attempts {
			if attempt.WorkItemID != timeline.Issue.ID || attempt.Landing == nil || !attempt.Landing.Landed {
				continue
			}
			landed := attempt.Landing.ObservedAt
			if landed.IsZero() || landed.Before(span.From) || landed.After(span.To) {
				continue
			}
			timing := gate.Interval("merging_to_landed", span.From, landed, "")
			digest := sha256.Sum256([]byte(timing.ReceiptID + "\x00" + timeline.Issue.ID))
			timing.ReceiptID = "timing_" + hex.EncodeToString(digest[:16])
			out = append(out, timing)
			break
		}
	}
	return out
}

func PipelineTimingPage(timings []gate.PipelineTiming, offset, limit int) operatortool.ReadPage[gate.PipelineTiming] {
	page := operatortool.OffsetPage(timings, offset, limit)
	size := 2
	for i, timing := range page.Items {
		raw, err := json.Marshal(timing)
		if err != nil {
			return page
		}
		if i > 0 && size+len(raw)+1 > operatortool.WorkListPageBytes {
			page.Items = page.Items[:i]
			next := offset + i
			page.NextOffset = &next
			break
		}
		size += len(raw) + 1
	}
	return page
}
