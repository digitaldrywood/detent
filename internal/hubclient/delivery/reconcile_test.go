package delivery_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNativeReleaseDeliveryReconciliation(t *testing.T) {
	scenariotest.RunReleaseDeliveryReconciliation(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestRunnerDeliveredCompletion(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "NativeCompletion no-change release source already on release", Delivered: "release", Rework: true, Land: true, WantState: "Done", WantChanges: 1},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
