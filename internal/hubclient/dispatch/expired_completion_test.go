package dispatch_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestRunnerExpiredCompletion(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "expired parent preserves staged code", Expired: true, Staged: true},
		{Name: "expired parent stops before native validation", Expired: true, Staged: true, Validator: "pass"},
		{Name: "expired parent preserves unpushed Rework commits", Expired: true, Rework: true, Formal: true, Commit: true},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
