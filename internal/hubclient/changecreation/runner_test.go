package changecreation_test

import (
	"os"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestRunnerCreation(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "enrolled runner executes beside intake-off local runtime", LocalIntakeOff: true, Staged: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "commits", Commit: true, WantChanged: true, WantState: "In Review", WantChanges: 1},
		{Name: "initial interactive code stays conversation owned", Interactive: true, Staged: true, WantNone: true, WantState: "In Progress"},
		{Name: "inherited synthetic fixtures publish", SourceFixture: "synthetic", Staged: true, WantChanged: true, WantState: "In Review", WantChanges: 1},
		{Name: "introduced credential refuses publication", SourceFixture: "introduced", Staged: true},
		{Name: "inherited credential refuses publication", SourceFixture: "inherited", Staged: true},
		{Name: "unsafe intermediate commit refuses publication", SourceFixture: "history", Staged: true},
		{Name: "host commits staged code", Staged: true, WantChanged: true, WantState: "In Review", WantChanges: 1},
		{Name: "ordinary staged code reaches landing", Staged: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "successful base moved wait reclaims the reviewed unlanded version", Staged: true, Land: true, BaseMoved: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "hosted template commits reach Human Review", Hosted: true, Commit: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
		{Name: "hosted template without commits ends", Hosted: true, WantState: "Done"},
		{Name: "no commits", WantState: "Done"},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
