package planner_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNativePlanContinuation(t *testing.T) {
	scenariotest.RunPlanContinuation(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNativePlannerAutomaticHandoff(t *testing.T) {
	scenariotest.RunPlannerAutomaticHandoff(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestRunnerDisposition(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "blocked native729 first unchanged Rework preserves dependency", FinalMessage: scenariotest.DependencyMessage, WantDisposition: scenariotest.DependencyDisposition(), Rework: true},
		{Name: "blocked native825 unchanged Rework preserves instance gate blocker", FinalMessage: scenariotest.InstanceMessage, WantDisposition: scenariotest.InstanceDisposition(), Rework: true},
		{Name: "blocked SSH Rework retains staged source", FinalMessage: scenariotest.InstanceMessage, WantDisposition: scenariotest.InstanceDisposition(), Rework: true, SSH: true, Staged: true},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
