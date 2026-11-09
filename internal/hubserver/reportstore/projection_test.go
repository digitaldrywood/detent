package reportstore

import (
	"math"
	"testing"

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
