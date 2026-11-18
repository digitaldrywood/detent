package checkpoint_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestRunnerRecovery(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "enrolled source evidence hold releases capacity and resumes with fresh authority", Enrolled: true, SummaryFailure: true, Staged: true, Validator: "pass"},
		{Name: "deferred interrupted source summary failure survives Scheduler restart", RestartCompletion: true, Enrolled: true, SummaryFailure: true, Staged: true},
		{Name: "deferred interrupted source summary failure restores separate required acceptance", RestartCompletion: true, Enrolled: true, SummaryFailure: true, Staged: true, Validator: "pass"},
		{Name: "deferred interrupted source wait survives Scheduler restart", RestartCompletion: true, Enrolled: true, SummaryFailure: true, InitialSourceWait: true, Staged: true},
		{Name: "acknowledged interrupted completion survives legacy recapture restart", LegacyReleaseRestart: true, Enrolled: true, SummaryFailure: true, Staged: true},
		{Name: "deferred source summary failure releases interrupted and resumes without coding", SummaryFailure: true, Staged: true},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
