package dispatch_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestRunnerExpiredCompletion(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "expired parent publishes completed staged code", Expired: true, Staged: true, WantChanged: true, WantState: "In Review", WantChanges: 1},
		{Name: "expired parent publishes with native validation", Expired: true, Staged: true, Validator: "pass", Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "expired parent publishes completed Rework commits", Expired: true, Rework: true, Formal: true, Commit: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
