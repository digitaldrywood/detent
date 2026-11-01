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
		{Name: "blocked native729 first unchanged Rework preserves dependency", FinalMessage: scenariotest.DependencyMessage, WantDisposition: scenariotest.DependencyDisposition(), Rework: true},
		{Name: "imported unchanged Rework records implementing attempt before validation", ImportedVersion: true, Rework: true, Validator: "pass", Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "imported unchanged Rework cannot clear existing human rejection", ImportedVersion: true, Rework: true, Formal: true, Validator: "pass", Land: true},
		{Name: "blocked native825 unchanged Rework preserves instance gate blocker", FinalMessage: scenariotest.InstanceMessage, WantDisposition: scenariotest.InstanceDisposition(), Rework: true},
		{Name: "blocked SSH Rework retains staged source", FinalMessage: scenariotest.InstanceMessage, WantDisposition: scenariotest.InstanceDisposition(), Rework: true, SSH: true, Staged: true},
		{Name: "absorbed Rework settles the original version on its actual base", Absorbed: true, Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "NativeCompletion no-change source already on target", Delivered: "ancestor", AdvanceTarget: true, Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "NativeCompletion no-change reviewed head already delivered", Delivered: "exact", Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "NativeCompletion no-change release source already on release", Delivered: "release", Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "absorbed SSH Rework settles without a second provider session", Absorbed: true, SSH: true, Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "absorbed Rework preserves formal human rejection", Absorbed: true, Rework: true, Formal: true, Land: true, WantState: "Human Review", WantChanges: 1},
		{Name: "absorbed Rework preserves a reported human hold", Absorbed: true, Hold: true, Rework: true, Land: true, WantState: "Human Review", WantChanges: 1},
		{Name: "same worker reconnects during a provider command and publishes once", Reconnect: true, Staged: true, WantChanged: true, WantState: "In Review", WantChanges: 1},
		{Name: "enrolled source evidence hold releases capacity and resumes with fresh authority", Enrolled: true, SummaryFailure: true, Staged: true, Validator: "pass"},
		{Name: "authenticated Continue delivers after retained source pass summary failure", ContinueMessage: true, SummaryFailure: true, Staged: true},
		{Name: "authenticated Continue delivers after completed source and acceptance feedback", ContinueMessage: true, CompletedContinue: true, SummaryFailure: true, Feedback: "technical", Validator: "pass", Staged: true},
		{Name: "authenticated Continue delivers after optional completed source feedback", ContinueMessage: true, CompletedContinue: true, SummaryFailure: true, Feedback: "technical", Staged: true},
		{Name: "authenticated Continue delivers after source wait summary failure", ContinueMessage: true, InitialSourceWait: true, SummaryFailure: true, Staged: true},
		{Name: "deferred interrupted source summary failure survives Scheduler restart", RestartCompletion: true, Enrolled: true, SummaryFailure: true, Staged: true},
		{Name: "deferred interrupted source summary failure restores separate required acceptance", RestartCompletion: true, Enrolled: true, SummaryFailure: true, Staged: true, Validator: "pass"},
		{Name: "deferred interrupted source wait survives Scheduler restart", RestartCompletion: true, Enrolled: true, SummaryFailure: true, InitialSourceWait: true, Staged: true},
		{Name: "acknowledged interrupted completion survives legacy recapture restart", LegacyReleaseRestart: true, Enrolled: true, SummaryFailure: true, Staged: true},
		{Name: "deferred source summary failure releases interrupted and resumes without coding", SummaryFailure: true, Staged: true},
		{Name: "current change feedback refreshes retained source pass before acceptance", SummaryFailure: true, Staged: true, Validator: "pass", Feedback: "change"},
		{Name: "issue feedback refreshes retained source pass before acceptance", SummaryFailure: true, Staged: true, Validator: "pass", Feedback: "issue"},
		{Name: "human edit refreshes retained source pass before acceptance", SummaryFailure: true, Staged: true, Validator: "pass", Feedback: "edit"},
		{Name: "current change feedback refreshes source wait before acceptance", SummaryFailure: true, Staged: true, Validator: "pass", Feedback: "change", InitialSourceWait: true},
		{Name: "technical evidence feedback reviews without writable work", SummaryFailure: true, Staged: true, Feedback: "technical"},
		{Name: "runner feedback preserves retained source pass reuse", SummaryFailure: true, Staged: true, Feedback: "bot"},
		{Name: "expired parent publishes completed staged code", Expired: true, Staged: true, WantChanged: true, WantState: "In Review", WantChanges: 1},
		{Name: "expired parent publishes with native validation", Expired: true, Staged: true, Validator: "pass", Land: true, WantChanged: true, WantState: "Merging", WantChanges: 1},
		{Name: "expired parent publishes completed Rework commits", Expired: true, Rework: true, Formal: true, Commit: true, WantChanged: true, WantState: "Human Review", WantChanges: 1},
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
