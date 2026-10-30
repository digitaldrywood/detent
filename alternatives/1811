package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func readNativeBlockerHistory(ctx context.Context, q nativeQueryer, scope nativeScope, issue tracker.NativeIssue, attempt tracker.NativeAttempt) ([]tracker.CollaborationEvent, error) {
	rows, err := q.QueryContext(ctx, `WITH report AS (
 SELECT sequence FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='run.finished' ORDER BY sequence DESC LIMIT 1
), lane AS (
 SELECT sequence FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='workflow.transitioned' ORDER BY sequence DESC LIMIT 1
), evidence AS (
 SELECT sequence FROM report UNION SELECT sequence FROM lane
)
SELECT e.sequence,e.type,e.actor_json,`+collaborationPayloadColumn("e")+`,e.recorded_at,e.page_projection_ready,e.event_revision
FROM evidence JOIN collaboration_events e ON e.organization_id=? AND e.project_id=? AND e.work_item_id=? AND e.sequence=evidence.sequence ORDER BY e.sequence`,
		scope.organization, scope.project, issue.WorkItemID, scope.organization, scope.project, issue.WorkItemID,
		scope.organization, scope.project, issue.WorkItemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type record struct {
		event           tracker.CollaborationEvent
		actor, data, at string
		ready, revision sql.NullInt64
	}
	var records []record
	for rows.Next() {
		row := record{event: tracker.CollaborationEvent{OrganizationID: scope.organization, ProjectID: scope.project, AggregateID: issue.WorkItemID}}
		if err := rows.Scan(&row.event.AggregateSequence, &row.event.Type, &row.actor, &row.data, &row.at, &row.ready, &row.revision); err != nil {
			return nil, err
		}
		records = append(records, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var events []tracker.CollaborationEvent
	for _, row := range records {
		event := row.event
		if err := json.Unmarshal([]byte(row.actor), &event.Actor); err != nil {
			return nil, err
		}
		if err := decodeRunHistory(ctx, q, row.data, &event.Data); err != nil {
			return nil, err
		}
		if row.ready.Valid && row.ready.Int64 == 1 {
			event.Data.Revision = tracker.Revision(row.revision.Int64)
		}
		if event.Data.Revision == 0 {
			continue
		}
		if event.RecordedAt, err = parseTimeValue(row.at); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	var reportSequence, laneSequence int64
	reportRevision := attempt.WorkItemRevision
	for _, event := range events {
		switch event.Type {
		case "run.finished":
			reportSequence = event.AggregateSequence
			if event.Actor.Kind == "runner" && event.Data.Run != nil && event.Data.Run.AttemptID == attempt.AttemptID && event.Data.Run.FencingToken == attempt.FencingToken {
				reportRevision = max(reportRevision, event.Data.Revision)
			}
		case "workflow.transitioned":
			laneSequence = event.AggregateSequence
		}
	}
	if reportSequence == 0 {
		return events, nil
	}
	var invalidatingSequence int64
	err = q.QueryRowContext(ctx, `SELECT sequence FROM collaboration_events
WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<>?
AND (page_projection_ready IS NULL OR (event_revision>? AND event_revision<=?
 AND (type<>'workflow.transitioned' OR actor_kind IS NOT 'runner' OR reason IS NOT 'worker_progress')))
ORDER BY sequence LIMIT 1`, scope.organization, scope.project, issue.WorkItemID, reportSequence, laneSequence, reportRevision, issue.Revision).Scan(&invalidatingSequence)
	if err == nil {
		return nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return events, nil
}

type nativeRecordedRecovery struct {
	attempt tracker.NativeAttempt
	prior   string
}

func (s *Service) prepareNativeRecordedRecovery(ctx context.Context, scope nativeScope, item string, request tracker.Transition) (nativeRecordedRecovery, error) {
	ctx = s.freshRunDetailContext(ctx)
	issue, _, err := readNativeIssue(ctx, s.database.reader, scope, item)
	if err != nil {
		return nativeRecordedRecovery{}, err
	}
	if err := requireNativeEdit(issue, request.ExpectedRevision); err != nil {
		return nativeRecordedRecovery{}, err
	}
	var latest string
	if err := s.database.reader.QueryRowContext(ctx, `SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? ORDER BY fencing_token DESC LIMIT 1`, scope.organization, scope.project, issue.WorkItemID).Scan(&latest); err != nil {
		return nativeRecordedRecovery{}, err
	}
	if latest != request.BlockerAttemptID {
		return nativeRecordedRecovery{}, tracker.ErrStaleFencingToken
	}
	attempt, err := readNativeAttemptRecord(ctx, s.database.reader, scope, string(issue.WorkItemID), request.BlockerAttemptID, s.config.now())
	if err != nil {
		return nativeRecordedRecovery{}, err
	}
	if attempt.Status != "succeeded" || attempt.Disposition == nil || attempt.Disposition.Status != workpad.StatusBlocked {
		return nativeRecordedRecovery{}, nativePublicInvalid("Recorded blockers do not authorize recovery")
	}
	history, err := readNativeBlockerHistory(ctx, s.database.reader, scope, issue, attempt)
	if err != nil {
		return nativeRecordedRecovery{}, err
	}
	if !slices.ContainsFunc(history, func(event tracker.CollaborationEvent) bool {
		return event.Type == "run.finished" && event.Data.Run != nil && event.Data.Run.AttemptID == attempt.AttemptID && event.Data.Run.FencingToken == attempt.FencingToken
	}) {
		return nativeRecordedRecovery{}, nativePublicInvalid("Recorded blockers do not authorize recovery")
	}
	_, prior, valid := tracker.RecordedNativeBlockers(issue, []tracker.NativeAttempt{attempt}, history)
	if !valid {
		return nativeRecordedRecovery{}, nativePublicInvalid("Recorded blockers do not authorize recovery")
	}
	return nativeRecordedRecovery{attempt: attempt, prior: prior}, nil
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

func normalizeNativeDependencyBlockers(ctx context.Context, q nativeQueryer, scope nativeScope, disposition *tracker.NativeDisposition) error {
	if disposition == nil || disposition.Status != workpad.StatusBlocked || disposition.HumanAction || disposition.ReasonCode != "" || len(disposition.BlockerEvidence) == 0 {
		return nil
	}
	integration, err := readProjectIntegration(ctx, q, scope)
	if err != nil {
		return err
	}
	repository := integration.Repository
	if repository == "" {
		repository = integration.CheckoutRepository
	}
	if repository == "" {
		return nil
	}
	for i := range disposition.BlockerEvidence {
		blocker := &disposition.BlockerEvidence[i]
		if blocker.Owner != workpad.BlockerOwnerOrchestrator || blocker.Unverifiable || blocker.Predicate == nil || blocker.Predicate.Type != workpad.PredicateIssueState {
			continue
		}
		for _, reference := range []*string{&blocker.Ref, &blocker.Predicate.Identifier} {
			parsed, err := workpad.ParseRef(*reference, string(scope.project))
			if err != nil {
				continue
			}
			if number, ok := strings.CutPrefix(parsed, repository+"#"); ok {
				*reference = string(scope.project) + "#" + number
			}
		}
	}
	return nil
}

func recordNativeDependencyBlockers(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, disposition *tracker.NativeDisposition, now time.Time) (tracker.NativeIssue, error) {
	if disposition == nil || disposition.HumanAction {
		return issue, nil
	}
	refs := workpad.IssueDependencyRefs(&workpad.Signal{Status: disposition.Status, ReasonCode: disposition.ReasonCode, Blockers: disposition.BlockerEvidence}, string(scope.project))
	dependencies := make([]tracker.NativeWorkItemID, 0, len(refs))
	for _, ref := range refs {
		dependency, err := resolveNativeBlockerReference(ctx, tx, scope, ref)
		if errors.Is(err, sql.ErrNoRows) || dependency.WorkItemID == issue.WorkItemID {
			return issue, nil
		}
		if err != nil {
			return issue, err
		}
		dependencies = append(dependencies, dependency.WorkItemID)
	}
	for _, id := range dependencies {
		if slices.Contains(issue.Dependencies, id) {
			continue
		}
		updated, err := changeNativeDependencyTx(ctx, tx, scope, string(issue.WorkItemID), tracker.DependencyMutation{ExpectedRevision: issue.Revision, RelatedWorkItemID: id, Operation: "add"}, now)
		if err != nil {
			return issue, err
		}
		issue = updated
	}
	return issue, nil
}

func validateNativeRecordedRecovery(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, request tracker.Transition, prepared nativeRecordedRecovery, now time.Time) error {
	attempt := prepared.attempt
	var latest, status string
	var sequence int64
	var revision tracker.Revision
	if err := tx.QueryRowContext(ctx, `SELECT id,sequence,work_item_revision,status FROM native_attempts WHERE organization_id = ? AND project_id = ? AND work_item_id = ? ORDER BY fencing_token DESC LIMIT 1`, scope.organization, scope.project, issue.WorkItemID).Scan(&latest, &sequence, &revision, &status); err != nil {
		return err
	}
	if latest != request.BlockerAttemptID || latest != attempt.AttemptID || sequence != attempt.Sequence || revision != attempt.WorkItemRevision || status != attempt.Status {
		return tracker.ErrStaleFencingToken
	}
	if request.LeaseID != attempt.LeaseID || request.FencingToken != attempt.FencingToken {
		return tracker.ErrStaleFencingToken
	}
	var recorded, active bool
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(l.lease_id = ? AND l.fencing_token = ?), 0), COALESCE(max(l.released_at IS NULL AND julianday(l.expires_at) > julianday(?)), 0) FROM leases l JOIN issues i ON i.id = l.issue_id WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ?`, attempt.LeaseID, attempt.FencingToken, formatHubTime(now), scope.organization, scope.project, issue.WorkItemID).Scan(&recorded, &active); err != nil {
		return err
	}
	if !recorded || active {
		return tracker.ErrStaleFencingToken
	}
	if !instanceOwnedBlockers(attempt.Disposition) {
		if err := requireLeaseRunner(ctx, tx, attempt.LeaseID, scope); err != nil {
			return err
		}
	}
	if _, err := validateClaimPolicy(ctx, tx, claimCandidateQuery{NativeScope: &scope, PolicyID: request.PolicyID}, attempt.MachineID); err != nil {
		return err
	}
	if _, err := requireProjectPolicy(ctx, tx, string(scope.organization)+"/"+string(scope.project)); err != nil {
		return err
	}
	prior := prepared.prior
	disposition := attempt.Disposition
	reason := strings.ReplaceAll(request.ReasonDetail, " ", "_")
	if prior != request.State || disposition.HumanAction || disposition.ReasonCode != "" && (disposition.ReasonCode != "instance_limitation" || !instanceOwnedBlockers(disposition)) || !disposition.Blockers || len(disposition.BlockerEvidence) == 0 || reason != "recorded_blocker_recovery" && reason != "dependency_auto_unblock" {
		return nativePublicInvalid("Recorded blockers do not authorize recovery")
	}
	project, err := readNativeProject(ctx, tx, scope)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(project.States, func(state tracker.NativeState) bool {
		return state.Name == prior && state.Dispatchable && !state.Terminal && !state.OperatorOnly
	}) {
		return nativePublicInvalid("Recorded blocker return lane is not dispatchable")
	}
	for _, blocker := range disposition.BlockerEvidence {
		if blocker.Owner == workpad.BlockerOwnerInstance {
			continue
		}
		if blocker.Unverifiable || blocker.Owner != workpad.BlockerOwnerOrchestrator || blocker.Predicate == nil || blocker.Predicate.Type != workpad.PredicateIssueState {
			return nativePublicInvalid("Recorded blocker cannot be verified by native dependencies")
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
			return nativePublicInvalid("Recorded dependency remains unfinished")
		}
	}
	var satisfied bool
	if err := tx.QueryRowContext(ctx, `SELECT `+nativeCandidateDependenciesSatisfied+` FROM issues i JOIN projects p ON p.id = i.project_id WHERE i.organization_id = ? AND i.project_id = ? AND i.native_id = ?`, scope.organization, scope.project, issue.WorkItemID).Scan(&satisfied); err != nil {
		return err
	}
	if !satisfied {
		return nativePublicInvalid("Native dependencies remain unfinished")
	}
	return nil
}

func instanceOwnedBlockers(disposition *tracker.NativeDisposition) bool {
	if disposition == nil || len(disposition.BlockerEvidence) == 0 {
		return false
	}
	for _, blocker := range disposition.BlockerEvidence {
		if blocker.Owner != workpad.BlockerOwnerInstance {
			return false
		}
	}
	return true
}
