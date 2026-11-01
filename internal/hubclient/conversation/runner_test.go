package conversation_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestConversationRunnerExecutesLiveTurns(t *testing.T) {
	scenariotest.RunConversationRunnerExecutesLiveTurns(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestConversationRunnerNegotiatesPreStartBinding(t *testing.T) {
	scenariotest.RunConversationRunnerNegotiatesPreStartBinding(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
