package hubserver

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type productivityQuality struct {
	analyticsQualityInputs
	currentLandings []nativeAnalyticsLanding
}

type productivityHour struct {
	cost                     float64
	worked, reworked, landed int
}

func requireProductivityChangeColumns(ctx context.Context, query nativeQueryer, scope nativeScope, id string) error {
	var pending bool
	err := query.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM change_requests c WHERE c.organization_id=? AND c.project_id=? AND c.work_item_id=? AND (c.created_at IS NULL OR EXISTS(SELECT 1 FROM change_versions v WHERE v.change_id=c.id AND v.head_sha IS NULL) OR EXISTS(SELECT 1 FROM change_evidence e WHERE e.change_id=c.id AND e.has_validator IS NULL))) OR EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=? AND e.project_id=? AND e.work_item_id=? AND e.type='workflow.transitioned' AND (e.from_state IS NULL OR e.to_state IS NULL))`, scope.organization, scope.project, id, scope.organization, scope.project, id).Scan(&pending)
	if err != nil {
		return err
	}
	if !pending {
		err = query.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND report_projection_version IS NULL) OR EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='completion' AND projection_version IS NULL)`, scope.organization, scope.project, id, scope.organization, scope.project).Scan(&pending)
		if err != nil {
			return err
		}
	}
	if pending {
		return errChangeColumnsIncomplete
	}
	return nil
}

func refreshProductivitySummary(ctx context.Context, tx projectionQuery, scope nativeScope, id string) error {
	if err := requireProductivityChangeColumns(ctx, tx, scope, id); err != nil {
		return err
	}
	var created, closed, state, kind, priority, actorRaw, body, labelsRaw string
	err := tx.QueryRowContext(ctx, `SELECT i.created_at,coalesce(CASE WHEN ws.terminal=1 THEN `+nativeTerminalEnteredAt+` END,''),ws.detent_state,coalesce(i.issue_type,''),coalesce(CAST(q.priority_override AS TEXT),''),i.actor_json,i.body,i.labels_json
 FROM issues i JOIN workflow_states ws ON ws.id=i.workflow_state_id LEFT JOIN queue_entries q ON q.issue_id=i.id
 WHERE i.organization_id=? AND i.project_id=? AND i.native_id=?`, scope.organization, scope.project, id).Scan(&created, &closed, &state, &kind, &priority, &actorRaw, &body, &labelsRaw)
	if err != nil {
		return err
	}
	createdAt, err := parseTimeValue(created)
	if err != nil {
		return err
	}
	closedNS := int64(0)
	if closed != "" {
		at, err := parseTimeValue(closed)
		if err != nil {
			return err
		}
		closedNS = at.UnixNano()
	}
	var actor tracker.Actor
	if err := json.Unmarshal([]byte(actorRaw), &actor); err != nil {
		return err
	}
	filerKind := "unknown"
	_, machine := issueorigin.Parse(body)
	switch {
	case machine || actor.Kind == "machine" || actor.Kind == "automation" || actor.Kind == "system" || actor.Kind == "integration":
		filerKind = "automation"
	case actor.Kind == "agent" || actor.Kind == "runner" || actor.Agent != "":
		filerKind = "agents"
	case actor.Kind == "human":
		filerKind = "people"
	}
	completedKind, completedID, completedName := "unknown", "", ""
	if state == nativeShippedState {
		err = tx.QueryRowContext(ctx, `WITH final_attempt AS (
 SELECT organization_id,lease_id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND julianday(started_at)<=julianday(?) ORDER BY julianday(started_at) DESC,id DESC LIMIT 1
 ) SELECT CASE WHEN r.id IS NULL THEN 'unknown' ELSE 'runner' END,coalesce(r.id,''),coalesce(r.display_name,'')
 FROM final_attempt a LEFT JOIN leases l ON l.lease_id=a.lease_id LEFT JOIN lease_runners lr ON lr.lease_id=l.lease_id LEFT JOIN runner_identities r ON r.id=lr.runner_id AND r.organization_id=a.organization_id AND r.machine_id=l.machine_id`, scope.organization, scope.project, id, closed).Scan(&completedKind, &completedID, &completedName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var completionActor string
		err = tx.QueryRowContext(ctx, `SELECT actor_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='workflow.transitioned' AND to_state='Done' ORDER BY sequence DESC LIMIT 1`, scope.organization, scope.project, id).Scan(&completionActor)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			var who tracker.Actor
			if err := json.Unmarshal([]byte(completionActor), &who); err != nil {
				return err
			}
			if who.Kind == "human" && who.Agent == "" {
				completedKind, completedID, completedName = "people", who.PrincipalID, who.DisplayName
			}
		}
	}
	var cycle any
	var sampleRaw string
	err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='completion' AND sample_id=? ORDER BY hour DESC LIMIT 1`, scope.organization, scope.project, id).Scan(&sampleRaw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var sample analyticsResidenceSample
		if err := json.Unmarshal([]byte(sampleRaw), &sample); err != nil {
			return err
		}
		if !sample.Partial && sample.Started != nil && sample.LastDone != nil {
			cycle = sample.LastDone.Sub(*sample.Started).Seconds()
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO report_issue_summaries VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET created_ns=excluded.created_ns,closed_ns=excluded.closed_ns,state=excluded.state,issue_type=excluded.issue_type,priority=excluded.priority,filer_id=excluded.filer_id,filer_kind=excluded.filer_kind,completed_kind=excluded.completed_kind,completed_id=excluded.completed_id,completed_name=excluded.completed_name,cycle_seconds=excluded.cycle_seconds`, scope.organization, scope.project, id, createdAt.UnixNano(), closedNS, state, kind, priority, actor.PrincipalID, filerKind, completedKind, completedID, completedName, cycle)
	if err != nil {
		return err
	}
	for _, statement := range []string{
		"DELETE FROM report_issue_labels WHERE organization_id=? AND project_id=? AND work_item_id=?",
		"DELETE FROM report_issue_hours WHERE organization_id=? AND project_id=? AND work_item_id=?",
		"DELETE FROM report_issue_runners WHERE organization_id=? AND project_id=? AND work_item_id=?",
	} {
		if _, err := tx.ExecContext(ctx, statement, scope.organization, scope.project, id); err != nil {
			return err
		}
	}
	var labels []string
	if err := json.Unmarshal([]byte(labelsRaw), &labels); err != nil {
		return err
	}
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO report_issue_labels VALUES(?,?,?,?) ON CONFLICT DO NOTHING`, scope.organization, scope.project, id, label); err != nil {
			return err
		}
	}
	if err := refreshProductivityRunners(ctx, tx, scope, id); err != nil {
		return err
	}
	hours, err := productivityHours(ctx, tx, scope, id)
	if err != nil {
		return err
	}
	type hourRow struct {
		hour  int64
		value productivityHour
	}
	orderedHours := make([]hourRow, 0, len(hours))
	for hour, value := range hours {
		orderedHours = append(orderedHours, hourRow{hour, value})
	}
	slices.SortFunc(orderedHours, func(a, b hourRow) int { return cmp.Compare(a.hour, b.hour) })
	for _, row := range orderedHours {
		hour, value := row.hour, row.value
		if _, err := tx.ExecContext(ctx, `INSERT INTO report_issue_hours VALUES(?,?,?,?,?,?,?,?)`, scope.organization, scope.project, id, hour, value.cost, value.worked, value.reworked, value.landed); err != nil {
			return err
		}
	}
	return nil
}

func productivityHours(ctx context.Context, tx projectionQuery, scope nativeScope, id string) (map[int64]productivityHour, error) {
	hours := map[int64]productivityHour{}
	atHour := func(at time.Time) int64 {
		return at.UTC().Truncate(time.Hour).Unix()
	}
	rows, err := tx.QueryContext(ctx, `SELECT unixepoch(u.period)/3600*3600,sum(u.cost_estimate) FROM native_attempts a CROSS JOIN attempt_usage u ON +u.organization_id=a.organization_id AND +u.project_id=a.project_id AND u.attempt_id=a.id WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? GROUP BY 1`, scope.organization, scope.project, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var h int64
		var cost float64
		if err := rows.Scan(&h, &cost); err != nil {
			rows.Close()
			return nil, err
		}
		hour := atHour(time.Unix(h, 0))
		value := hours[hour]
		value.cost = cost
		hours[hour] = value
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	inputs, err := productivityQualityInputs(ctx, tx, scope, id)
	if err != nil {
		return nil, err
	}
	for _, landing := range inputs.landings {
		h := atHour(landing.Landing.LandedAt)
		value := hours[h]
		value.worked = 1
		hours[h] = value
	}
	for _, landing := range inputs.currentLandings {
		h := atHour(landing.Landing.LandedAt)
		value := hours[h]
		value.landed++
		hours[h] = value
	}
	for _, event := range inputs.events {
		h := atHour(event.At)
		value := hours[h]
		returned := (event.Kind == "changes_requested" || event.Data.ToState == "Rework") && qualityReturnedBeforeLanding(inputs.landings, event)
		if returned && (event.Kind == "changes_requested" || classifyReworkReason(event.Data.FromState, event.Data.ReasonDetail).Kind != reportKindInfrastructure) {
			value.reworked = 1
		}
		if returned || event.Kind == "worked" || event.Data.ToState == "In Progress" || event.Data.ToState == "Human Review" || event.Data.ToState == "Merging" {
			value.worked = 1
		}
		hours[h] = value
	}
	return hours, nil
}

func productivityQualityInputs(ctx context.Context, tx projectionQuery, scope nativeScope, id string) (productivityQuality, error) {
	out := productivityQuality{}
	if err := requireProductivityChangeColumns(ctx, tx, scope, id); err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.id,c.work_item_id,l.record_json,coalesce(c.landed_version_id=v.id AND c.landed_head_sha=v.head_sha AND length(c.landed_merge_sha) IN (40,64),0) FROM change_requests c JOIN change_versions v ON v.change_id=c.id JOIN quality_landings l ON l.version_id=v.id WHERE c.organization_id=? AND c.project_id=? AND c.work_item_id=?
 UNION ALL SELECT c.id,c.work_item_id,c.record_json,1 FROM change_requests c JOIN change_versions v ON v.change_id=c.id AND v.id=c.landed_version_id WHERE c.organization_id=? AND c.project_id=? AND c.work_item_id=? AND NOT EXISTS(SELECT 1 FROM quality_landings l WHERE l.version_id=v.id) AND c.landed_head_sha=v.head_sha AND length(c.landed_merge_sha) IN (40,64)`, scope.organization, scope.project, id, scope.organization, scope.project, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var landing nativeAnalyticsLanding
		var current bool
		var raw string
		if err := rows.Scan(&landing.ChangeID, &landing.WorkItemID, &raw, &current); err != nil {
			rows.Close()
			return out, err
		}
		if err := decodeQualityLanding(ctx, raw, &landing.Landing); err != nil {
			rows.Close()
			return out, err
		}
		out.landings = append(out.landings, landing)
		if current {
			out.currentLandings = append(out.currentLandings, landing)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT type,recorded_at,data_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND type='workflow.transitioned'
 UNION ALL SELECT 'worked',started_at,'{}' FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? AND coalesce(nullif(role,''),execution_role) IN ('code','rework')`, scope.organization, scope.project, id, scope.organization, scope.project, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		e := qualityHistoryEvent{Item: id}
		var at, raw string
		if err := rows.Scan(&e.Kind, &at, &raw); err != nil {
			rows.Close()
			return out, err
		}
		e.At, err = parseTimeValue(at)
		if err != nil {
			rows.Close()
			return out, err
		}
		if err := json.Unmarshal([]byte(raw), &e.Data); err != nil {
			rows.Close()
			return out, err
		}
		out.events = append(out.events, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return out, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT e.record_json FROM change_requests c JOIN change_evidence e ON e.change_id=c.id WHERE c.organization_id=? AND c.project_id=? AND c.work_item_id=? AND e.kind='review' AND e.claim_review_decision='changes_requested' AND NOT EXISTS(SELECT 1 FROM change_versions current JOIN change_versions newer ON newer.change_id=current.change_id AND newer.number>current.number WHERE current.id=e.version_id AND julianday(newer.created_at)<=julianday(e.created_at))`, scope.organization, scope.project, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var review tracker.ChangeReview
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return out, err
		}
		if err := json.Unmarshal([]byte(raw), &review); err != nil {
			rows.Close()
			return out, err
		}
		out.events = append(out.events, qualityHistoryEvent{Item: id, Kind: "changes_requested", VersionID: review.VersionID, At: review.CreatedAt})
	}
	return out, errors.Join(rows.Err(), rows.Close())
}

func refreshProductivityRunners(ctx context.Context, tx projectionQuery, scope nativeScope, id string) error {
	rows, err := tx.QueryContext(ctx, `SELECT r.id,a.started_at,a.updated_at FROM native_attempts a LEFT JOIN lease_runners lr ON lr.lease_id=a.lease_id JOIN runner_identities r ON r.id=coalesce(lr.runner_id,a.runner_id) AND r.organization_id=a.organization_id WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=?`, scope.organization, scope.project, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	type interval struct {
		runner           string
		started, updated int64
	}
	var intervals []interval
	for rows.Next() {
		var runner, start, end string
		if err := rows.Scan(&runner, &start, &end); err != nil {
			rows.Close()
			return err
		}
		from, err := parseTimeValue(start)
		if err != nil {
			rows.Close()
			return err
		}
		to, err := parseTimeValue(end)
		if err != nil {
			rows.Close()
			return err
		}
		intervals = append(intervals, interval{runner, from.UnixNano(), to.UnixNano()})
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	slices.SortFunc(intervals, func(a, b interval) int {
		if order := cmp.Compare(a.runner, b.runner); order != 0 {
			return order
		}
		return cmp.Compare(a.started, b.started)
	})
	merged := intervals[:0]
	for _, next := range intervals {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if last.runner == next.runner && next.started <= last.updated {
				last.updated = max(last.updated, next.updated)
				continue
			}
		}
		merged = append(merged, next)
	}
	for _, v := range merged {
		if _, err := tx.ExecContext(ctx, `INSERT INTO report_issue_runners VALUES(?,?,?,?,?,?)`, scope.organization, scope.project, id, v.runner, v.started, v.updated); err != nil {
			return err
		}
	}
	return nil
}
