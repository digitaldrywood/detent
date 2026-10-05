package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func readNativeBlockerHistory(ctx context.Context, q nativeQueryer, scope nativeScope, issue tracker.NativeIssue) ([]tracker.CollaborationEvent, error) {
	rows, err := q.QueryContext(ctx, `SELECT sequence, type, actor_json, data_json, recorded_at FROM collaboration_events
WHERE organization_id = ? AND project_id = ? AND work_item_id = ? AND json_extract(data_json, '$.revision') IS NOT NULL ORDER BY sequence`, scope.organization, scope.project, issue.WorkItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []tracker.CollaborationEvent
	for rows.Next() {
		event := tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: issue.WorkItemID}
		var actor, data, at string
		if err := rows.Scan(&event.AggregateSequence, &event.Type, &actor, &data, &at); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(actor), &event.Actor); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &event.Data); err != nil {
			return nil, err
		}
		if event.RecordedAt, err = parseTimeValue(at); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func resolveNativeBlockerReference(ctx context.Context, q nativeQueryer, scope nativeScope, reference string) (tracker.NativeIssue, error) {
	parsed, err := workpad.ParseRef(reference, string(scope.project))
	if err != nil {
		return tracker.NativeIssue{}, err
	}
	project, number, _ := strings.Cut(parsed, "#")
	if !strings.HasPrefix(project, "prj_") {
		return tracker.NativeIssue{}, nativeNotFound()
	}
	condition, grantArgs := scope.credential.projectGrantSQL("i.organization_id", "i.project_id")
	args := append([]any{scope.organization, project, number}, grantArgs...)
	var id string
	if err := q.QueryRowContext(ctx, `SELECT i.native_id FROM issues i WHERE i.organization_id = ? AND i.project_id = ? AND i.number = ? AND (`+condition+`)`, args...).Scan(&id); err != nil {
		return tracker.NativeIssue{}, err
	}
	scope.project = tracker.ProjectID(project)
	issue, _, err := readNativeIssue(ctx, q, scope, id)
	return issue, err
}

func validateNativeRecordedRecovery(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, request tracker.Transition, now time.Time) error {
	var latest string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ? ORDER BY fencing_token DESC LIMIT 1`, scope.organization, scope.project, issue.WorkItemID).Scan(&latest); err != nil {
		return err
	}
	if latest != request.BlockerAttemptID {
		return tracker.ErrStaleFencingToken
	}
	attempt, err := readNativeAttempt(ctx, tx, scope, string(issue.WorkItemID), latest, now)
	if err != nil {
		return err
	}
	if request.LeaseID != attempt.LeaseID || request.FencingToken != attempt.FencingToken {
		return tracker.ErrStaleFencingToken
	}
	var fence tracker.FencingToken
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(l.fencing_token), 0), COALESCE(max(l.released_at IS NULL AND julianday(l.expires_at) > julianday(?)), 0) FROM leases l JOIN issues i ON i.id = l.issue_id WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ?`, formatHubTime(now), scope.organization, scope.project, issue.WorkItemID).Scan(&fence, &active); err != nil {
		return err
	}
	if fence != attempt.FencingToken || active {
		return tracker.ErrStaleFencingToken
	}
	if err := requireLeaseRunner(ctx, tx, attempt.LeaseID, scope); err != nil {
		return err
	}
	if _, err := validateClaimPolicy(ctx, tx, claimCandidateQuery{NativeScope: &scope, PolicyID: request.PolicyID}, attempt.MachineID); err != nil {
		return err
	}
	approval, err := readProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project))
	if err != nil {
		return err
	}
	workflow, err := config.ApplyNativePolicy(config.Workflow{}, approval.Policy)
	if err != nil {
		return err
	}
	if !workflow.Config.Operator.ActionEnabled(config.OperatorActionReturnRetiredParks) {
		return policyMismatch("Recorded blocker recovery is not enabled by the selected policy")
	}
	history, err := readNativeBlockerHistory(ctx, tx, scope, issue)
	if err != nil {
		return err
	}
	_, prior, valid := tracker.RecordedNativeBlockers(issue, []tracker.NativeAttempt{attempt}, history)
	disposition := attempt.Disposition
	if !valid || prior != request.State || disposition.HumanAction || disposition.ReasonCode != "" || !disposition.Blockers || len(disposition.BlockerEvidence) == 0 || strings.ReplaceAll(request.ReasonDetail, " ", "_") != "recorded_blocker_recovery" {
		return nativeInvalid("Recorded blockers do not authorize recovery")
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(project.States, func(state tracker.NativeState) bool {
		return state.Name == prior && state.Dispatchable && !state.Terminal && !state.OperatorOnly
	}) {
		return nativeInvalid("Recorded blocker return lane is not dispatchable")
	}
	for _, blocker := range disposition.BlockerEvidence {
		if blocker.Unverifiable || blocker.Owner != workpad.BlockerOwnerOrchestrator || blocker.Predicate == nil || blocker.Predicate.Type != workpad.PredicateIssueState {
			return nativeInvalid("Recorded blocker cannot be verified by native dependencies")
		}
		reference := blocker.Predicate.Identifier
		if reference == "" {
			reference = blocker.Ref
		}
		dependency, err := resolveNativeBlockerReference(ctx, tx, scope, reference)
		if err != nil {
			return err
		}
		ready := dependency.Terminal
		if len(blocker.Predicate.States) > 0 {
			ready = ready && !slices.ContainsFunc(blocker.Predicate.States, func(state string) bool {
				return strings.EqualFold(state, dependency.State) || strings.EqualFold(state, "closed") && dependency.Terminal || strings.EqualFold(state, "open") && !dependency.Terminal
			})
		}
		if !ready {
			return nativeInvalid("Recorded dependency remains unfinished")
		}
	}
	var satisfied bool
	if err := tx.QueryRowContext(ctx, `SELECT `+nativeCandidateDependenciesSatisfied+` FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ?`, scope.organization, scope.project, issue.WorkItemID).Scan(&satisfied); err != nil {
		return err
	}
	if !satisfied {
		return nativeInvalid("Native dependencies remain unfinished")
	}
	return nil
}
