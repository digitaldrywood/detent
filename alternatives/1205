package changecreation_test

import (
	"context"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeCreationSettlement(t *testing.T) {
	base := scenariotest.FixtureHead("a")
	head := scenariotest.FixtureHead("c")
	scenariotest.RunSettlement(t, []scenariotest.SettlementScenario{
		{Name: "canceled deferred validation preserves authority", ValidationErr: context.Canceled, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", CheckpointHead: head, Source: scenariotest.FixtureDiff(head, "README.md"), FailCreate: true, WantDiagnostic: "change creation unavailable"},
		{Name: "timed out deferred validation preserves authority", ValidationErr: context.DeadlineExceeded, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", CheckpointHead: head, Source: scenariotest.FixtureDiff(head, "README.md"), FailCreate: true, WantDiagnostic: "change creation unavailable"},
		{Name: "excluded checkpoint path is a permanent publication refusal", SourceRefusal: true, Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), WantDiagnostic: `excluded source path ".detent/skills/split-issue.md"`},
		{Name: "commits open a change", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"),
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 1},
		{Name: "rework reuses the item's change", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true,
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 1},
		{Name: "rework publishes a new head as the next version", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true, Published: scenariotest.FixtureHead("b"),
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 2},
		{Name: "rework keeps the version that already carries its head", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true, Published: head,
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 1},
		{Name: "clean preserved current head retains its deliverable with an empty attempt diff", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "unpushed", Source: func(context.Context) (tracker.AttemptDiffRequest, bool) {
			return tracker.AttemptDiffRequest{BaseSHA: head, HeadSHA: head, Files: []tracker.AttemptDiffFile{}}, true
		}, Existing: true, Published: head,
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head}, WantChanges: 1, WantVersions: 1},
		{Name: "removed clean worktree reuses its matching final checkpoint diff", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true, Published: head, UnreadFinal: true, CheckpointHead: head,
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 1},
		{Name: "final checkpoint refreshes the diff after a late commit before cleanup", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(scenariotest.FixtureHead("b"), "README.md"), RefreshFinal: true, UnreadFinal: true, CheckpointHead: head,
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 1},
		{Name: "final checkpoint advances past the stored diff before cleanup", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(scenariotest.FixtureHead("b"), "README.md"), RefreshFinal: true, UnreadFinal: true, CheckpointHead: scenariotest.FixtureHead("b"),
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 1},
		{Name: "an unavailable final diff cannot reuse an older checkpoint head", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true, Published: head, UnreadFinal: true, CheckpointHead: scenariotest.FixtureHead("b"),
			WantDiagnostic: "checkpoint head", WantChanges: 1, WantVersions: 1},
		{Name: "rework preserves the same head after configuration changes", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true, Published: head, Repolicy: true,
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 1},
		{Name: "changed review requirements republish the same source", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true, Published: head, Repolicy: true, Reviewer: true,
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 2},
		{Name: "a policy that asks for a reviewer leaves the version waiting", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), Reviewer: true,
			WantChange: &runner.NativeChange{Changed: true, BaseSHA: base, HeadSHA: head, Files: 1}, WantChanges: 1, WantVersions: 1},
		{Name: "a remote no https URL names opens the change without a version", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), NoRemote: true,
			WantDiagnostic: "https", WantChanges: 1},
		{Name: "a dirty worktree takes the ordinary path", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "dirty", Source: scenariotest.FixtureDiff(base, "scratch.txt")},
		{Name: "committed work left dirty takes the ordinary path", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "dirty", Source: scenariotest.FixtureDiff(head, "README.md")},
		{Name: "an unread final worktree cannot succeed", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unknown", Source: scenariotest.FixtureDiff(head, "README.md"), WantDiagnostic: "checkpoint is unavailable"},
		{Name: "an unstored final diff is not reviewable", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FailDiff: true,
			WantDiagnostic: "diff storage unavailable"},
		{Name: "a refused create reports the commits without a change", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), FailCreate: true,
			WantDiagnostic: "change creation unavailable"},
		{Name: "a refused new version cannot succeed", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true, Published: scenariotest.FixtureHead("b"), FailVersion: true,
			WantDiagnostic: "version publication unavailable", WantChanges: 1, WantVersions: 1},
		{Name: "a lost lease decides nothing", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md"), LoseLease: true},
		{Name: "a failed run decides nothing", Role: runner.RoleCode, Outcome: "failed", Worktree: "unpushed", Source: scenariotest.FixtureDiff(head, "README.md")},
		{Name: "no readable worktree cannot succeed", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "clean", WantDiagnostic: "diff source is unavailable"},
		{Name: "version acknowledgment loss retains the current immutable version", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true, Published: scenariotest.FixtureHead("b"), DropVersion: true, WantDiagnostic: "version acknowledgment lost", WantChanges: 1, WantVersions: 2},
		{Name: "version allowance refusal retains the previous immutable version", Role: runner.RoleRework, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), Existing: true, Published: scenariotest.FixtureHead("b"), VersionCode: "allowance_exhausted", WantDiagnostic: "allowance_exhausted", WantChanges: 1, WantVersions: 1},
		{Name: "diff allowance refusal is a publication diagnostic", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), DiffCode: "allowance_exhausted", WantDiagnostic: "allowance_exhausted"},
		{Name: "fenced diff refusal retains lost authority", Role: runner.RoleCode, Outcome: "succeeded", Worktree: "clean", Source: scenariotest.FixtureDiff(head, "README.md"), DiffCode: "stale_fencing_token", WantDiagnostic: "stale_fencing_token"},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.SettlementChecks{})
}
