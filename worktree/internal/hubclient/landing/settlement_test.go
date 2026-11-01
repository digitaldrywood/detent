package landing_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/runner"
)

func TestLandingSettlement(t *testing.T) {
	head := scenariotest.FixtureHead("c")
	scenariotest.RunSettlement(t, []scenariotest.SettlementScenario{
		{Name: "NativeChange changed integration head publishes a new version", Land: true, Integrate: true, IntegrateChanged: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "NativeChange audit enabled changed integration head publishes a new version", Land: true, Integrate: true, IntegrateChanged: true, IntegrateAudit: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "NativeChange audit enabled published integration head retains its audit", Land: true, Integrate: true, IntegrateChanged: true, IntegrateHead: true, IntegrateAudit: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "NativeChange genuine integration conflict retains Rework evidence", RetainSource: true, Land: true, Integrate: true, IntegrationConflict: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "NativeChange audited integration conflict retains corrective landing", RetainSource: true, IntegrateAudit: true, Land: true, Integrate: true, IntegrationConflict: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "NativeChange same published head keeps reviewed receipts after incorporation refusal", Land: true, Integrate: true, IntegrateHead: true, IntegrationConflict: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "reviewed source lands with its coding lease in the four lane workflow", Land: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "reviewed source integrates during finalization", Land: true, Integrate: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "NativeCompletion no-change reviewed head already delivered", Land: true, Integrate: true, IntegrateHead: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "prepared completion restart allows inline landing runtime", LandingRestart: true, Land: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", CheckpointHead: head, Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
		{Name: "NativeCompletion differing final head already integrated with a nonempty diff", Land: true, Integrate: true, IntegrateChanged: true, IntegrationIncorporated: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FinalMessage: "```detent-status\nschema: 1\nstatus: complete\nblockers: []\nhuman_action: null\n```"},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.SettlementChecks{})
}
