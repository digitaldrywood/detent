package nochange_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNoChangeCompletionSuppliesCurrentBaseToRequiredGates(t *testing.T) {
	scenariotest.RunNoChangeCompletion(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNoChangeRunnerExecutesIndependentRequiredGates(t *testing.T) {
	scenariotest.RunNoChangeRequiredGates(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestRunnerAbsorbedCompletion(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "absorbed Rework settles the original version on its actual base", Absorbed: true, Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "NativeCompletion no-change source already on target", Delivered: "ancestor", AdvanceTarget: true, Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "NativeCompletion no-change reviewed head already delivered", Delivered: "exact", Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "absorbed SSH Rework settles without a second provider session", Absorbed: true, SSH: true, Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "absorbed Rework preserves formal human rejection", Absorbed: true, Rework: true, Formal: true, Land: true, WantState: "Human Review", WantChanges: 1},
		{Name: "absorbed Rework preserves a reported human hold", Absorbed: true, Hold: true, Rework: true, Land: true, WantState: "Human Review", WantChanges: 1},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
