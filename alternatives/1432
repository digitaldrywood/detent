package validation_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestRunnerValidation(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "SSH native validator pass after publication", SSH: true, Validator: "pass", Staged: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "dependency validation with large output", DependencyOnly: true, LargeOutput: true, Validator: "pass", Staged: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "dependency-only native validation evidence", DependencyOnly: true, Validator: "pass", Staged: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "SSH dependency-only native validation evidence", SSH: true, DependencyOnly: true, Validator: "pass", Staged: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "failed dependency check cannot pass review", DependencyOnly: true, FailedCheck: true, Validator: "pass", WantVerdict: "wait", Staged: true, WantChanged: true, WantState: "In Review", WantChanges: 1},
		{Name: "native validator pass after publication", Validator: "pass", Staged: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "native validator low score requests rework", LowScore: true, Validator: "pass", WantVerdict: "rework", Staged: true, WantChanged: true, WantState: "In Review", WantChanges: 1},
		{Name: "native validator rework retains version", Validator: "rework", Staged: true, WantChanged: true, WantState: "In Review", WantChanges: 1},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
