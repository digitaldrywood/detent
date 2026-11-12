package reportstore

import (
	"math"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestProcessProjectionRequiresTerminalFiniteUsage(t *testing.T) {
	for _, test := range []struct {
		name, kind string
		usage      *procgroup.Usage
		known      bool
	}{
		{"finished usage", "run.finished", &procgroup.Usage{PeakMemoryBytes: 4096, UserCPUSeconds: 8, SystemCPUSeconds: 2, WallSeconds: 10}, true},
		{"missing usage", "run.finished", nil, false},
		{"unfinished usage", "run.observed", &procgroup.Usage{PeakMemoryBytes: 4096, WallSeconds: 10}, false},
		{"missing wall time", "run.finished", &procgroup.Usage{PeakMemoryBytes: 4096}, false},
		{"missing memory", "run.finished", &procgroup.Usage{WallSeconds: 10}, false},
		{"negative CPU", "run.finished", &procgroup.Usage{PeakMemoryBytes: 4096, UserCPUSeconds: -1, WallSeconds: 10}, false},
		{"infinite CPU", "run.finished", &procgroup.Usage{PeakMemoryBytes: 4096, SystemCPUSeconds: math.Inf(1), WallSeconds: 10}, false},
		{"CPU sum overflow", "run.finished", &procgroup.Usage{PeakMemoryBytes: 4096, UserCPUSeconds: 1e308, SystemCPUSeconds: 1e308, WallSeconds: 1}, false},
		{"CPU rate overflow", "run.finished", &procgroup.Usage{PeakMemoryBytes: 4096, UserCPUSeconds: 1e308, SystemCPUSeconds: 0, WallSeconds: 1e-300}, false},
		{"NaN wall time", "run.finished", &procgroup.Usage{PeakMemoryBytes: 4096, WallSeconds: math.NaN()}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := ProjectEvent(tracker.CollaborationEvent{Type: test.kind, Data: tracker.CollaborationData{Run: &tracker.NativeRunData{ProcessUsage: test.usage, Outcome: "succeeded"}}})
			expectedStatus := "unavailable"
			if test.known {
				expectedStatus = "usable"
			}
			if test.kind != "run.finished" {
				expectedStatus = "unknown"
			}
			if got == nil || got.ProcessEvidenceStatus != expectedStatus {
				t.Fatalf("projection=%+v", got)
			}
			if test.known {
				if got.ProcessPeakMemoryBytes != 4096 || got.ProcessUserCPUSeconds != 8 || got.ProcessSystemCPUSeconds != 2 || got.ProcessWallSeconds != 10 {
					t.Fatalf("process usage=%+v", got)
				}
			} else if got.ProcessPeakMemoryBytes != 0 || got.ProcessUserCPUSeconds != 0 || got.ProcessSystemCPUSeconds != 0 || got.ProcessWallSeconds != 0 {
				t.Fatalf("unknown usage published numbers=%+v", got)
			}
		})
	}
}

func TestProjectionPreservesPhaseTimingCompleteness(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	closed := tracker.NativePhase{StartedAt: base, FinishedAt: base.Add(60250001 * time.Microsecond)}
	for _, test := range []struct {
		name    string
		runtime *tracker.NativeRuntimeObservation
		status  string
		seconds float64
	}{
		{name: "missing runtime", status: "partial"},
		{name: "missing phases", runtime: &tracker.NativeRuntimeObservation{}, status: "partial"},
		{name: "fractional closed interval", runtime: &tracker.NativeRuntimeObservation{Phases: []tracker.NativePhase{closed}}, status: "complete", seconds: 60.250001},
		{name: "dropped interval", runtime: &tracker.NativeRuntimeObservation{Phases: []tracker.NativePhase{closed}, PhasesDropped: 1}, status: "partial", seconds: 60.250001},
		{name: "missing start preserves immutable historical sum", runtime: &tracker.NativeRuntimeObservation{Phases: []tracker.NativePhase{{FinishedAt: base}}}, status: "partial", seconds: 9223372036.854776},
		{name: "open interval partial", runtime: &tracker.NativeRuntimeObservation{Phases: []tracker.NativePhase{closed, {StartedAt: base.Add(time.Hour)}}}, status: "partial", seconds: 60.250001},
		{name: "negative interval partial with zero sum", runtime: &tracker.NativeRuntimeObservation{Phases: []tracker.NativePhase{{StartedAt: base, FinishedAt: base.Add(-time.Second)}}}, status: "partial"},
	} {
		t.Run(test.name, func(t *testing.T) {
			projection := ProjectEvent(tracker.CollaborationEvent{Type: "run.finished", Data: tracker.CollaborationData{Run: &tracker.NativeRunData{Outcome: "succeeded", Runtime: test.runtime}}})
			if projection == nil || projection.PhaseTimingStatus != test.status || math.Abs(projection.DurationSeconds-test.seconds) > 1e-9 {
				t.Fatalf("phase projection=%+v want status=%s seconds=%f", projection, test.status, test.seconds)
			}
		})
	}
}
