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

func TestRunnerRecoveredSource(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "Rework receives current Change discussion", Rework: true, Commit: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
		{Name: "Rework receives formal requested changes", Rework: true, Formal: true, Commit: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
		{Name: "host commits staged Rework", Rework: true, Formal: true, Staged: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
		{Name: "clean Rework excludes target advancement", Rework: true, Formal: true, AdvanceTarget: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
		{Name: "staged Rework excludes target advancement", Rework: true, Formal: true, Staged: true, AdvanceTarget: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
		{Name: "committed Rework excludes target advancement", Rework: true, Formal: true, Commit: true, AdvanceTarget: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
		{Name: "host signing unavailable preserves requested changes", Rework: true, Formal: true, Staged: true, BrokenSigning: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
		{Name: "Rework lands the clean preserved reviewed head without source changes", Rework: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "reopened completed Rework lands its reviewed current version", Reopened: true, Rework: true, Staged: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "reopened Rework accepts current requested changes before repair", Reopened: true, Rework: true, Formal: true, Staged: true, Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "Rework forwards a refused version to review", Rework: true, Commit: true, FailVersion: true, WantState: "Human Review"},
		{Name: "Rework scoped read failure releases claim before dispatch", Rework: true, FailDetail: true},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}
