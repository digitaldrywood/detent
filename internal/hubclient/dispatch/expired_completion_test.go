package dispatch_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestRunnerExpiredCompletion(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "expired parent retains staged code", Expired: true, Staged: true},
		{Name: "expired parent interrupts native validation", Expired: true, Staged: true, Validator: "pass", Land: true},
		{Name: "expired parent retains Rework commits", Expired: true, Rework: true, Formal: true, Commit: true},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
