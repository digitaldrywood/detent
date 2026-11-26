package remoterunner

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestLifecycleRetriesStartWithHubGeneration(t *testing.T) {
	var reports []runnerauth.LifecycleCommand
	owner := NewLifecycle("1.2.3", "revision", func(_ context.Context, command runnerauth.LifecycleCommand) error {
		reports = append(reports, command)
		if len(reports) == 1 {
			return &api.APIError{Status: http.StatusConflict, Code: runnerauth.LifecycleStartGenerationChanged, CurrentRevision: 7}
		}
		return nil
	})
	for _, state := range []runnerauth.LifecycleState{runnerauth.LifecycleStarting, runnerauth.LifecycleReady} {
		if err := owner.Report(t.Context(), state, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(reports) != 3 || reports[0].PreviousGeneration != 0 || reports[1].PreviousGeneration != 7 ||
		reports[0].LifecycleReport != reports[1].LifecycleReport || reports[1].Sequence != 1 ||
		reports[2].Sequence != 2 || reports[2].PreviousGeneration != 0 {
		t.Fatalf("generation negotiation reports = %+v", reports)
	}
}

func TestLifecycleReportsRoutingTransitions(t *testing.T) {
	for _, initial := range []string{"active", "draining"} {
		t.Run(initial, func(t *testing.T) {
			reports := []runnerauth.LifecycleReport{}
			owner := NewLifecycle("1.2.3", "revision", func(_ context.Context, command runnerauth.LifecycleCommand) error {
				report := command.LifecycleReport
				reports = append(reports, report)
				return nil
			})
			report := func(state runnerauth.LifecycleState) {
				t.Helper()
				if err := owner.Report(t.Context(), state, "", ""); err != nil {
					t.Fatal(err)
				}
			}
			report(runnerauth.LifecycleStarting)
			if err := owner.ObserveRouting(t.Context(), initial); err != nil {
				t.Fatal(err)
			}
			if len(reports) != 1 {
				t.Fatal("routing replaced startup report before ready")
			}
			report(runnerauth.LifecycleReady)
			if err := owner.ObserveRouting(t.Context(), "draining"); err != nil {
				t.Fatal(err)
			}
			if err := owner.ObserveRouting(t.Context(), "draining"); err != nil {
				t.Fatal(err)
			}
			if err := owner.ObserveRouting(t.Context(), "active"); err != nil {
				t.Fatal(err)
			}
			states := []runnerauth.LifecycleState{}
			for i, r := range reports {
				states = append(states, r.State)
				if r.ProcessID != reports[0].ProcessID || r.Sequence != int64(i+1) {
					t.Fatalf("transition identity=%+v", r)
				}
			}
			if !slices.Equal(states, []runnerauth.LifecycleState{runnerauth.LifecycleStarting, runnerauth.LifecycleReady, runnerauth.LifecycleDraining, runnerauth.LifecycleReady}) {
				t.Fatalf("routing transitions=%v", states)
			}
		})
	}
}

func TestLifecycleReportsShutdownOnce(t *testing.T) {
	for _, reason := range []string{"signal", "update"} {
		t.Run(reason, func(t *testing.T) {
			var reports []runnerauth.LifecycleReport
			owner := NewLifecycle("1.2.3", "revision", func(_ context.Context, command runnerauth.LifecycleCommand) error {
				report := command.LifecycleReport
				reports = append(reports, report)
				return nil
			})
			var target string
			if reason == "update" {
				target = "1.2.4"
				if err := owner.Report(t.Context(), runnerauth.LifecycleShuttingDown, reason, target); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := owner.Shutdown(t.Context(), reason); err != nil {
					t.Fatal(err)
				}
			}
			if len(reports) != 1 || reports[0].State != runnerauth.LifecycleShuttingDown || reports[0].Reason != reason || reports[0].TargetVersion != target {
				t.Fatalf("shutdown reports = %+v", reports)
			}
		})
	}
}
