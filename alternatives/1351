package readtools_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNativeCommentReadFallback(t *testing.T) {
	scenariotest.RunNativeCommentReadFallback(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNativeExecutionReadToolsKeepHostAuthority(t *testing.T) {
	scenariotest.RunNativeExecutionReadToolsKeepHostAuthority(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNativeChangeOversizedReviewRecovery(t *testing.T) {
	scenariotest.RunNativeChangeOversizedReviewRecovery(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
