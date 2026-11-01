package changesource_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNativeReworkRecoversReviewedSourceAcrossMachines(t *testing.T) {
	scenariotest.RunNativeReworkRecoversReviewedSourceAcrossMachines(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNativeSourceResumesWithoutPreviousRuntime(t *testing.T) {
	scenariotest.RunNativeSourceResumesWithoutPreviousRuntime(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
