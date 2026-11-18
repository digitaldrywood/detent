package nochange_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestNoChangeSettlement(t *testing.T) {
	base := scenariotest.FixtureHead("a")
	head := scenariotest.FixtureHead("c")
	scenariotest.RunSettlement(t, []scenariotest.SettlementScenario{
		{Name: "native273 malformed predicate retains rejection", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(base), FinalMessage: "```detent-status\nschema: 1\nstatus: blocked\nblockers:\n  - reason: browser unavailable\n    predicate: instance_available\nhuman_action: null\n```", WantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{Name: "invalid report preserves legacy receipt", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(base), FinalMessage: "```detent-status\nschema: 99\nstatus: in_progress\nblockers: []\nhuman_action: null\n```", WantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{Name: "verified empty Rework publishes cancellation for independent review", EmptyRework: "verified", RetainSource: true, Reviewer: true, Role: runner.RoleRework, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head), Existing: true, Published: scenariotest.FixtureHead("b"),
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head}, WantChanges: 1, WantVersions: 2},
		{Name: "unverified empty Rework preserves the mismatch", EmptyRework: "unverified", RetainSource: true, Role: runner.RoleRework, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head), Existing: true, Published: scenariotest.FixtureHead("b"),
			WantDiagnostic: "the final attempt diff does not identify the current Change Request head", WantChanges: 1, WantVersions: 1},
		{Name: "empty rework without recovered source retains the mismatch", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head), Existing: true, Published: scenariotest.FixtureHead("b"), RetainSource: true,
			WantDiagnostic: "the final attempt diff does not identify the current Change Request head", WantChanges: 1, WantVersions: 1},
		{Name: "empty recovered rework without source capture retains the mismatch", EmptyRework: "verified", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head), Existing: true, Published: scenariotest.FixtureHead("b"),
			WantDiagnostic: "the final attempt diff does not identify the current Change Request head", WantChanges: 1, WantVersions: 1},
		{Name: "empty unrelated head retains the mismatch for review", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(base), Existing: true, Published: head,
			WantDiagnostic: "the final attempt diff does not identify the current Change Request head", WantChanges: 1, WantVersions: 1},
		{Name: "a clean worktree with no commits opens nothing", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(base),
			WantChange: &runner.NativeChange{BaseSHA: base, HeadSHA: base}},
		{Name: "an inaccessible preserved version cannot succeed", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head), Existing: true, Published: head, FailDetail: true,
			WantDiagnostic: "change detail unavailable", WantChanges: 1, WantVersions: 1},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.SettlementChecks{})
}
