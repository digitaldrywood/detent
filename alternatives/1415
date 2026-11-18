package provider_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNativeRetryProgressSharedOutagePersistsOneDefect(t *testing.T) {
	scenariotest.RunNativeRetryProgressSharedOutagePersistsOneDefect(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNativeRetryProgressLoadsRepeatedLandingRefusals(t *testing.T) {
	scenariotest.RunNativeRetryProgressLoadsRepeatedLandingRefusals(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNativeRetryProgressRerouteExecutionIdentity(t *testing.T) {
	scenariotest.RunNativeRetryProgressRerouteExecutionIdentity(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNativeRetryProgressFailureReleasesExecutionIdentity(t *testing.T) {
	scenariotest.RunNativeRetryProgressFailureReleasesExecutionIdentity(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
