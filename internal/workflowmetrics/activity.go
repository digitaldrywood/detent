package workflowmetrics

import (
	"encoding/json"
	"sort"
	"time"
)

const PhaseTypeAgentActivity PhaseType = "agent_activity"

// InstructionRef identifies a source without retaining private instruction text.
type InstructionRef struct {
	PathRef     string    `json:"path_ref,omitempty"`
	Evidence    string    `json:"evidence,omitempty"`
	Name        string    `json:"name"`
	Hash        string    `json:"sha256"`
	Version     string    `json:"version,omitempty"`
	MatchedLine int       `json:"matched_line,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
}

// ActivityAction is untimed evidence within a tool interval. Native metadata
// does not supply per-action timing, outcomes or causal instruction origins.
type ActivityAction struct {
	Index             int              `json:"index"`
	Type              string           `json:"type"`
	TypeRef           string           `json:"type_ref,omitempty"`
	NameRef           string           `json:"name_ref,omitempty"`
	PathRef           string           `json:"path_ref,omitempty"`
	Fingerprint       string           `json:"fingerprint"`
	Kind              string           `json:"kind"`
	Evidence          string           `json:"evidence"`
	Attribution       string           `json:"attribution"`
	CausalAttribution string           `json:"causal_attribution"`
	Sources           []InstructionRef `json:"sources,omitempty"`
	SourceCoverage    string           `json:"source_coverage,omitempty"`
	Repeat            int              `json:"repeat"`
}

type ActivitySpan struct {
	ID                string           `json:"id"`
	ParentID          string           `json:"parent_id"`
	Kind              string           `json:"kind"`
	Evidence          string           `json:"evidence"`
	Fingerprint       string           `json:"fingerprint,omitempty"`
	Head              string           `json:"head,omitempty"`
	HeadObservedAt    time.Time        `json:"head_observed_at,omitzero"`
	HeadAttribution   string           `json:"head_attribution,omitempty"`
	Attribution       string           `json:"attribution"`
	Sources           []InstructionRef `json:"sources,omitempty"`
	Actions           []ActivityAction `json:"actions,omitempty"`
	ActionsDropped    int              `json:"actions_dropped,omitempty"`
	CausalAttribution string           `json:"causal_attribution,omitempty"`
	StartedAt         time.Time        `json:"started_at"`
	FinishedAt        time.Time        `json:"finished_at,omitzero"`
	Outcome           string           `json:"outcome"`
	ExitCode          *int             `json:"exit_code,omitempty"`
	Repeat            int              `json:"repeat"`
	ElapsedSeconds    float64          `json:"elapsed_seconds,omitempty"`
	PendingSeconds    float64          `json:"pending_seconds,omitempty"`
	WaitReason        string           `json:"wait_reason,omitempty"`
}

// ActivityProfile is a bounded checkpoint of received lifecycle events. AsOf is
// the end of observable coverage, not a claim that silence is model reasoning.
type ActivityProfile struct {
	Schema             int              `json:"schema"`
	Instance           string           `json:"instance"`
	AttemptID          int64            `json:"attempt_id"`
	AttemptRef         string           `json:"attempt_ref"`
	Generation         uint64           `json:"generation"`
	SessionID          int64            `json:"session_id"`
	ProviderThreadRef  string           `json:"provider_thread_ref,omitempty"`
	ProviderSessionRef string           `json:"provider_session_ref,omitempty"`
	Stage              string           `json:"stage"`
	StartedAt          time.Time        `json:"started_at"`
	AsOf               time.Time        `json:"as_of"`
	FinishedAt         time.Time        `json:"finished_at,omitzero"`
	Status             string           `json:"status"`
	Coverage           string           `json:"coverage"`
	CoverageNotes      []string         `json:"coverage_notes,omitempty"`
	Dropped            uint64           `json:"dropped_events"`
	Unpaired           uint64           `json:"unpaired_events"`
	DetailOmitted      uint64           `json:"detail_omitted,omitempty"`
	ProjectionOmitted  uint64           `json:"projection_omitted,omitempty"`
	Summary            *ActivitySummary `json:"timing_summary,omitempty"`
	Sources            []InstructionRef `json:"instructions"`
	Spans              []ActivitySpan   `json:"spans"`
}

type ActivitySummary struct {
	Through    time.Time         `json:"through"`
	DetailFrom time.Time         `json:"detail_from"`
	Breakdown  ActivityBreakdown `json:"breakdown"`
}

type ActivityBreakdown struct {
	ElapsedSeconds    float64            `json:"elapsed_seconds"`
	ObservedSeconds   float64            `json:"observed_seconds"`
	UnknownSeconds    float64            `json:"unknown_seconds"`
	ConcurrentSeconds float64            `json:"concurrent_seconds"`
	ByKind            map[string]float64 `json:"by_kind_seconds"`
}

type ActivityGap struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

type ActivityAudit struct {
	Gaps                  []ActivityGap     `json:"unobserved_intervals"`
	Profile               ActivityProfile   `json:"profile"`
	Breakdown             ActivityBreakdown `json:"breakdown"`
	UnobservedTailSeconds float64           `json:"unobserved_tail_seconds"`
}

func ActivityAudits(events []PhaseEvent, now time.Time) []ActivityAudit {
	out := make([]ActivityAudit, 0)
	for _, event := range events {
		if event.PhaseType != PhaseTypeAgentActivity {
			continue
		}
		var p ActivityProfile
		if json.Unmarshal([]byte(event.MetadataJSON), &p) != nil || p.Schema != 1 {
			continue
		}
		audit := ActivityAudit{Profile: p, Breakdown: p.Breakdown(), Gaps: p.Gaps()}
		for i := range audit.Profile.Spans {
			span := &audit.Profile.Spans[i]
			if span.FinishedAt.IsZero() {
				span.PendingSeconds = max(0, p.AsOf.Sub(span.StartedAt).Seconds())
			} else {
				span.ElapsedSeconds = max(0, span.FinishedAt.Sub(span.StartedAt).Seconds())
			}
		}
		if p.FinishedAt.IsZero() {
			audit.UnobservedTailSeconds = max(0, now.Sub(p.AsOf).Seconds())
		}
		out = append(out, audit)
	}
	return out
}

// Breakdown partitions wall time: concurrent intervals are counted once in
// their own bucket. It never sums nested or overlapping tool durations.
func (p ActivityProfile) Breakdown() ActivityBreakdown {
	end := p.AsOf
	if !p.FinishedAt.IsZero() {
		end = p.FinishedAt
	}
	start := p.StartedAt
	if p.Summary != nil && p.Summary.Through.After(start) {
		start = p.Summary.Through
	}
	b := ActivityBreakdown{ElapsedSeconds: max(0, end.Sub(p.StartedAt).Seconds()), ByKind: make(map[string]float64)}
	if p.Summary != nil {
		b.ObservedSeconds = p.Summary.Breakdown.ObservedSeconds
		b.ConcurrentSeconds = p.Summary.Breakdown.ConcurrentSeconds
		for kind, seconds := range p.Summary.Breakdown.ByKind {
			if kind != "unobserved" {
				b.ByKind[kind] = seconds
			}
		}
	}
	type boundary struct {
		at    time.Time
		span  ActivitySpan
		delta int
	}
	points := make([]boundary, 0, len(p.Spans)*2)
	for _, span := range p.Spans {
		spanStart := span.StartedAt
		finish := span.FinishedAt
		// An unmatched start does not prove the tool ran until the checkpoint.
		if finish.IsZero() || span.Outcome == "unobserved" {
			continue
		}
		if spanStart.Before(start) {
			spanStart = start
		}
		if finish.After(end) {
			finish = end
		}
		if !finish.After(spanStart) {
			continue
		}
		points = append(points, boundary{spanStart, span, 1}, boundary{finish, span, -1})
	}
	sort.Slice(points, func(i, j int) bool { return points[i].at.Before(points[j].at) })
	active := make(map[string]ActivitySpan)
	previous := start
	for _, point := range points {
		seconds := max(0, point.at.Sub(previous).Seconds())
		parents := make(map[string]bool)
		for _, span := range active {
			parents[span.ParentID] = true
		}
		count, kind := 0, ""
		for id, span := range active {
			if !parents[id] {
				count++
				kind = span.Kind
			}
		}
		if count > 0 {
			b.ObservedSeconds += seconds
			if count > 1 {
				kind = "concurrent"
				b.ConcurrentSeconds += seconds
			}
			b.ByKind[kind] += seconds
		}
		if point.delta > 0 {
			active[point.span.ID] = point.span
		} else {
			delete(active, point.span.ID)
		}
		previous = point.at
	}
	b.UnknownSeconds = max(0, b.ElapsedSeconds-b.ObservedSeconds)
	b.ByKind["unobserved"] = b.UnknownSeconds
	return b
}

func (p *ActivityProfile) SummarizeThrough(through time.Time) {
	if through.Before(p.StartedAt) || p.Summary != nil && !through.After(p.Summary.Through) {
		return
	}
	prefix := *p
	prefix.AsOf, prefix.FinishedAt = through, time.Time{}
	p.Summary = &ActivitySummary{Through: through, DetailFrom: through, Breakdown: prefix.Breakdown()}
}

// Gaps gives the complement of confirmed tool intervals. Pending tools do not
// manufacture observed duration; their starts and pending age remain in spans.
func (p ActivityProfile) Gaps() []ActivityGap {
	end := p.AsOf
	if !p.FinishedAt.IsZero() {
		end = p.FinishedAt
	}
	startAt := p.StartedAt
	if p.Summary != nil && p.Summary.DetailFrom.After(startAt) {
		startAt = p.Summary.DetailFrom
	}
	intervals := make([]ActivityGap, 0, len(p.Spans))
	for _, span := range p.Spans {
		if span.FinishedAt.IsZero() || span.Outcome == "unobserved" {
			continue
		}
		start, finish := span.StartedAt, span.FinishedAt
		if start.Before(startAt) {
			start = startAt
		}
		if finish.After(end) {
			finish = end
		}
		if finish.After(start) {
			intervals = append(intervals, ActivityGap{start, finish})
		}
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].StartedAt.Before(intervals[j].StartedAt) })
	gaps := make([]ActivityGap, 0)
	cursor := startAt
	for _, interval := range intervals {
		if interval.StartedAt.After(cursor) {
			gaps = append(gaps, ActivityGap{cursor, interval.StartedAt})
		}
		if interval.FinishedAt.After(cursor) {
			cursor = interval.FinishedAt
		}
	}
	if end.After(cursor) {
		gaps = append(gaps, ActivityGap{cursor, end})
	}
	return gaps
}
