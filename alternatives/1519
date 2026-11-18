package activity

import (
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/agentidentity"
	"github.com/digitaldrywood/detent/internal/hubserver/reportdata"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type nativeAnalyticsAttempt = reportdata.AttemptTiming
type analyticsResidenceSample = reportdata.ResidenceSample

var reportsAttemptTotals = reportdata.AttemptTotals

func reportsPercent(v float64) *float64 { return &v }
func TestActivityStageTiming(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	phase := func(start, finish int) tracker.NativePhase {
		p := tracker.NativePhase{Name: "implement", StartedAt: base.Add(time.Duration(start) * time.Second)}
		if finish >= 0 {
			p.FinishedAt = base.Add(time.Duration(finish) * time.Second)
		}
		return p
	}
	for _, tt := range []struct {
		name                     string
		phases                   []tracker.NativePhase
		running                  bool
		start, elapsed, duration *float64
		partial                  bool
	}{
		{name: "latest open phase", phases: []tracker.NativePhase{phase(0, 10), phase(20, -1)}, running: true, start: reportsPercent(20), elapsed: reportsPercent(80)},
		{name: "last recorded start regardless of current name", phases: []tracker.NativePhase{phase(0, 10), phase(30, 40)}, running: true, start: reportsPercent(30), elapsed: reportsPercent(70)},
		{name: "sum closed intervals without gaps or open tail", phases: []tracker.NativePhase{phase(0, 10), phase(20, 40), phase(50, -1)}, start: reportsPercent(50), duration: reportsPercent(30)},
		{name: "negative interval contributes nothing", phases: []tracker.NativePhase{phase(10, 5), phase(20, 40)}, start: reportsPercent(20), duration: reportsPercent(20)},
		{name: "missing phases", running: true, partial: true},
		{name: "no closed data", phases: []tracker.NativePhase{phase(20, -1)}, start: reportsPercent(20), partial: true},
		{name: "future start unavailable", phases: []tracker.NativePhase{phase(101, -1)}, running: true, partial: true},
		{name: "zero last start does not fall back", phases: []tracker.NativePhase{phase(0, 10), {}}, running: true, partial: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := operatortool.ActivityAttempt{Stage: "code", Phase: "different"}
			activityStageTiming(&a, tt.phases, base.Add(100*time.Second), tt.running)
			var start *float64
			if a.StageStartedAt != nil {
				seconds := a.StageStartedAt.Sub(base).Seconds()
				start = &seconds
			}
			if !reflect.DeepEqual(start, tt.start) || !reflect.DeepEqual(a.StageElapsedSeconds, tt.elapsed) || !reflect.DeepEqual(a.StageDurationSeconds, tt.duration) || a.Partial != tt.partial {
				t.Fatalf("timing=%+v start=%v", a, start)
			}
		})
	}
}
func TestActivityTypicalDurations(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	makeAttempt := func(role, model, effort, status string, seconds int) nativeAnalyticsAttempt {
		return nativeAnalyticsAttempt{Identity: agentidentity.Configured("codex", "codex", "", role, model, "openai", effort, "", base), Status: status, Phases: []tracker.NativePhase{{StartedAt: base, FinishedAt: base.Add(time.Duration(seconds) * time.Second)}}}
	}
	population := []nativeAnalyticsAttempt{
		makeAttempt("code", "a", "high", "succeeded", 60), makeAttempt("code", "b", "medium", "failed", 180), makeAttempt("code", "c", "low", "succeeded", 300),
		makeAttempt("plan", "a", "high", "succeeded", 30), makeAttempt("code", "a", "high", "running", 900), makeAttempt("code", "a", "high", "interrupted", 900), makeAttempt("code", "a", "high", "cancelled", 900), makeAttempt("merge", "a", "high", "succeeded", 0),
	}
	for _, tt := range []struct {
		name     string
		attempts []nativeAnalyticsAttempt
		partial  bool
		want     []operatortool.ActivityTypicalDuration
	}{
		{name: "combined models and efforts", attempts: population, want: []operatortool.ActivityTypicalDuration{{ProjectID: "project", Stage: "code", AnalyticsDuration: operatortool.AnalyticsDuration{Count: 3, Seconds: 540, P50Seconds: 180, P90Seconds: 276}}, {ProjectID: "project", Stage: "plan", AnalyticsDuration: operatortool.AnalyticsDuration{Count: 1, Seconds: 30, P50Seconds: 30, P90Seconds: 30}}}},
		{name: "no data", attempts: population[4:], want: []operatortool.ActivityTypicalDuration{}},
		{name: "partial sample", attempts: population[:1], partial: true, want: []operatortool.ActivityTypicalDuration{{ProjectID: "project", Stage: "code", AnalyticsDuration: operatortool.AnalyticsDuration{Count: 1, Seconds: 60, P50Seconds: 60, P90Seconds: 60}, Partial: true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			samples := make([]activityDurationSample, len(tt.attempts))
			for i, a := range tt.attempts {
				samples[i] = activityDurationSample{Role: a.Identity.Role, Status: a.Status, Seconds: a.Phases[0].FinishedAt.Sub(a.Phases[0].StartedAt).Seconds()}
			}
			got := activityTypicalDurations("project", samples, tt.partial)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("typical=%+v want=%+v", got, tt.want)
			}
			combined := append([]nativeAnalyticsAttempt{}, tt.attempts...)
			for i := range combined {
				combined[i].Identity = agentidentity.Configured("codex", "codex", "", combined[i].Identity.Role, "combined", "openai", "combined", "", base)
			}
			reports, _ := reportsAttemptTotals(combined, nil)
			for _, typical := range got {
				found := false
				for _, stage := range reports {
					if stage.Stage == typical.Stage {
						found = true
						reported := stage.Duration
						reported.Samples = nil
						if !reflect.DeepEqual(typical.AnalyticsDuration, reported) {
							t.Fatalf("activity=%+v Reports=%+v", typical, stage)
						}
					}
				}
				if !found {
					t.Fatalf("Reports missing %s", typical.Stage)
				}
			}
		})
	}
}
func TestActivityIssueResidence(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	lanes := []tracker.NativeState{{Name: "Ideas"}, {Name: "Ready", Dispatchable: true}, {Name: "Build", Dispatchable: true}, {Name: "Waiting"}, {Name: "Shipped", Terminal: true}, {Name: "Cancelled", Terminal: true}}
	type entry struct {
		lane   string
		second int
	}
	for _, tt := range []struct {
		name         string
		entries      []entry
		start, total float64
		seconds      []float64
		counts       []int
		names        []string
		terminal     string
		unknown      bool
	}{
		{name: "first active entry excludes backlog", entries: []entry{{"Ideas", 0}, {"Ready", 10}, {"Build", 30}}, start: 10, total: 90, seconds: []float64{20, 70}, counts: []int{1, 1}, names: []string{"Ready", "Build"}},
		{name: "holding before active resets cycle", entries: []entry{{"Ready", 0}, {"Build", 10}, {"Ideas", 20}, {"Ready", 40}, {"Build", 60}}, start: 40, total: 60, seconds: []float64{20, 40}, counts: []int{1, 1}, names: []string{"Ready", "Build"}},
		{name: "holding after active parks without reset", entries: []entry{{"Ready", 0}, {"Build", 10}, {"Waiting", 20}, {"Build", 50}}, total: 100, seconds: []float64{10, 60, 30}, counts: []int{1, 2, 1}, names: []string{"Ready", "Build", "Waiting"}},
		{name: "unvisited active lanes present and holding absent", entries: []entry{{"Ready", 0}}, total: 100, seconds: []float64{100, 0}, counts: []int{1, 0}, names: []string{"Ready", "Build"}},
		{name: "custom terminal stops clock", entries: []entry{{"Ready", 0}, {"Build", 10}, {"Shipped", 70}}, total: 70, seconds: []float64{10, 60}, counts: []int{1, 1}, names: []string{"Ready", "Build"}, terminal: "Shipped"},
		{name: "cancelled is terminal", entries: []entry{{"Ideas", 0}, {"Ready", 10}, {"Cancelled", 25}}, start: 10, total: 15, seconds: []float64{15, 0}, counts: []int{1, 0}, names: []string{"Ready", "Build"}, terminal: "Cancelled"},
		{name: "truncated prefix does not fabricate a start", entries: []entry{{"Build", 20}}, unknown: true, seconds: []float64{0, 0}, counts: []int{0, 0}, names: []string{"Ready", "Build"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			samples := []analyticsResidenceSample{{Created: base, At: base, Entered: base, Initial: tt.entries[0].lane, State: tt.entries[0].lane}}
			for i, e := range tt.entries[1:] {
				at := base.Add(time.Duration(e.second) * time.Second)
				samples = append(samples, analyticsResidenceSample{Created: base, At: at, Entered: at, State: e.lane, Sequence: int64(i + 1)})
			}
			if tt.unknown {
				samples[0].Sequence = 4
			}
			got := activityIssueResidence(operatortool.ActivityIssue{}, lanes, samples, base.Add(100*time.Second))
			if got.Partial != tt.unknown || (!tt.unknown && got.ClockStartedAt.Sub(base).Seconds() != tt.start) || (tt.unknown && !got.ClockStartedAt.IsZero()) || got.TotalSeconds != tt.total || got.TerminalLane != tt.terminal {
				t.Fatalf("issue=%+v", got)
			}
			var names []string
			var seconds []float64
			var counts []int
			for _, lane := range got.Lanes {
				names = append(names, lane.Name)
				seconds = append(seconds, lane.Seconds)
				counts = append(counts, lane.Entries)
			}
			if !reflect.DeepEqual(names, tt.names) || !reflect.DeepEqual(seconds, tt.seconds) || !reflect.DeepEqual(counts, tt.counts) {
				t.Fatalf("lanes=%+v", got.Lanes)
			}
		})
	}
}
