package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func independentRepairRequest(failure failureattribution.Evidence, source string) tracker.CreateIssue {
	diagnostic := failure.Diagnostic
	fingerprint := issueorigin.DefectFingerprint(diagnostic)
	if fingerprint == "" {
		fingerprint = issueorigin.Fingerprint(failure.Command + "\n" + diagnostic)
	}
	body := fmt.Sprintf("## Acceptance criteria\n\nRepair the source failure reproduced on base `%s` by `%s` and add a focused regression.\n\n```text\n%s\n```\n\n## Must not break\n\nPreserve existing intended behavior, source verification and validation.\n\n## How we know it worked\n\nReproduce with the recorded command on the implementation-start base, then verify the regression and the recorded command pass.\n\nObserved change: %s\nHead: %s\nHead output digest: %s\nBase output digest: %s\n\n## Invariants touched\n\nPreserve the project invariants, source verification, and this report's fingerprint and origin. Repair the cause without adding a protection mechanism.\n\n## Safety regression\n\nPreserve existing safety regression tables and boundary seeds when the repair touches a safety-critical path.", failure.BaseSHA, failure.Command, diagnostic, source, failure.HeadSHA, failure.HeadOutputDigest, failure.BaseOutputDigest)
	body = issueorigin.Stamp(body, issueorigin.Origin{Kind: "worker", Instance: "outcome", Source: source, Fingerprint: fingerprint})
	return tracker.CreateIssue{Title: "Repair independent validation failure", Body: body, Type: "bug", State: "Todo", Priority: new(1)}
}

func routeIndependentReview(ctx context.Context, tx *sql.Tx, scope nativeScope, change tracker.ChangeRequest, review *tracker.ChangeReview, now time.Time) error {
	failures := review.IndependentFailures()
	if len(failures) == 0 || review.VersionID != change.CurrentVersion {
		return nil
	}
	issue, _, err := readNativeIssue(ctx, tx, scope, string(change.WorkItemID))
	if err != nil || issue.Terminal || issue.Archived {
		return err
	}
	dependencies, refused, err := attachIndependentRepairs(ctx, tx, scope, issue, failures, change.ID+"/"+review.ID, now)
	if err != nil {
		return err
	}
	review.RepairDependencies, review.RepairRefusal = dependencies, refused
	return nil
}

func attachIndependentRepairs(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, failures []failureattribution.Evidence, source string, now time.Time) ([]tracker.NativeWorkItemID, string, error) {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT independent_repairs"); err != nil {
		return nil, "", err
	}
	var dependencies []tracker.NativeWorkItemID
	seen := map[string]bool{}
	for _, failure := range failures {
		request := independentRepairRequest(failure, source)
		origin, _ := issueorigin.Parse(request.Body)
		if seen[origin.Fingerprint] {
			continue
		}
		seen[origin.Fingerprint] = true
		repairScope := scope
		repairScope.duplicateSearch = nil
		repair, err := recordNativeMachineOccurrenceTx(ctx, tx, repairScope, request, now, true)
		if err != nil {
			return nil, "", err
		}
		if scope.credential.Scope == apiScopeWorker {
			continue
		}
		if repair.Terminal || repair.WorkItemID == issue.WorkItemID {
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO independent_repairs"); err != nil {
				return nil, "", err
			}
			_, err = tx.ExecContext(ctx, "RELEASE independent_repairs")
			if repair.WorkItemID == issue.WorkItemID {
				return nil, "Dependencies cannot form a cycle", err
			}
			return nil, "Repair owner is terminal and cannot accept new work", err
		}
		updated, err := changeNativeDependencyTx(ctx, tx, scope, string(issue.WorkItemID), tracker.DependencyMutation{ExpectedRevision: issue.Revision, RelatedWorkItemID: repair.WorkItemID, Operation: "add"}, now)
		if err != nil {
			var invalid *nativeError
			if !errors.As(err, &invalid) || invalid.Message != "Dependencies cannot form a cycle" {
				return nil, "", err
			}
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO independent_repairs"); err != nil {
				return nil, "", err
			}
			_, err = tx.ExecContext(ctx, "RELEASE independent_repairs")
			return nil, "Dependencies cannot form a cycle", err
		}
		issue = updated
		dependencies = append(dependencies, repair.WorkItemID)
	}
	if len(dependencies) != 0 {
		if err := routeChangeForRework(ctx, tx, scope, issue, now); err != nil {
			return nil, "", err
		}
	}
	slices.Sort(dependencies)
	_, err := tx.ExecContext(ctx, "RELEASE independent_repairs")
	return dependencies, "", err
}

func recordIndependentRunFailure(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, data *tracker.NativeRunData, now time.Time) error {
	if data.Outcome != "failed" || data.Runtime == nil || data.Runtime.Validation == nil || data.Runtime.Landing != nil || data.Runtime.Validation.ExitCode == 0 || data.Runtime.Validation.BaseReproduction == nil || issue.Archived || issue.Terminal {
		return nil
	}
	command := data.Runtime.Validation
	var raw string
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(COALESCE(NULLIF(checkpoint_object_key,''),checkpoint_json),'null') FROM native_attempts WHERE id=?", data.AttemptID).Scan(&raw); err != nil {
		return err
	}
	checkpoint, err := decodeRunCheckpoint(ctx, tx, raw)
	if err != nil {
		return fmt.Errorf("decode failure source: %w", err)
	}
	if checkpoint == nil || checkpoint.HeadSHA != command.HeadSHA || checkpoint.UncertainForgeEffect() {
		return nil
	}
	result := &gate.ValidatorResult{Verdict: gate.ValidatorVerdictRework, BaseSHA: command.BaseReproduction.HeadSHA, HeadSHA: command.HeadSHA, Commands: []gate.CommandResult{*command}}
	gate.NormalizeAttribution(result)
	failures := tracker.IndependentGateFailures(result)
	if len(failures) == 0 {
		return nil
	}
	dependencies, _, err := attachIndependentRepairs(ctx, tx, scope, issue, failures, data.AttemptID, now)
	if err != nil {
		return err
	}
	data.Runtime.RepairDependencies = dependencies
	return nil
}
