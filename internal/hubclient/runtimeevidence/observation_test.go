package runtimeevidence_test

import (
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/hubclient/runtimeevidence"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }

func TestNativeRuntimeCheckpointsDoNotGrowHistory(t *testing.T) {
	scenariotest.RunNativeRuntimeCheckpointsDoNotGrowHistory(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestNativeAttemptTerminalActivityReceipts(t *testing.T) {
	scenariotest.RunNativeAttemptTerminalActivityReceipts(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestMergeValidationObservationDoesNotInventPhase(t *testing.T) {
	validation := gate.CommandResult{Command: "go test internal/example"}
	observation := runtimeevidence.Merge(nil, tracker.NativeRuntimeObservation{Validation: &validation})
	if observation.Phase != "" || !observation.HeartbeatAt.IsZero() || len(observation.Phases) != 0 || observation.Validation == nil {
		t.Fatalf("merged validation runtime = %+v, want no fabricated phase", observation)
	}

	at := time.Date(2026, time.November, 3, 12, 0, 0, 0, time.UTC)
	previous := &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: at,
		Phases: []tracker.NativePhase{{Name: "implementation", StartedAt: at}}}
	observation = runtimeevidence.Merge(previous, tracker.NativeRuntimeObservation{Validation: &validation})
	if observation.Phase != previous.Phase || !reflect.DeepEqual(observation.Phases, previous.Phases) || observation.Validation == nil {
		t.Fatalf("merged existing runtime = %+v, want prior phase history and validation", observation)
	}
}

func TestMergeKeepsNativeSessionCorrelation(t *testing.T) {
	for _, test := range []struct {
		name           string
		previous       *tracker.NativeRuntimeObservation
		observation    tracker.NativeRuntimeObservation
		wantAttempt    int64
		wantGeneration uint64
	}{
		{name: "native session replaces legacy correlation", observation: tracker.NativeRuntimeObservation{Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 9}}, wantAttempt: 0, wantGeneration: 0},
		{name: "native generation replaces legacy correlation", observation: tracker.NativeRuntimeObservation{Generation: 3, Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 9, Generation: 3}}, wantAttempt: 0, wantGeneration: 3},
		{name: "conflicting generation remains distinct", observation: tracker.NativeRuntimeObservation{Generation: 4, Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 9, Generation: 3}}, wantAttempt: 0, wantGeneration: 4},
		{name: "local session retains explicit correlation", observation: tracker.NativeRuntimeObservation{LocalAttemptID: 8, Generation: 3, Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 9, AttemptID: 8, Generation: 3}}, wantAttempt: 8, wantGeneration: 3},
		{name: "sparse heartbeat retains correlation", wantAttempt: 7, wantGeneration: 2},
		{name: "sparse heartbeat retains native generation", previous: &tracker.NativeRuntimeObservation{Generation: 3, Activity: &workflowmetrics.ActivityProfile{Schema: 1, SessionID: 9, Generation: 3}}, wantAttempt: 0, wantGeneration: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			previous := &tracker.NativeRuntimeObservation{LocalAttemptID: 7, Generation: 2}
			if test.previous != nil {
				previous = test.previous
			}
			merged := runtimeevidence.Merge(previous, test.observation)
			if merged.LocalAttemptID != test.wantAttempt || merged.Generation != test.wantGeneration {
				t.Fatalf("merged correlation = %d/%d, want %d/%d", merged.LocalAttemptID, merged.Generation, test.wantAttempt, test.wantGeneration)
			}
			wantActivity := test.observation.Activity
			if wantActivity == nil {
				wantActivity = previous.Activity
			}
			if !reflect.DeepEqual(merged.Activity, wantActivity) {
				t.Fatalf("merged activity = %+v, want %+v", merged.Activity, wantActivity)
			}
		})
	}
}
