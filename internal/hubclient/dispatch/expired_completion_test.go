package dispatch_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestRunnerExpiredCompletion(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "expired parent preserves staged code", Expired: true, Staged: true, WantState: "In Progress"},
		{Name: "expired parent skips native validation and landing", Expired: true, Staged: true, Validator: "pass", Land: true, WantState: "In Progress"},
		{Name: "expired parent preserves Rework commits", Expired: true, Rework: true, Formal: true, Commit: true, WantState: "Rework", WantChanges: 1},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
