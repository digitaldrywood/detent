package planner_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestPlannerSettlement(t *testing.T) {
	head := scenariotest.FixtureHead("c")
	scenariotest.RunSettlement(t, []scenariotest.SettlementScenario{
		{Name: "a plan run decides nothing", Role: runner.RolePlan, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md")},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.SettlementChecks{})
}
