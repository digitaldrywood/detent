package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type analyticsResidenceSample struct {
	ID          string
	Created     time.Time
	At          time.Time
	AtNS        int64
	Initial     string
	State       string
	Entered     time.Time
	Started     *time.Time
	Partial     bool
	Events      int
	Transitions int
	Lanes       map[string]float64
	Queue       float64
	QueueTo     *time.Time
	LastDone    *time.Time
	MergeClaim  *time.Time
	Sequence    int64
	LanesNS     map[string]int64
	QueueNS     int64
	Causes      map[string]operatortool.AnalyticsReworkCause
}

func analyticsResidenceStates(ctx context.Context, q nativeQueryer, scope nativeScope) (map[string]tracker.NativeState, error) {
	rows, err := q.QueryContext(ctx, `SELECT detent_state,terminal,dispatchable FROM workflow_states WHERE project_id=? LIMIT 50`, scope.project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	states := map[string]tracker.NativeState{}
	for rows.Next() {
		var state tracker.NativeState
		if err := rows.Scan(&state.Name, &state.Terminal, &state.Dispatchable); err != nil {
			return nil, err
		}
		states[state.Name] = state
	}
	return states, rows.Err()
}

func readShippedResidenceIssues(ctx context.Context, q nativeQueryer, scope nativeScope, ids []string, to time.Time) ([]analyticsResidenceIssue, error) {
	issues := []analyticsResidenceIssue{}
	err := forShippedBatches(ids, func(batch string) error {
		rows, err := q.QueryContext(ctx, `SELECT i.native_id,i.created_at,coalesce(
(SELECT e.analytics_from_state FROM collaboration_events e
 WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned'
 ORDER BY e.sequence LIMIT 1),CASE WHEN EXISTS (
 SELECT 1 FROM collaboration_events c WHERE c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.type='issue.created'
 ) THEN ws.detent_state END,''),
(SELECT CASE WHEN e.analytics_projection_version IS NULL THEN e.data_json END FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.project_id=i.project_id AND e.work_item_id=i.native_id AND e.type='workflow.transitioned' ORDER BY e.sequence LIMIT 1)
FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
WHERE i.organization_id=? AND i.project_id=? AND i.native_id IN (SELECT value FROM json_each(?))
ORDER BY i.native_id`, scope.organization, scope.project, batch)
		if err != nil {
			return fmt.Errorf("read shipped issues: %w", err)
		}
		defer rows.Close()
		first := len(issues)
		byID := map[string]int{}
		for rows.Next() {
			var issue analyticsResidenceIssue
			var created string
			var unknown sql.NullString
			if err := rows.Scan(&issue.id, &created, &issue.initial, &unknown); err != nil {
				return fmt.Errorf("read shipped issues: %w", err)
			}
			if unknown.Valid {
				var data struct {
					FromState *string `json:"from_state"`
				}
				if err := decodeStorageJSON(ctx, []byte(unknown.String), &data); err != nil {
					return err
				}
				if data.FromState != nil {
					issue.initial = *data.FromState
				}
			}
			if issue.created, err = parseTimeValue(created); err != nil {
				return fmt.Errorf("decode shipped issue creation: %w", err)
			}
			byID[issue.id] = len(issues)
			issues = append(issues, issue)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return fmt.Errorf("read shipped issues: %w", err)
		}
		if len(issues) == first {
			return nil
		}
		events, err := q.QueryContext(ctx, `SELECT work_item_id,type,recorded_at,analytics_from_state,analytics_to_state,scheduler_source,scheduler_outcome,CASE WHEN analytics_projection_version IS NULL THEN data_json END FROM collaboration_events
WHERE organization_id=? AND project_id=? AND julianday(recorded_at)<julianday(?)
AND work_item_id IN (SELECT value FROM json_each(?))
AND (type='workflow.transitioned' OR (type='scheduler.decision' AND (scheduler_outcome IS NULL OR (scheduler_source='native_claim' AND scheduler_outcome='claimed'))))
ORDER BY work_item_id,sequence LIMIT ?`, scope.organization, scope.project, formatHubTime(to), batch, maxAnalyticsPopulation+1)
		if err != nil {
			return fmt.Errorf("read shipped transitions: %w", err)
		}
		defer events.Close()
		observed := 0
		for events.Next() {
			if observed == maxAnalyticsPopulation {
				for i := first; i < len(issues); i++ {
					issues[i].partial = true
				}
				break
			}
			observed++
			var id, at string
			var from, to, source, outcome, raw sql.NullString
			var event analyticsResidenceEvent
			if err := events.Scan(&id, &event.kind, &at, &from, &to, &source, &outcome, &raw); err != nil {
				return fmt.Errorf("read shipped transitions: %w", err)
			}
			if event.at, err = parseTimeValue(at); err != nil {
				return fmt.Errorf("decode shipped transition time: %w", err)
			}
			if raw.Valid {
				if err := decodeStorageJSON(ctx, []byte(raw.String), &event.data); err != nil {
					return fmt.Errorf("decode shipped transition: %w", err)
				}
			} else {
				event.data.FromState, event.data.ToState = from.String, to.String
				if event.kind == "scheduler.decision" {
					event.data.Decision = &tracker.NativeSchedulerDecision{Source: source.String, Outcome: outcome.String}
				}
			}
			if event.kind == "scheduler.decision" && (event.data.Decision == nil || event.data.Decision.Source != "native_claim" || event.data.Decision.Outcome != "claimed") {
				continue
			}
			if i, ok := byID[id]; ok {
				issues[i].events = append(issues[i].events, event)
			}
		}
		return events.Err()
	})
	return issues, err
}

const shippedBatchSize = 500

func forShippedBatches(ids []string, read func(string) error) error {
	for batch := range slices.Chunk(ids, shippedBatchSize) {
		encoded, err := json.Marshal(batch)
		if err != nil {
			return fmt.Errorf("encode shipped issues: %w", err)
		}
		if err := read(string(encoded)); err != nil {
			return err
		}
	}
	return nil
}
