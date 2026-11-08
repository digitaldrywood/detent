package conversation_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) {
	os.Exit(testsupport.RunMain(m, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return errors.Join(scenariotest.ClosePostgres(), testsupport.CloseNativeChangePostgres(ctx), testsupport.CloseDispatchPostgres(ctx))
	}))
}
func TestConversationRunnerExecutesLiveTurns(t *testing.T) {
	scenariotest.RunConversationRunnerExecutesLiveTurns(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestConversationRunnerNegotiatesPreStartBinding(t *testing.T) {
	scenariotest.RunConversationRunnerNegotiatesPreStartBinding(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestRunnerContinuation(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "authenticated Continue delivers after retained source pass summary failure", ContinueMessage: true, SummaryFailure: true, Staged: true},
		{Name: "authenticated Continue delivers after completed source and acceptance feedback", ContinueMessage: true, CompletedContinue: true, SummaryFailure: true, Feedback: "technical", Validator: "pass", Staged: true},
		{Name: "authenticated Continue delivers after optional completed source feedback", ContinueMessage: true, CompletedContinue: true, SummaryFailure: true, Feedback: "technical", Staged: true},
		{Name: "authenticated Continue delivers after source wait summary failure", ContinueMessage: true, InitialSourceWait: true, SummaryFailure: true, Staged: true},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
