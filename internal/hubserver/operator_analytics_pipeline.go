package hubserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type pipelineStage struct {
	Stage    string                         `json:"stage"`
	Duration operatortool.AnalyticsDuration `json:"duration"`
	Executed int                            `json:"executed"`
	Reused   int                            `json:"reused"`
	Outcomes map[string]int                 `json:"outcomes"`
}

type pipelineReport struct {
	Stages  []pipelineStage `json:"stages"`
	Partial bool            `json:"partial"`
	Source  string          `json:"source"`
}

func summarizePipelineTimings(timings []gate.PipelineTiming, w operatortool.AnalyticsWindow) pipelineReport {
	out := pipelineReport{Stages: []pipelineStage{}, Source: "recorded_receipts_and_workflow_timestamps; intervals_clipped_to_window; unique_receipts; quantiles_linear_interpolation; reused_duration_excluded"}
	seen := map[string]bool{}
	stages := map[string]*pipelineStage{}
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
			stage = &pipelineStage{Stage: timing.Stage, Outcomes: map[string]int{}}
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
		stage.Duration = analyticsDuration(durations[name])
		out.Stages = append(out.Stages, *stage)
	}
	sort.Slice(out.Stages, func(i, j int) bool { return out.Stages[i].Stage < out.Stages[j].Stage })
	return out
}

func uniquePipelineTimings(timings []gate.PipelineTiming, w operatortool.AnalyticsWindow) []gate.PipelineTiming {
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

func residencePipelineTimings(timeline analyticsResidenceTimeline, attempts []nativeAnalyticsAttempt) []gate.PipelineTiming {
	out := []gate.PipelineTiming{}
	if timeline.issue.partial {
		return out
	}
	for _, span := range timeline.spans {
		if span.lane != "Merging" {
			continue
		}
		var claimed time.Time
		for _, event := range timeline.issue.events {
			if event.kind == "scheduler.decision" && !event.at.Before(span.from) && !event.at.After(span.to) {
				claimed = event.at
				break
			}
		}
		if !claimed.IsZero() {
			timing := gate.Interval("merging_queue", span.from, claimed, "")
			digest := sha256.Sum256([]byte(timing.ReceiptID + "\x00" + timeline.issue.id))
			timing.ReceiptID = "timing_" + hex.EncodeToString(digest[:16])
			out = append(out, timing)
		}
		for _, attempt := range attempts {
			if attempt.WorkItemID != timeline.issue.id || attempt.Landing == nil || !attempt.Landing.Landed {
				continue
			}
			landed := attempt.Landing.ObservedAt
			if landed.IsZero() || landed.Before(span.from) || landed.After(span.to) {
				continue
			}
			timing := gate.Interval("merging_to_landed", span.from, landed, "")
			digest := sha256.Sum256([]byte(timing.ReceiptID + "\x00" + timeline.issue.id))
			timing.ReceiptID = "timing_" + hex.EncodeToString(digest[:16])
			out = append(out, timing)
			break
		}
	}
	return out
}

func readPipelineBarriers(ctx context.Context, q nativeQueryer, scope nativeScope, out *nativeAnalyticsProject) error {
	rows, err := queryAnalyticsPopulation(ctx, q, "barriers", `SELECT response_json FROM native_commands WHERE organization_id=? AND operation LIKE '%/landing-barrier' AND json_extract(response_json,'$.project_id')=? AND julianday(created_at)>=julianday(?) AND julianday(created_at)<julianday(?) ORDER BY created_at,actor_id,command_key LIMIT ? OFFSET ?`, scope.organization, scope.project, formatHubTime(out.Window.From), formatHubTime(out.Window.To))
	if err != nil {
		return err
	}
	defer rows.Close()
	observed := 0
	for rows.Next() {
		if observed == maxAnalyticsPopulation {
			out.Pipeline.Partial = true
			break
		}
		observed++
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var barrier tracker.LandingBarrier
		if err := json.Unmarshal([]byte(raw), &barrier); err != nil {
			return err
		}
		if barrier.Result != nil && !barrier.Running {
			out.pipelineTimings = append(out.pipelineTimings, barrier.Result.PipelineTiming("barrier"))
			if barrier.Result.Pipeline != nil {
				out.pipelineTimings = append(out.pipelineTimings, barrier.Result.Pipeline.Timings...)
				out.Pipeline.Partial = out.Pipeline.Partial || barrier.Result.Pipeline.Dropped > 0
			}
		}
		if barrier.Running && !barrier.ClaimedAt.IsZero() && !barrier.ReadyAt.IsZero() {
			timing := gate.Interval("barrier_claim", barrier.ReadyAt, barrier.ClaimedAt, "")
			out.pipelineTimings = append(out.pipelineTimings, timing)
		}
	}
	return rows.Err()
}

func pipelineTimingPage(timings []gate.PipelineTiming, offset, limit int) operatortool.ReadPage[gate.PipelineTiming] {
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
