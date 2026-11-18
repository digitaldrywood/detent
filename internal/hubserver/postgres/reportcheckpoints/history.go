package reportcheckpoints

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reports/residence"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const historyDue = "(k.history_sequence=-1 OR k.history_sequence-k.history_through>=256)"

type historyBlock struct {
	through                                         int64
	created                                         time.Time
	initial                                         string
	key                                             string
	count                                           int
	min, max                                        *time.Time
	unclassified                                    bool
	reopenIDs, reopenVersions, reopenFrom, reopenTo []string
	reopenAt                                        []time.Time
	base                                            reportstore.HistoryBase
}

const blockColumns = "through_sequence,created_at,initial_state,definition_key,segment_events,segment_min_at,segment_max_at,segment_unclassified,reopen_ids,reopen_versions,reopen_times,reopen_from,reopen_to,through_recorded_at,state,entered_at,started_at,queue_cut_at,partial,lane_names,lane_seconds,system_seconds"

func (b *historyBlock) scan(row pgx.Row, prefix ...any) error {
	var lanes []string
	var seconds []float64
	base := &b.base
	err := row.Scan(append(prefix, &b.through, &b.created, &b.initial, &b.key, &b.count, &b.min, &b.max, &b.unclassified, &b.reopenIDs, &b.reopenVersions, &b.reopenAt, &b.reopenFrom, &b.reopenTo, &base.Last, &base.State, &base.Entered, &base.Started, &base.QueueCut, &base.Partial, &lanes, &seconds, &base.SystemSeconds)...)
	if err != nil {
		return err
	}
	if lanes == nil {
		return nil
	}
	base.LaneSeconds = make(map[string]float64, len(lanes))
	for i, lane := range lanes {
		base.LaneSeconds[lane] = seconds[i]
	}
	return nil
}

func historyStateKey(states []tracker.NativeState) string {
	parts := make([]string, len(states))
	for i, state := range states {
		parts[i] = fmt.Sprintf("%d:%s%t%t", len(state.Name), state.Name, state.Terminal, state.Dispatchable)
	}
	slices.Sort(parts)
	return strings.Join(parts, "")
}

func historyStep(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, project string) error {
	var item string
	var through, head, policy int64
	err := db.QueryRow(ctx, `SELECT k.work_item_id,CASE WHEN k.history_policy_revision=c.history_policy_revision THEN k.history_through ELSE 0 END,k.history_sequence,c.history_policy_revision FROM report_issue_checkpoints k JOIN report_checkpoint_cursors c USING (organization_id,project_id)
 WHERE k.organization_id=$1 AND k.project_id=$2 AND (`+historyDue+` OR k.history_through>0 AND k.history_policy_revision<c.history_policy_revision) ORDER BY k.work_item_id LIMIT 1 FOR UPDATE OF k`, organization, project).Scan(&item, &through, &head, &policy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("select due history checkpoint: %w", err)
	}
	if head == -1 {
		_, err := db.Exec(ctx, `UPDATE report_issue_checkpoints SET history_sequence=coalesce((SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND type IN ('workflow.transitioned','scheduler.decision') ORDER BY sequence DESC LIMIT 1),0) WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, organization, project, item)
		return err
	}
	var issue reportstore.HistoryIssue
	err = db.QueryRow(ctx, `SELECT i.created_at,coalesce((SELECT e.from_state FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.work_item_id=$3 AND e.type='workflow.transitioned' ORDER BY e.sequence LIMIT 1),
 CASE WHEN EXISTS (SELECT 1 FROM collaboration_events e WHERE e.organization_id=$1 AND e.project_id=$2 AND e.work_item_id=$3 AND e.type='issue.created') THEN i.workflow_state_name END,'') FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3`, organization, project, item).Scan(&issue.CreatedAt, &issue.InitialState)
	if err != nil {
		return fmt.Errorf("read history checkpoint issue: %w", err)
	}
	rows, err := db.Query(ctx, `SELECT name,terminal,dispatchable FROM project_workflow_states WHERE organization_id=$1 AND project_id=$2 ORDER BY name`, organization, project)
	if err != nil {
		return err
	}
	definitions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (tracker.NativeState, error) {
		var state tracker.NativeState
		err := row.Scan(&state.Name, &state.Terminal, &state.Dispatchable)
		return state, err
	})
	if err != nil {
		return err
	}
	key := historyStateKey(definitions)
	if through > 0 {
		var previous historyBlock
		err := previous.scan(db.QueryRow(ctx, "SELECT "+blockColumns+" FROM report_history_checkpoints WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND through_sequence=$4", organization, project, item, through))
		switch {
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		case err == nil && previous.key == key && previous.initial == issue.InitialState && previous.created.Equal(issue.CreatedAt):
			issue.Base = &previous.base
		default:
			through = 0
		}
	}
	states := make(map[string]tracker.NativeState, len(definitions))
	for _, state := range definitions {
		states[state.Name] = state
	}
	return storeHistoryBlock(ctx, db, organization, project, item, through, head, policy, key, issue, states)
}

func storeHistoryBlock(ctx context.Context, db pgquery.DBTX, organization tracker.OrganizationID, project, item string, through, head, policy int64, key string, issue reportstore.HistoryIssue, states map[string]tracker.NativeState) error {
	var first int64
	err := db.QueryRow(ctx, `SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND sequence>$4 AND type IN ('workflow.transitioned','scheduler.decision') ORDER BY sequence LIMIT 1`, organization, project, item, through).Scan(&first)
	if err != nil {
		return err
	}
	end := head / FeedBatch * FeedBatch
	if first <= end {
		end = ((first-1)/FeedBatch + 1) * FeedBatch
	}
	block := historyBlock{through: end, reopenIDs: []string{}, reopenVersions: []string{}, reopenFrom: []string{}, reopenTo: []string{}, reopenAt: []time.Time{}}
	rows, err := db.Query(ctx, `SELECT e.id,e.version_id,e.recorded_at,e.type,e.from_state,e.to_state,coalesce(f.rework_class,'') FROM collaboration_events e LEFT JOIN report_event_facts f ON e.to_state='Rework' AND f.organization_id=e.organization_id AND f.event_id=e.id
 WHERE e.organization_id=$1 AND e.project_id=$2 AND e.work_item_id=$3 AND e.sequence>$4 AND e.sequence<=$5 AND e.type IN ('workflow.transitioned','scheduler.decision') ORDER BY e.sequence`, organization, project, item, through, end)
	if err != nil {
		return err
	}
	for rows.Next() {
		var event reportstore.WorkflowEvent
		if err := rows.Scan(&event.ID, &event.VersionID, &event.At, &event.Type, &event.FromState, &event.ToState, &event.Class.Name); err != nil {
			rows.Close()
			return err
		}
		issue.Events = append(issue.Events, event)
		block.unclassified = block.unclassified || event.ToState == "Rework" && event.Class.Name == ""
		if event.Type == "workflow.transitioned" && strings.EqualFold(event.FromState, "Done") && !strings.EqualFold(event.ToState, "Done") {
			block.reopenIDs = append(block.reopenIDs, event.ID)
			block.reopenVersions = append(block.reopenVersions, event.VersionID)
			block.reopenAt = append(block.reopenAt, event.At)
			block.reopenFrom = append(block.reopenFrom, event.FromState)
			block.reopenTo = append(block.reopenTo, event.ToState)
		}
		if block.min == nil || event.At.Before(*block.min) {
			block.min = &event.At
		}
		if block.max == nil || event.At.After(*block.max) {
			block.max = &event.At
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	base := residence.Checkpoint(issue, states)
	lanes := slices.Sorted(maps.Keys(base.LaneSeconds))
	seconds := make([]float64, len(lanes))
	for i, lane := range lanes {
		seconds[i] = base.LaneSeconds[lane]
	}
	if base.LaneSeconds == nil {
		lanes, seconds = nil, nil
	} else if lanes == nil {
		lanes = []string{}
	}
	_, err = db.Exec(ctx, `INSERT INTO report_history_checkpoints (organization_id,project_id,work_item_id,`+blockColumns+`)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25)
 ON CONFLICT (organization_id,project_id,work_item_id,through_sequence) DO UPDATE SET
 (created_at,initial_state,definition_key,segment_events,segment_min_at,segment_max_at,segment_unclassified,reopen_ids,reopen_versions,reopen_times,reopen_from,reopen_to,through_recorded_at,state,entered_at,started_at,queue_cut_at,partial,lane_names,lane_seconds,system_seconds)=
 (EXCLUDED.created_at,EXCLUDED.initial_state,EXCLUDED.definition_key,EXCLUDED.segment_events,EXCLUDED.segment_min_at,EXCLUDED.segment_max_at,EXCLUDED.segment_unclassified,EXCLUDED.reopen_ids,EXCLUDED.reopen_versions,EXCLUDED.reopen_times,EXCLUDED.reopen_from,EXCLUDED.reopen_to,EXCLUDED.through_recorded_at,EXCLUDED.state,EXCLUDED.entered_at,EXCLUDED.started_at,EXCLUDED.queue_cut_at,EXCLUDED.partial,EXCLUDED.lane_names,EXCLUDED.lane_seconds,EXCLUDED.system_seconds)`, organization, project, item, end, issue.CreatedAt, issue.InitialState, key, len(issue.Events), block.min, block.max, block.unclassified, block.reopenIDs, block.reopenVersions, block.reopenAt, block.reopenFrom, block.reopenTo, base.Last, base.State, base.Entered, base.Started, base.QueueCut, base.Partial, lanes, seconds, base.SystemSeconds)
	if err != nil {
		return fmt.Errorf("store report history checkpoint: %w", err)
	}
	_, err = db.Exec(ctx, `UPDATE report_issue_checkpoints SET history_through=$4,history_policy_revision=$5 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, organization, project, item, end, policy)
	return err
}
