package recovery_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestRunnerHistory(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "growing multi-page machine history keeps fresh conflict recovery compact", Rework: true, StaleBase: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}

func TestNativeRunnerReconnectsRetainedAttempt(t *testing.T) {
	scenariotest.RunNativeRunnerReconnectsRetainedAttempt(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
