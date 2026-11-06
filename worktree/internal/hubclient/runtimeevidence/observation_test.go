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
