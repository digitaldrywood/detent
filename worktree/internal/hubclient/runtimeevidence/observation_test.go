package runtimeevidence_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }

func TestNativeRuntimeCheckpointsDoNotGrowHistory(t *testing.T) {
	scenariotest.RunNativeRuntimeCheckpointsDoNotGrowHistory(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestNativeAttemptTerminalActivityReceipts(t *testing.T) {
	scenariotest.RunNativeAttemptTerminalActivityReceipts(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
