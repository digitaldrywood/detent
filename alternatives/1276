package landing_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNativeExecutionLandingTargetRefusesUnreviewed(t *testing.T) {
	scenariotest.RunNativeExecutionLandingTargetRefusesUnreviewed(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNativeExecutionOperatorLandingTarget(t *testing.T) {
	scenariotest.RunNativeExecutionOperatorLandingTarget(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNativeLandingBatchClaimsOneCapacityUnit(t *testing.T) {
	scenariotest.RunNativeLandingBatchClaimsOneCapacityUnit(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestNativeLandingScenarios(t *testing.T) {
	scenariotest.RunLandingScenarios(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestLinkedNativeIssueLandsWithoutGitHub(t *testing.T) {
	scenariotest.RunLinkedLandingScenarios(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
