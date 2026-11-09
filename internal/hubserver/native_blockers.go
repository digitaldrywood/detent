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

func prepareNativeDependencyBlockers(ctx context.Context, q nativeQueryer, scope nativeScope, issue tracker.NativeIssue, disposition *tracker.NativeDisposition) ([]tracker.NativeWorkItemID, error) {
	if disposition == nil || disposition.HumanAction {
		return nil, nil
	}
	refs := workpad.IssueDependencyRefs(&workpad.Signal{Status: disposition.Status, ReasonCode: disposition.ReasonCode, Blockers: disposition.BlockerEvidence}, string(scope.project))
	dependencies := make([]tracker.NativeWorkItemID, 0, len(refs))
	for _, ref := range refs {
		dependency, err := resolveNativeBlockerReference(ctx, q, scope, ref)
		if errors.Is(err, sql.ErrNoRows) || dependency.WorkItemID == issue.WorkItemID {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		dependencies = append(dependencies, dependency.WorkItemID)
	}
	return dependencies, nil
}

func recordNativeDependencyBlockers(ctx context.Context, tx *sql.Tx, scope nativeScope, issue tracker.NativeIssue, disposition *tracker.NativeDisposition, now time.Time) (tracker.NativeIssue, error) {
	dependencies, err := prepareNativeDependencyBlockers(ctx, tx, scope, issue, disposition)
	if err != nil {
		return issue, err
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
