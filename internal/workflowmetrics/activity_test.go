package workflowmetrics

import (
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestActivityBreakdownPartitionsObservedWallTime(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	span := func(id, parent, kind string, start, end int) ActivitySpan {
		return ActivitySpan{ID: id, ParentID: parent, Kind: kind, StartedAt: at.Add(time.Duration(start) * time.Second), FinishedAt: at.Add(time.Duration(end) * time.Second), Outcome: "completed"}
	}
	for _, tt := range []struct {
		name                          string
		spans                         []ActivitySpan
		observed, concurrent, waiting float64
	}{
		{"overlap", []ActivitySpan{span("a", "turn", "local_validation", 2, 8), span("b", "turn", "review", 4, 10)}, 8, 4, 0},
		{"nested wait", []ActivitySpan{span("a", "turn", "local_validation", 2, 10), span("wait", "a", "waiting", 3, 6)}, 8, 0, 3},
		{"missing completion", []ActivitySpan{{ID: "a", Kind: "local_validation", StartedAt: at.Add(time.Second), Outcome: "running"}}, 0, 0, 0},
		{"out of order", []ActivitySpan{span("a", "turn", "review", 8, 2)}, 0, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			profile := ActivityProfile{StartedAt: at, AsOf: at.Add(12 * time.Second), Spans: tt.spans}
			b := profile.Breakdown()
			if b.ObservedSeconds != tt.observed || b.UnknownSeconds != 12-tt.observed || b.ConcurrentSeconds != tt.concurrent || b.ByKind["waiting"] != tt.waiting {
				t.Fatalf("breakdown = %+v", b)
			}
			gapSeconds := 0.0
			for _, gap := range profile.Gaps() {
				gapSeconds += gap.FinishedAt.Sub(gap.StartedAt).Seconds()
			}
			if gapSeconds != b.UnknownSeconds {
				t.Fatalf("gaps=%v, unknown=%v", gapSeconds, b.UnknownSeconds)
			}
			sum := 0.0
			for _, v := range b.ByKind {
				sum += v
			}
			if sum != 12 {
				t.Fatalf("partition sums to %v", sum)
			}
			profile.SummarizeThrough(at.Add(6 * time.Second))
			if !reflect.DeepEqual(profile.Breakdown(), b) {
				t.Fatalf("compaction changed overlapping or nested timing: before=%+v after=%+v", b, profile.Breakdown())
			}
		})
	}
}

func TestPublicActivityProfileBoundsAndRedaction(t *testing.T) {
	at := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	p := ActivityProfile{Schema: 1, AttemptID: 168, Generation: 27, Status: "running", Stage: "implementation", StartedAt: at, AsOf: at.Add(1024 * time.Second), Instance: "/private/instance", CoverageNotes: []string{"private instruction text"}, Dropped: 2, Unpaired: 3, ProviderThreadRef: "private thread"}
	for i := range 1024 {
		span := ActivitySpan{ID: "private shell command/" + strconv.Itoa(i), ParentID: "private parent", Kind: "implementation", Evidence: "edit_tool", Attribution: "inferred_text_match", CausalAttribution: "private origin", StartedAt: at.Add(time.Duration(i) * time.Second), FinishedAt: at.Add(time.Duration(i+1) * time.Second), Outcome: "completed", Sources: []InstructionRef{{Name: "/private/instructions", Hash: "private contents", PathRef: "/private/path"}}}
		for range 32 {
			span.Actions = append(span.Actions, ActivityAction{Type: "read", Fingerprint: strings.Repeat("a", 64), NameRef: "private name", PathRef: "/private/path", Evidence: "native_read", Kind: "context_read", CausalAttribution: "private cause"})
		}
		p.Spans = append(p.Spans, span)
	}
	public := PublicActivityProfile(p)
	raw, err := json.Marshal(public)
	if err != nil || len(raw) > 128*1024 || len(public.Spans) == 0 || public.Dropped != p.Dropped || public.ProjectionOmitted != uint64(len(p.Spans)-len(public.Spans)) || public.Unpaired != 3 || public.Coverage != "partial" {
		t.Fatalf("public profile: bytes=%d spans=%d dropped=%d err=%v", len(raw), len(public.Spans), public.Dropped, err)
	}
	if strings.Contains(string(raw), "private") {
		t.Fatalf("private data survived public projection: %s", raw)
	}
	if p.Spans[0].ID != "private shell command/0" || p.Instance != "/private/instance" {
		t.Fatal("projection changed the local recorder")
	}
	if public.Spans[len(public.Spans)-1].FinishedAt != p.AsOf || !reflect.DeepEqual(public.Breakdown(), p.Breakdown()) || public.Breakdown().ObservedSeconds != 1024 {
		t.Fatalf("projection lost recent work or whole-attempt timing: %+v", public.Breakdown())
	}
	second, err := json.Marshal(PublicActivityProfile(public))
	if err != nil || string(raw) != string(second) {
		t.Fatal("public projection is not stable across producer and owner")
	}
	t.Run("hourly summary respects byte bound", func(t *testing.T) {
		large := ActivityProfile{Schema: 1, StartedAt: at, AsOf: at.Add(ActivityHourLimit * time.Hour)}
		large.Summary = &ActivitySummary{Through: large.AsOf, DetailFrom: large.AsOf, Breakdown: ActivityBreakdown{ByKind: make(map[string]float64)}}
		kinds := []string{"context_read", "implementation", "waiting", "local_validation", "rebase", "merge", "review", "tool_execution", "unclassified", "unknown", "concurrent", "unobserved"}
		for i := range ActivityHourLimit {
			from, to := at.Add(time.Duration(i)*time.Hour+123456789*time.Nanosecond), at.Add(time.Duration(i+1)*time.Hour)
			elapsed := to.Sub(from).Seconds()
			seconds := elapsed / float64(len(kinds)+1)
			unknown := elapsed - seconds*float64(len(kinds)-1)
			b := ActivityBreakdown{ElapsedSeconds: elapsed, ObservedSeconds: elapsed - unknown, UnknownSeconds: unknown, ConcurrentSeconds: seconds, ByKind: make(map[string]float64)}
			for _, kind := range kinds {
				b.ByKind[kind] = seconds
			}
			b.ByKind["unobserved"] = unknown
			large.Summary.Breakdown = addActivityBreakdowns(large.Summary.Breakdown, b)
			large.Summary.Hourly = append(large.Summary.Hourly, ActivityHour{From: from, To: to, Breakdown: b})
		}
		gap := large.AsOf.Sub(large.StartedAt).Seconds() - large.Summary.Breakdown.ElapsedSeconds
		large.Summary.Breakdown.ElapsedSeconds += gap
		large.Summary.Breakdown.UnknownSeconds += gap
		large.Summary.Breakdown.ByKind["unobserved"] += gap
		for range 64 {
			large.Sources = append(large.Sources, InstructionRef{Name: "AGENTS.md (effective)", Hash: strings.Repeat("a", 64), PathRef: strings.Repeat("b", 64), Version: strings.Repeat("c", 64), Evidence: "recorder_snapshot_after_native_read", MatchedLine: 10000, ObservedAt: at.Add(123456789 * time.Nanosecond)})
		}
		bounded := PublicActivityProfile(large)
		raw, err := json.Marshal(bounded)
		if err != nil || len(raw) > 128*1024 || len(bounded.Summary.Hourly) >= ActivityHourLimit || !breakdownsClose(bounded.Breakdown(), large.Breakdown()) || len(large.Summary.Hourly) != ActivityHourLimit {
			t.Fatalf("hourly projection bytes=%d err=%v", len(raw), err)
		}
	})
}

func TestActivityHourlyTimingSurvivesCompaction(t *testing.T) {
	at := time.Date(2026, 9, 30, 23, 59, 58, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		duration time.Duration
		hours    int
	}{
		{name: "two hours", duration: 8 * time.Second, hours: 2},
		{name: "bounded history", duration: 200 * time.Hour, hours: ActivityHourLimit},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := ActivityProfile{StartedAt: at, AsOf: at.Add(tt.duration), Spans: []ActivitySpan{{ID: "tool", Kind: "local_validation", Outcome: "completed", StartedAt: at, FinishedAt: at.Add(4 * time.Second)}}}
			p.SummarizeThrough(at.Add(time.Second))
			p.SummarizeThrough(at.Add(3 * time.Second))
			p.SummarizeThrough(p.AsOf)
			p.Spans = nil
			if len(p.Summary.Hourly) != tt.hours || p.Breakdown().ObservedSeconds != 4 || p.Breakdown().ElapsedSeconds != tt.duration.Seconds() {
				t.Fatalf("compacted timing: %+v", p.Summary)
			}
			if tt.hours == 2 {
				first, missingFirst := p.BreakdownBetween(at.Truncate(time.Hour), at.Truncate(time.Hour).Add(time.Hour))
				second, missingSecond := p.BreakdownBetween(at.Truncate(time.Hour).Add(time.Hour), p.AsOf.Add(time.Hour))
				if first.ObservedSeconds != 2 || first.UnknownSeconds != 0 || second.ObservedSeconds != 2 || second.UnknownSeconds != 4 || missingFirst != 0 || missingSecond != 0 {
					t.Fatalf("hourly timing: first=%+v second=%+v missing=%v/%v", first, second, missingFirst, missingSecond)
				}
				original := p
				p.StartEarlier(at.Add(-time.Second))
				first, missingFirst = p.BreakdownBetween(at.Truncate(time.Hour), at.Truncate(time.Hour).Add(time.Hour))
				if first.ObservedSeconds != 2 || first.UnknownSeconds != 1 || missingFirst != 0 || original.Summary.Breakdown.ElapsedSeconds != 8 || original.Summary.Hourly[0].Breakdown.ElapsedSeconds != 2 {
					t.Fatalf("earlier boundary mutated or lost timing: %+v original=%+v", p.Summary, original.Summary)
				}
			} else {
				_, missing := p.BreakdownBetween(at, p.Summary.Hourly[0].From)
				if missing == 0 {
					t.Fatal("discarded hourly history was reported as observed")
				}
			}
		})
	}
}

func breakdownsClose(a, b ActivityBreakdown) bool {
	close := func(x, y float64) bool { return math.Abs(x-y) <= 1e-6 }
	if !close(a.ElapsedSeconds, b.ElapsedSeconds) || !close(a.ObservedSeconds, b.ObservedSeconds) || !close(a.UnknownSeconds, b.UnknownSeconds) || !close(a.ConcurrentSeconds, b.ConcurrentSeconds) || len(a.ByKind) != len(b.ByKind) {
		return false
	}
	for kind, seconds := range a.ByKind {
		if other, ok := b.ByKind[kind]; !ok || !close(seconds, other) {
			return false
		}
	}
	return true
}
