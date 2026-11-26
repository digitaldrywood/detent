package evidence_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNativeChangeLargeValidationFinalization(t *testing.T) {
	scenariotest.RunNativeChangeLargeValidationFinalization(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestRunnerFeedback(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "current change feedback refreshes retained source pass before acceptance", SummaryFailure: true, Staged: true, Validator: "pass", Feedback: "change"},
		{Name: "issue feedback refreshes retained source pass before acceptance", SummaryFailure: true, Staged: true, Validator: "pass", Feedback: "issue"},
		{Name: "runner feedback preserves retained source pass reuse", SummaryFailure: true, Staged: true, Feedback: "bot"},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
