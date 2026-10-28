package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func refreshAnalyticsIssue(ctx context.Context, tx *sql.Tx, statements analyticsStatements, id string, now time.Time) error {
	var organization, project string
	var sequence int64
	if err := tx.QueryRowContext(ctx, "SELECT i.organization_id,i.project_id,max(i.event_sequence,coalesce((SELECT max(e.sequence) FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.work_item_id=i.native_id),0)) FROM issues i WHERE i.native_id=?", id).Scan(&organization, &project, &sequence); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, "DELETE FROM analytics_rollup_positions WHERE work_item_id=?", id)
		}
		return err
	}
	var unknown bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND analytics_projection_version IS NULL)", organization, project, id).Scan(&unknown); err != nil {
		return err
	}
	if unknown {
		return errors.New("analytics event projection incomplete")
	}
	var through int64
	err := tx.QueryRowContext(ctx, "SELECT sequence FROM analytics_rollup_positions WHERE organization_id=? AND project_id=? AND work_item_id=?", organization, project, id).Scan(&through)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := statements.exec(ctx, tx, analyticsEventsRefresh, organization, project, id, through, projectionUpper(nil)); err != nil {
		return err
	}
	if _, err := statements.exec(ctx, tx, analyticsIssuesRefresh, organization, project, id); err != nil {
		return err
	}
	if err := refreshAnalyticsUsage(ctx, tx, organization, project, id, through); err != nil {
		return err
	}
	if err := refreshAnalyticsAttempts(ctx, tx, statements, organization, project, id); err != nil {
		return err
	}
	if err := refreshAnalyticsSkipSamples(ctx, tx, nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}, id, through); err != nil {
		return err
	}
	if err := refreshAnalyticsClaims(ctx, tx, nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}, id, through); err != nil {
		return err
	}
	var residenceChanged bool
	if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=?) OR EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed')))`, organization, project, id, organization, project, id, through).Scan(&residenceChanged); err != nil {
		return err
	}
	if residenceChanged {
		if err := refreshAnalyticsResidence(ctx, tx, nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}, id, through); err != nil {
			return err
		}
	}
	if err := refreshAnalyticsLeaseSnapshots(ctx, tx, organization, project, id, now); err != nil {
		return err
	}
	if err := refreshProductivitySummary(ctx, tx, nativeScope{organization: tracker.OrganizationID(organization), project: tracker.ProjectID(project)}, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_positions WHERE work_item_id=? AND (organization_id<>? OR project_id<>?)", id, organization, project); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions(organization_id,project_id,work_item_id,sequence) VALUES(?,?,?,?) ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET sequence=excluded.sequence,failed=0`, organization, project, id, sequence)
	return err
}

func rebuildAnalyticsRollups(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	for _, statement := range []string{
		"DELETE FROM analytics_sample_hours WHERE organization_id=? AND project_id=?",
		"DELETE FROM analytics_residence_retention WHERE organization_id=? AND project_id=?",
		"DELETE FROM analytics_slot_retention WHERE organization_id=? AND project_id=?",
	} {
		if _, err := tx.ExecContext(ctx, statement, scope.organization, scope.project); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_backfill WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_positions WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		return err
	}
	if err := refreshAnalyticsUsage(ctx, tx, string(scope.organization), string(scope.project), "", 0); err != nil {
		return err
	}
	ids, err := nativePageIDs(ctx, tx, "SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND native_id IS NOT NULL", scope.organization, scope.project)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshAnalyticsIssue(ctx, tx, nil, id, now); err != nil {
			return err
		}
	}

	return nil
}

func (d *database) projectAnalyticsBatch(ctx context.Context, tx *sql.Tx, now time.Time, limit int) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_rollup_dirty WHERE NOT EXISTS(SELECT 1 FROM analytics_rollup_positions p WHERE p.work_item_id=analytics_rollup_dirty.work_item_id)
AND EXISTS(SELECT 1 FROM issues i JOIN analytics_rollup_backfill b ON b.organization_id=i.organization_id AND b.project_id=i.project_id WHERE i.native_id=analytics_rollup_dirty.work_item_id)`); err != nil {
		return fmt.Errorf("skip analytics items awaiting backfill: %w", err)
	}
	ids, err := nativePageIDs(ctx, tx, "SELECT work_item_id FROM analytics_rollup_dirty ORDER BY work_item_id LIMIT ?", limit)
	if err != nil {
		return fmt.Errorf("list analytics rollup work: %w", err)
	}
	var failures error
	for _, id := range ids {
		if err := refreshAnalyticsIssueIsolated(ctx, tx, d.analytics, id, now); err != nil {
			failures = errors.Join(failures, fmt.Errorf("refresh analytics item %s: %w", id, err))
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_dirty WHERE work_item_id=?", id); err != nil {
			return errors.Join(failures, fmt.Errorf("clear analytics rollup work: %w", err))
		}
	}
	return failures
}

func refreshAnalyticsIssueIsolated(ctx context.Context, tx *sql.Tx, statements analyticsStatements, id string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT analytics_rollup"); err != nil {
		return fmt.Errorf("open analytics rollup savepoint: %w", err)
	}
	err := refreshAnalyticsIssue(ctx, tx, statements, id, now)
	if err != nil {
		_, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO analytics_rollup")
		_, markErr := tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions(organization_id,project_id,work_item_id,sequence,failed) SELECT organization_id,project_id,native_id,0,1 FROM issues WHERE native_id=? ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET failed=1`, id)
		err = errors.Join(err, rollbackErr, markErr)
	}
	if _, releaseErr := tx.ExecContext(ctx, "RELEASE analytics_rollup"); releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("release analytics rollup savepoint: %w", releaseErr))
	}
	return err
}

func refreshAnalyticsLeaseSnapshots(ctx context.Context, tx *sql.Tx, organization, project, item string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, analyticsLeaseRefresh, organization, project, organization, project, item)
	if err != nil {
		return err
	}
	return writeAnalyticsSlotRows(ctx, tx, organization, project, rows, now)
}

func writeAnalyticsSlotRows(ctx context.Context, tx *sql.Tx, organization, project string, rows *sql.Rows, now time.Time) error {
	type lease struct {
		id, acquired, expires string
		released              sql.NullString
	}
	var leases []lease
	for rows.Next() {
		var l lease
		if err := rows.Scan(&l.id, &l.acquired, &l.released, &l.expires); err != nil {
			return errors.Join(err, rows.Close())
		}
		leases = append(leases, l)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var retained sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT max(through_hour) FROM analytics_slot_retention WHERE organization_id=? AND project_id=?", organization, project).Scan(&retained); err != nil {
		return err
	}
	for _, l := range leases {
		start, err := parseTimeValue(l.acquired)
		if err != nil {
			return err
		}
		finish, err := parseTimeValue(l.expires)
		if err != nil {
			return err
		}
		if l.released.Valid {
			released, err := parseTimeValue(l.released.String)
			if err != nil {
				return err
			}
			finish = minTime(finish, released)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='slots' AND dimension=? AND hour>=?", organization, project, l.id, retained.Int64); err != nil {
			return err
		}
		horizon := now.Truncate(time.Hour).Add(analyticsSlotHorizon)
		for hour := maxTime(start.Truncate(time.Hour), time.Unix(retained.Int64, 0)); hour.Before(finish) && hour.Before(horizon); hour = hour.Add(time.Hour) {
			seconds := minTime(finish, hour.Add(time.Hour)).Sub(maxTime(start, hour)).Seconds()
			if seconds <= 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO analytics_rollup_legacy(organization_id,project_id,hour,metric,dimension,value,data_json,acquired_at,released_at,expires_at,projection_version) VALUES(?,?,?,'slots',?,?,'{}',?,?,?,1)", organization, project, hour.Unix(), l.id, seconds/3600, l.acquired, l.released, l.expires); err != nil {
				return err
			}
		}
	}
	return nil
}

const (
	analyticsSourceIssues  = `WITH analytics_source_issues AS (SELECT organization_id,project_id,unixepoch(created_at)/3600*3600 AS hour,'issue' AS metric,native_id AS dimension,1 AS value,'{}' AS data_json,id AS issue_id,native_id,title,created_at,updated_at,workflow_state_id FROM issues WHERE organization_id IS NOT NULL AND project_id IS NOT NULL AND native_id IS NOT NULL) `
	analyticsSourceEvents  = `WITH analytics_source_events AS (SELECT e.organization_id,e.project_id,unixepoch(e.recorded_at)/3600*3600 AS hour,'event' AS metric,e.id AS dimension,1 AS value,'{}' AS data_json,e.work_item_id,e.sequence,e.type,e.recorded_at,e.scheduler_outcome,e.scheduler_source,e.scheduler_reason,e.analytics_to_state,e.analytics_from_state,e.analytics_attempt_id,e.analytics_skip_key AS skip_key,e.analytics_recorded_ns AS sample_ns,julianday(e.recorded_at) AS recorded_day FROM collaboration_events e WHERE e.type IN ('workflow.transitioned','scheduler.decision','issue.created') AND e.analytics_projection_version=1) `
	analyticsSourceLeases  = `WITH analytics_source_leases AS (SELECT i.organization_id,i.project_id,unixepoch(l.acquired_at)/3600*3600 AS hour,'lease' AS metric,l.lease_id AS dimension,1 AS value,'{}' AS data_json,l.lease_id,l.issue_id,l.acquired_at,l.released_at,l.expires_at FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.native_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM analytics_slot_retention p WHERE p.organization_id=i.organization_id AND p.project_id=i.project_id AND unixepoch(coalesce(l.released_at,l.expires_at))<=p.through_hour)) `
	analyticsEventsRefresh = analyticsSourceEvents + `INSERT INTO analytics_rollup_legacy(organization_id,project_id,hour,metric,dimension,value,data_json,work_item_id,sequence,type,recorded_at,scheduler_outcome,scheduler_source,scheduler_reason,analytics_to_state,analytics_from_state,analytics_attempt_id,skip_key,sample_ns,recorded_day,projection_version) SELECT organization_id,project_id,hour,metric,dimension,value,data_json,work_item_id,sequence,type,recorded_at,scheduler_outcome,scheduler_source,scheduler_reason,analytics_to_state,analytics_from_state,analytics_attempt_id,skip_key,sample_ns,recorded_day,1 FROM analytics_source_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<=? ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO NOTHING`
	analyticsIssuesRefresh = analyticsSourceIssues + `INSERT INTO analytics_rollup_legacy(organization_id,project_id,hour,metric,dimension,value,data_json,issue_id,native_id,title,created_at,updated_at,workflow_state_id,projection_version) SELECT organization_id,project_id,hour,metric,dimension,value,data_json,issue_id,native_id,title,created_at,updated_at,workflow_state_id,1 FROM analytics_source_issues WHERE organization_id=? AND project_id=? AND dimension=? ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json,issue_id=excluded.issue_id,native_id=excluded.native_id,title=excluded.title,created_at=excluded.created_at,updated_at=excluded.updated_at,workflow_state_id=excluded.workflow_state_id,projection_version=1`
	analyticsLeaseRefresh  = analyticsSourceLeases + `INSERT INTO analytics_rollup_legacy(organization_id,project_id,hour,metric,dimension,value,data_json,lease_id,issue_id,acquired_at,released_at,expires_at,projection_version) SELECT organization_id,project_id,hour,metric,dimension,value,data_json,lease_id,issue_id,acquired_at,released_at,expires_at,1 FROM analytics_source_leases WHERE organization_id=? AND project_id=? AND issue_id=(SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?) ON CONFLICT DO UPDATE SET hour=excluded.hour,data_json=excluded.data_json,lease_id=excluded.lease_id,issue_id=excluded.issue_id,acquired_at=excluded.acquired_at,released_at=excluded.released_at,expires_at=excluded.expires_at,projection_version=1 RETURNING dimension,acquired_at,released_at,expires_at`
)

func refreshAnalyticsAttempts(ctx context.Context, tx *sql.Tx, statements analyticsStatements, organization, project, item string) error {
	query := `SELECT id,work_item_id,lease_id,status,started_at,updated_at,data_json FROM native_attempts WHERE organization_id=? AND project_id=?`
	args := []any{organization, project}
	if item != "" {
		query += " AND work_item_id=?"
		args = append(args, item)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read analytics attempt sources: %w", err)
	}
	defer rows.Close()
	type projectedAttempt struct {
		id, payload string
		hour        int64
	}
	var attempts []projectedAttempt
	for rows.Next() {
		var id, workItem, lease, status, started, updated, raw string
		var failed sql.NullString
		var observation analyticsAttemptProjection
		if err := rows.Scan(&id, &workItem, &lease, &status, &started, &updated, &raw); err != nil {
			return fmt.Errorf("scan analytics attempt source: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &observation); err != nil {
			return err
		}
		failed = analyticsFailureTime(observation.Runtime.Landing, status, updated)
		payload, err := projectAnalyticsAttempt(raw, id, workItem, lease, status, started, updated, failed)
		if err != nil {
			return err
		}
		at, err := parseTimeValue(started)
		if err != nil {
			return err
		}
		attempts = append(attempts, projectedAttempt{id, payload, at.Unix() / 3600 * 3600})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, attempt := range attempts {
		if err := writeAnalyticsSample(ctx, tx, organization, project, attempt.hour, "attempt", attempt.id, 1, attempt.payload); err != nil {
			return fmt.Errorf("write analytics attempt projection: %w", err)
		}
	}
	return nil
}

func writeAnalyticsResidenceSample(ctx context.Context, tx nativeExecer, scope nativeScope, metric, dimension string, sample analyticsResidenceSample) error {
	sample.AtNS = sample.At.UnixNano()
	raw, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	err = writeAnalyticsSample(ctx, tx, string(scope.organization), string(scope.project), sample.At.Truncate(time.Hour).Unix(), metric, dimension, float64(sample.Events), string(raw))
	return err
}

func refreshAnalyticsResidence(ctx context.Context, tx projectionQuery, scope nativeScope, id string, after int64, before ...int64) error {
	states, err := analyticsResidenceStates(ctx, tx, scope)
	if err != nil {
		return err
	}
	var created, initial string
	err = tx.QueryRowContext(ctx, `SELECT i.created_at,coalesce((SELECT analytics_from_state FROM collaboration_events WHERE organization_id=i.organization_id AND project_id=i.project_id AND work_item_id=i.native_id AND type='workflow.transitioned' ORDER BY sequence LIMIT 1),CASE WHEN EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=i.organization_id AND work_item_id=i.native_id AND type='issue.created') THEN ws.detent_state END,'') FROM issues i LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id=?`, scope.organization, scope.project, id).Scan(&created, &initial)
	if err != nil {
		return err
	}
	start, err := parseTimeValue(created)
	if err != nil {
		return err
	}
	sample := analyticsResidenceSample{ID: id, Created: start, At: start, Initial: initial, State: initial, Entered: start, Partial: initial == "", Lanes: map[string]float64{}, LanesNS: map[string]int64{}, Causes: map[string]operatortool.AnalyticsReworkCause{}}
	var previous string
	if len(before) == 0 {
		err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? ORDER BY hour DESC LIMIT 1`, scope.organization, scope.project, id).Scan(&previous)
	} else {
		var prior int64
		err = tx.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed'))`, scope.organization, scope.project, id, after).Scan(&prior)
		if err != nil {
			return err
		}
		err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?`, scope.organization, scope.project, id+":"+strconv.FormatInt(prior, 10)).Scan(&previous)
		if errors.Is(err, sql.ErrNoRows) {
			err = tx.QueryRowContext(ctx, `SELECT data_json FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? ORDER BY hour DESC LIMIT 1`, scope.organization, scope.project, id).Scan(&previous)
			if err == nil {
				var retained analyticsResidenceSample
				if err := json.Unmarshal([]byte(previous), &retained); err != nil {
					return err
				}
				if retained.Sequence > after {
					err = sql.ErrNoRows
				}
			}
		}
	}
	fresh := errors.Is(err, sql.ErrNoRows)
	if err != nil && !fresh {
		return err
	}
	if !fresh {
		if err := json.Unmarshal([]byte(previous), &sample); err != nil {
			return err
		}
		fresh = sample.Initial != initial
		if fresh && len(before) > 0 {
			return errAnalyticsResidenceReset
		}
	}
	if fresh {
		after = 0
		sample = analyticsResidenceSample{ID: id, Created: start, At: start, Initial: initial, State: initial, Entered: start, Partial: initial == "", Lanes: map[string]float64{}, LanesNS: map[string]int64{}, Causes: map[string]operatortool.AnalyticsReworkCause{}}
		if states[initial].Dispatchable && analyticsResidenceGroup(initial, states) == "system" {
			sample.Started = &start
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric IN ('residence_hour','residence_edge','completion','residence_merge','merge_entry') AND sample_id=?`, scope.organization, scope.project, id); err != nil {
			return err
		}
	}
	write := func(sequence int64) error {
		if err := writeAnalyticsResidenceSample(ctx, tx, scope, "residence_hour", id, sample); err != nil {
			return err
		}
		return writeAnalyticsResidenceSample(ctx, tx, scope, "residence_edge", id+":"+strconv.FormatInt(sequence, 10), sample)
	}
	if fresh {
		if err := write(0); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT sequence,type,recorded_at,data_json FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed')) ORDER BY sequence`, scope.organization, scope.project, id, max(sample.Sequence, after), projectionUpper(before))
	if err != nil {
		return err
	}
	defer rows.Close()
	type event struct {
		sequence int64
		analyticsResidenceEvent
	}
	var events []event
	for rows.Next() {
		var e event
		var at, raw string
		if err := rows.Scan(&e.sequence, &e.kind, &at, &raw); err != nil {
			return err
		}
		e.at, err = parseTimeValue(at)
		if err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &e.data); err != nil {
			return err
		}
		events = append(events, e)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	mergeClaim := sample.MergeClaim
	writeMerge := func(to time.Time) error {
		if sample.Started == nil {
			return nil
		}
		raw, err := json.Marshal(analyticsMergeSample{FromNS: maxTime(sample.Entered, *sample.Started).UnixNano(), ID: id, From: maxTime(sample.Entered, *sample.Started), To: to, Claim: mergeClaim})
		if err != nil {
			return err
		}
		err = writeAnalyticsSample(ctx, tx, string(scope.organization), string(scope.project), sample.Entered.Truncate(time.Hour).Unix(), "residence_merge", id+":"+strconv.FormatInt(sample.Entered.UnixNano(), 10), 1, string(raw))
		return err
	}
	for _, e := range events {
		sample.Sequence = e.sequence
		sample.Events++
		if e.kind == "workflow.transitioned" {
			sample.Transitions++
			if e.data.ToState == "Merging" {
				at := sample.At
				sample.At = e.at
				if err := writeAnalyticsResidenceSample(ctx, tx, scope, "merge_entry", id+":"+strconv.FormatInt(e.sequence, 10), sample); err != nil {
					return err
				}
				sample.At = at
			}
		}
		if e.at.Before(sample.At) {
			sample.Partial = true
			if err := write(e.sequence); err != nil {
				return err
			}
			continue
		}
		sample.At = e.at
		if e.kind == "scheduler.decision" {
			if sample.State == "Rework" && sample.QueueTo == nil {
				at := e.at
				sample.QueueTo = &at
			}
			if sample.State == "Merging" && mergeClaim == nil {
				at := e.at
				mergeClaim = &at
			}
		} else {
			if e.data.FromState != sample.State || e.data.ToState == "" {
				sample.Partial = true
			} else if e.data.ToState != sample.State && sample.Started != nil {
				from := maxTime(sample.Entered, *sample.Started)
				if analyticsResidenceGroup(sample.State, states) != "excluded" {
					sample.Lanes[sample.State] += max(0, e.at.Sub(from).Seconds())
					sample.LanesNS[sample.State] += max(int64(0), e.at.Sub(from).Nanoseconds())
				}
				if sample.State == "Todo" || sample.State == "Rework" {
					to := e.at
					if sample.QueueTo != nil {
						to = minTime(to, *sample.QueueTo)
					}
					sample.Queue += max(0, to.Sub(from).Seconds())
					sample.QueueNS += max(int64(0), to.Sub(from).Nanoseconds())
				}
				if sample.State == "Merging" {
					if err := writeMerge(e.at); err != nil {
						return err
					}
				}
			}
			if e.data.ToState != "" && e.data.ToState != sample.State {
				sample.State, sample.Entered = e.data.ToState, e.at
				sample.QueueTo = nil
				mergeClaim = nil
				if sample.Started == nil && states[sample.State].Dispatchable && analyticsResidenceGroup(sample.State, states) == "system" {
					at := e.at
					sample.Started = &at
				}
				if sample.State == "Done" {
					at := e.at
					sample.LastDone = &at
					if err := writeAnalyticsResidenceSample(ctx, tx, scope, "completion", id+":"+strconv.FormatInt(e.sequence, 10), sample); err != nil {
						return err
					}
				}
				if sample.State == "Rework" {
					key := e.data.FromState + "\x00" + e.data.ReasonDetail
					cause := sample.Causes[key]
					cause.FromState, cause.ReasonDetail = e.data.FromState, e.data.ReasonDetail
					cause.Count++
					sample.Causes[key] = cause
				}
			}
		}
		sample.MergeClaim = mergeClaim
		if err := write(e.sequence); err != nil {
			return err
		}
	}
	if sample.State == "Merging" && !sample.Partial {
		if err := writeMerge(time.Time{}); err != nil {
			return err
		}
	}
	return nil
}

func refreshAnalyticsClaims(ctx context.Context, tx projectionQuery, scope nativeScope, id string, through int64, before ...int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT e.id,e.data_json,e.recorded_at,coalesce(p.data_json,'null'),coalesce(p.type,''),coalesce(p.recorded_at,'') FROM collaboration_events e LEFT JOIN collaboration_events p ON p.organization_id=e.organization_id AND p.project_id=e.project_id AND p.work_item_id=e.work_item_id AND p.sequence=e.sequence-1 WHERE e.organization_id=? AND e.project_id=? AND e.work_item_id=? AND e.sequence>? AND e.sequence<=? AND e.type='scheduler.decision' AND e.scheduler_source='native_claim' AND e.scheduler_outcome='claimed'`, scope.organization, scope.project, id, through, projectionUpper(before))
	if err != nil {
		return err
	}
	defer rows.Close()
	var samples []analyticsCapacitySample
	for rows.Next() {
		var sample analyticsCapacitySample
		var raw, at, priorRaw, priorType, priorAt string
		if err := rows.Scan(&sample.ID, &raw, &at, &priorRaw, &priorType, &priorAt); err != nil {
			return err
		}
		sample.At, err = parseTimeValue(at)
		if err != nil {
			return err
		}
		sample.AtNS = sample.At.UnixNano()
		sample.Start, err = parseTimeValue(priorAt)
		var claim, prior tracker.CollaborationData
		sample.Valid = priorType == "scheduler.decision" && err == nil && json.Unmarshal([]byte(raw), &claim) == nil && json.Unmarshal([]byte(priorRaw), &prior) == nil && nativeAnalyticsCapacityInterval(prior.Decision, claim.Decision, sample.Start, sample.At)
		sample.StartNS = sample.Start.UnixNano()
		if sample.Valid {
			sample.SecondsNS = sample.At.Sub(sample.Start).Nanoseconds()
		}
		samples = append(samples, sample)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, sample := range samples {
		raw, err := json.Marshal(sample)
		if err != nil {
			return err
		}
		if err := writeAnalyticsSample(ctx, tx, string(scope.organization), string(scope.project), sample.At.Truncate(time.Hour).Unix(), "capacity_claim", sample.ID, 1, string(raw)); err != nil {
			return err
		}
		if !sample.Valid {
			continue
		}
		for hour := sample.Start.Truncate(time.Hour); hour.Before(sample.At); hour = hour.Add(time.Hour) {
			seconds := minTime(sample.At, hour.Add(time.Hour)).Sub(maxTime(sample.Start, hour)).Seconds()
			if err := writeAnalyticsSample(ctx, tx, string(scope.organization), string(scope.project), hour.Unix(), "capacity_wait", sample.ID, seconds, string(raw)); err != nil {
				return err
			}
		}
	}
	return nil
}

func refreshAnalyticsSkipSamples(ctx context.Context, tx projectionQuery, scope nativeScope, id string, through int64, before ...int64) error {
	rows, err := tx.QueryContext(ctx, `SELECT id,recorded_at,coalesce(scheduler_source,''),coalesce(scheduler_reason,'') FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<=? AND type='scheduler.decision' AND scheduler_outcome='skipped'`, scope.organization, scope.project, id, through, projectionUpper(before))
	if err != nil {
		return err
	}
	defer rows.Close()
	type row struct {
		id     string
		sample analyticsSkipSample
	}
	var samples []row
	for rows.Next() {
		var r row
		var at string
		if err := rows.Scan(&r.id, &at, &r.sample.Source, &r.sample.Reason); err != nil {
			return err
		}
		r.sample.At, err = parseTimeValue(at)
		if err != nil {
			return err
		}
		r.sample.AtNS = r.sample.At.UnixNano()
		samples = append(samples, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range samples {
		raw, err := json.Marshal(r.sample)
		if err != nil {
			return err
		}
		if err := writeAnalyticsSample(ctx, tx, string(scope.organization), string(scope.project), r.sample.At.Truncate(time.Hour).Unix(), "skip_sample", r.id, 1, string(raw)); err != nil {
			return err
		}
	}
	return nil
}

func refreshAnalyticsUsage(ctx context.Context, tx *sql.Tx, organization, project, item string, through int64) error {
	query := `WITH analytics_item_attempts(id) AS (SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? UNION SELECT analytics_attempt_id FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>?) SELECT u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency,u.updated_at FROM analytics_item_attempts a CROSS JOIN attempt_usage u WHERE u.attempt_id=a.id AND +u.organization_id=? AND +u.project_id=?`
	args := []any{organization, project, item, organization, project, item, through, organization, project}
	if item == "" {
		query = `SELECT attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at FROM attempt_usage WHERE organization_id=? AND project_id=?`
		args = []any{organization, project}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	return writeAnalyticsUsageRows(ctx, tx, organization, project, rows)
}

func writeAnalyticsUsageRows(ctx context.Context, tx nativeExecer, organization, project string, rows *sql.Rows) error {
	defer rows.Close()
	type usage struct {
		attempt, period, provider, model, currency, updated string
		input, cached, output                               int64
		cost                                                float64
	}
	var samples []usage
	for rows.Next() {
		var u usage
		if err := rows.Scan(&u.attempt, &u.period, &u.provider, &u.model, &u.input, &u.cached, &u.output, &u.cost, &u.currency, &u.updated); err != nil {
			return errors.Join(err, rows.Close())
		}
		samples = append(samples, u)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, u := range samples {
		key := analyticsDimension(u.attempt, u.period, u.provider, u.model, u.currency)
		at, err := parseTimeValue(u.period)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_rollup_legacy(organization_id,project_id,hour,metric,dimension,value,data_json,attempt_id,period,provider,model,input,cached_input,output,currency,updated_at,projection_version) VALUES(?,?,?,'usage',?,?,'{}',?,?,?,?,?,?,?,?,?,1) ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json,attempt_id=excluded.attempt_id,period=excluded.period,provider=excluded.provider,model=excluded.model,input=excluded.input,cached_input=excluded.cached_input,output=excluded.output,currency=excluded.currency,updated_at=excluded.updated_at,projection_version=1`, organization, project, at.Unix()/3600*3600, key, u.cost, u.attempt, u.period, u.provider, u.model, u.input, u.cached, u.output, u.currency, u.updated); err != nil {
			return err
		}
	}
	return nil
}

var analyticsProjectionFields = []struct{ column, key string }{
	{"work_item_id", "work_item_id"}, {"sequence", "sequence"}, {"type", "type"},
	{"recorded_at", "recorded_at"}, {"started_at", "started_at"}, {"updated_at", "updated_at"},
	{"attempt_id", "attempt_id"}, {"period", "period"}, {"sample_id", "ID"},
	{"sample_at", "At"}, {"sample_ns", "AtNS"}, {"sample_start_ns", "StartNS"}, {"sample_from_ns", "FromNS"},
	{"skip_source", "Source"}, {"skip_reason", "Reason"}, {"last_at", "last_at"}, {"last_ns", "last_ns"},
	{"issue_id", "id"}, {"native_id", "native_id"}, {"title", "title"}, {"created_at", "created_at"},
	{"workflow_state_id", "workflow_state_id"}, {"lease_id", "lease_id"}, {"status", "status"},
	{"provider", "provider"}, {"model", "model"}, {"input", "input"}, {"cached_input", "cached_input"},
	{"output", "output"}, {"currency", "currency"}, {"acquired_at", "acquired_at"},
	{"released_at", "released_at"}, {"expires_at", "expires_at"}, {"failed_at", "failed_at"},
	{"scheduler_outcome", "outcome"}, {"scheduler_source", "source"}, {"scheduler_reason", "reason"},
	{"analytics_to_state", "to_state"},
}

const analyticsSampleUpsert = `INSERT INTO analytics_rollup_legacy(organization_id,project_id,hour,metric,dimension,value,data_json,work_item_id,sequence,type,recorded_at,started_at,updated_at,attempt_id,period,sample_id,sample_at,sample_ns,sample_start_ns,sample_from_ns,skip_source,skip_reason,last_at,last_ns,issue_id,native_id,title,created_at,workflow_state_id,lease_id,status,provider,model,input,cached_input,output,currency,acquired_at,released_at,expires_at,failed_at,scheduler_outcome,scheduler_source,scheduler_reason,analytics_to_state,attempt_role,projection_version,failed_day,recorded_day) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,julianday(?),julianday(?)) ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json,work_item_id=excluded.work_item_id,sequence=excluded.sequence,type=excluded.type,recorded_at=excluded.recorded_at,started_at=excluded.started_at,updated_at=excluded.updated_at,attempt_id=excluded.attempt_id,period=excluded.period,sample_id=excluded.sample_id,sample_at=excluded.sample_at,sample_ns=excluded.sample_ns,sample_start_ns=excluded.sample_start_ns,sample_from_ns=excluded.sample_from_ns,skip_source=excluded.skip_source,skip_reason=excluded.skip_reason,last_at=excluded.last_at,last_ns=excluded.last_ns,issue_id=excluded.issue_id,native_id=excluded.native_id,title=excluded.title,created_at=excluded.created_at,workflow_state_id=excluded.workflow_state_id,lease_id=excluded.lease_id,status=excluded.status,provider=excluded.provider,model=excluded.model,input=excluded.input,cached_input=excluded.cached_input,output=excluded.output,currency=excluded.currency,acquired_at=excluded.acquired_at,released_at=excluded.released_at,expires_at=excluded.expires_at,failed_at=excluded.failed_at,scheduler_outcome=excluded.scheduler_outcome,scheduler_source=excluded.scheduler_source,scheduler_reason=excluded.scheduler_reason,analytics_to_state=excluded.analytics_to_state,attempt_role=excluded.attempt_role,projection_version=excluded.projection_version,failed_day=excluded.failed_day,recorded_day=excluded.recorded_day`

const analyticsProjectionBackfill = `UPDATE analytics_rollup_legacy SET work_item_id=?,sequence=?,type=?,recorded_at=?,started_at=?,updated_at=?,attempt_id=?,period=?,sample_id=?,sample_at=?,sample_ns=?,sample_start_ns=?,sample_from_ns=?,skip_source=?,skip_reason=?,last_at=?,last_ns=?,issue_id=?,native_id=?,title=?,created_at=?,workflow_state_id=?,lease_id=?,status=?,provider=?,model=?,input=?,cached_input=?,output=?,currency=?,acquired_at=?,released_at=?,expires_at=?,failed_at=?,scheduler_outcome=?,scheduler_source=?,scheduler_reason=?,analytics_to_state=?,attempt_role=?,projection_version=?,failed_day=julianday(?),recorded_day=julianday(?) WHERE rowid=? AND data_json=? AND projection_version IS NULL`

func analyticsProjection(raw string) ([]any, error) {
	var data map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		return nil, errors.New("decode analytics projection: invalid JSON object")
	}
	values := make([]any, 0, len(analyticsProjectionFields)+4)
	for _, field := range analyticsProjectionFields {
		var value any
		encoded := data[field.key]
		if field.column == "skip_source" && len(encoded) == 0 {
			encoded = data["source"]
		}
		if field.column == "skip_reason" && len(encoded) == 0 {
			encoded = data["reason"]
		}
		if field.column == "issue_id" && len(data["issue_id"]) > 0 {
			encoded = data["issue_id"]
		}
		if len(encoded) > 0 && string(encoded) != "null" {
			if strings.HasSuffix(field.column, "_ns") || field.column == "sequence" || field.column == "input" || field.column == "cached_input" || field.column == "output" {
				var n int64
				if err := json.Unmarshal(encoded, &n); err != nil {
					return nil, fmt.Errorf("decode analytics field %s: invalid type", field.key)
				}
				value = n
			} else if (field.column == "issue_id" || field.column == "workflow_state_id") && encoded[0] != '"' {
				var n int64
				if err := json.Unmarshal(encoded, &n); err != nil {
					return nil, fmt.Errorf("decode analytics field %s: expected integer or string", field.key)
				}
				value = strconv.FormatInt(n, 10)
			} else {
				var s string
				if err := json.Unmarshal(encoded, &s); err != nil {
					return nil, fmt.Errorf("decode analytics field %s: invalid type", field.key)
				}
				value = s
			}
		}
		values = append(values, value)
	}
	var identity struct {
		Identity struct{ Role *string }
		Runtime  struct{ Identity struct{ Role *string } }
	}
	if err := json.Unmarshal([]byte(raw), &identity); err != nil {
		return nil, errors.New("decode analytics projection: invalid identity or timestamp")
	}
	role := identity.Runtime.Identity.Role
	if role == nil {
		role = identity.Identity.Role
	}
	values = append(values, role, 1)
	for _, field := range []struct{ column, key string }{{"failed_day", "failed_at"}, {"recorded_day", "recorded_at"}} {
		var rawTime *string
		if len(data[field.key]) > 0 && string(data[field.key]) != "null" {
			if err := json.Unmarshal(data[field.key], &rawTime); err != nil {
				return nil, errors.New("decode analytics projection: invalid identity or timestamp")
			}
		}
		values = append(values, rawTime)
	}
	return values, nil
}

func writeAnalyticsSample(ctx context.Context, tx nativeExecer, organization, project string, hour int64, metric, dimension string, value float64, raw string) error {
	values, err := analyticsProjection(raw)
	if err != nil {
		return err
	}
	args := append([]any{organization, project, hour, metric, dimension, value, raw}, values...)
	_, err = tx.ExecContext(ctx, analyticsSampleUpsert, args...)
	return err
}

func (d *database) convertAnalyticsProjections(ctx context.Context) error {
	var after int64
	if err := d.reader.QueryRowContext(ctx, `SELECT projection_rowid FROM analytics_maintenance_cursor WHERE id=1`).Scan(&after); err != nil {
		return err
	}
	rows, err := d.reader.QueryContext(ctx, `SELECT rowid,data_json FROM analytics_rollup_legacy WHERE projection_version IS NULL AND rowid>? ORDER BY rowid LIMIT ?`, after, storageConversionRows)
	if err != nil {
		return err
	}
	defer rows.Close()
	type projectionRow struct {
		id  int64
		raw string
	}
	var batch []projectionRow
	for rows.Next() {
		var row projectionRow
		if err := rows.Scan(&row.id, &row.raw); err != nil {
			return err
		}
		batch = append(batch, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if len(batch) == 0 && after == 0 {
		return nil
	}
	var failures error
	for _, row := range batch {
		after = row.id
		values, err := analyticsProjection(row.raw)
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("analytics rollup rowid=%d projection: %w", row.id, err))
			continue
		}
		values = append(values, row.id, row.raw)
		if _, err := d.db.ExecContext(ctx, analyticsProjectionBackfill, values...); err != nil {
			return errors.Join(failures, err)
		}
	}
	if len(batch) < storageConversionRows {
		after = 0
	}
	_, err = d.db.ExecContext(ctx, `UPDATE analytics_maintenance_cursor SET projection_rowid=? WHERE id=1`, after)
	return errors.Join(failures, err)
}

func addAnalyticsHour(ctx context.Context, tx *sql.Tx, scope nativeScope, hour int64, metric, dimension string, count int, total, minimum, maximum float64, unavailable int, last int64) error {
	bucket := 0
	if maximum > 0 {
		bucket = int(math.Ceil(math.Log2(maximum+1) * 16))
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO analytics_sample_hours VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(organization_id,project_id,metric,hour,dimension,bucket) DO UPDATE SET count=count+excluded.count,total=total+excluded.total,minimum=min(minimum,excluded.minimum),maximum=max(maximum,excluded.maximum),unavailable=unavailable+excluded.unavailable,last_ns=max(last_ns,excluded.last_ns)`, scope.organization, scope.project, hour, metric, dimension, bucket, count, total, minimum, maximum, unavailable, last)
	return err
}

func (d *database) compactAnalyticsSamples(ctx context.Context, now time.Time) error {
	return d.compactAnalyticsSamplesWithClock(ctx, now, time.Now)
}

func (d *database) compactAnalyticsSamplesWithClock(ctx context.Context, now time.Time, clock func() time.Time) error {
	failures := d.convertAnalyticsProjections(ctx)
	through := now.UTC().Truncate(time.Hour).Add(-analyticsSampleRetention).Unix()
	after := analyticsSampleCursor{}
	if err := d.reader.QueryRowContext(ctx, `SELECT sample_hour,sample_metric,sample_dimension,sample_rowid,samples_done,residence_hour,residence_organization,residence_project,residence_id FROM analytics_maintenance_cursor WHERE id=1`).Scan(&after.hour, &after.metric, &after.dimension, &after.rowid, &after.samplesDone, &after.residenceHour, &after.residenceOrganization, &after.residenceProject, &after.residenceID); err != nil {
		return errors.Join(failures, err)
	}
	deadline := clock().Add(analyticsBackfillBudget)
	for range analyticsSampleCompactionBatch {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return errors.Join(failures, err)
		}
		if _, err := tx.ExecContext(ctx, "SAVEPOINT analytics_sample"); err != nil {
			return errors.Join(failures, err, tx.Rollback())
		}
		more, err := compactAnalyticsSample(ctx, tx, through, &after)
		if err != nil {
			var decode *analyticsSampleDecodeError
			if !errors.As(err, &decode) {
				return errors.Join(failures, fmt.Errorf("compact analytics sample: %w", err), tx.Rollback())
			}
			failures = errors.Join(failures, err)
			if _, err := tx.ExecContext(ctx, "ROLLBACK TO analytics_sample"); err != nil {
				return errors.Join(failures, err, tx.Rollback())
			}
			more = true
		}
		if !more {
			after = analyticsSampleCursor{hour: math.MinInt64, residenceHour: math.MinInt64}
		}
		_, err = tx.ExecContext(ctx, `UPDATE analytics_maintenance_cursor SET sample_hour=?,sample_metric=?,sample_dimension=?,sample_rowid=?,samples_done=?,residence_hour=?,residence_organization=?,residence_project=?,residence_id=? WHERE id=1`, after.hour, after.metric, after.dimension, after.rowid, after.samplesDone, after.residenceHour, after.residenceOrganization, after.residenceProject, after.residenceID)
		if err != nil {
			return errors.Join(failures, err, tx.Rollback())
		}
		if err := tx.Commit(); err != nil {
			return errors.Join(failures, err)
		}
		if !more || clock().After(deadline) {
			return failures
		}
	}
	return failures
}

type analyticsSampleCursor struct {
	samplesDone                                          bool
	hour                                                 int64
	metric, dimension                                    string
	rowid                                                int64
	residenceOrganization, residenceProject, residenceID string
	residenceHour                                        int64
}

type analyticsSampleDecodeError struct{ rowid int64 }

func (e *analyticsSampleDecodeError) Error() string {
	return fmt.Sprintf("decode analytics rollup rowid=%d: invalid sample payload", e.rowid)
}

func compactAnalyticsSample(ctx context.Context, tx *sql.Tx, through int64, after *analyticsSampleCursor) (bool, error) {
	if after.samplesDone {
		return compactAnalyticsResidenceAfter(ctx, tx, through, after)
	}
	var scope nativeScope
	var rowid, hour int64
	var metric, dimension, raw string
	var value float64
	err := tx.QueryRowContext(ctx, `SELECT rowid,organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_rollup_legacy r
WHERE (hour,metric,dimension,rowid)>(?,?,?,?) AND projection_version=1 AND hour<? AND (metric IN ('skip_sample','slots','capacity_claim') OR (metric='capacity_wait' AND sample_ns<?*1000000000) OR
(metric='lease' AND unixepoch(coalesce(released_at,expires_at))<?))
ORDER BY hour,metric,dimension,rowid LIMIT 1`, after.hour, after.metric, after.dimension, after.rowid, through, through, through).Scan(&rowid, &scope.organization, &scope.project, &hour, &metric, &dimension, &value, &raw)
	if err == nil {
		after.hour, after.metric, after.dimension, after.rowid = hour, metric, dimension, rowid
		switch metric {
		case "skip_sample":
			var exists bool
			var sample analyticsSkipSample
			if err := json.Unmarshal([]byte(raw), &sample); err != nil {
				return false, &analyticsSampleDecodeError{rowid: rowid}
			}
			key := analyticsDimension(sample.Source, sample.Reason)
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM analytics_rollup_legacy a WHERE a.organization_id=? AND a.project_id=? AND a.metric='skipped' AND a.hour=? AND a.dimension=?)`, scope.organization, scope.project, hour, key).Scan(&exists); err != nil {
				return false, err
			}
			if !exists {
				return false, errors.New("skip sample has no hourly total")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_rollup_legacy(organization_id,project_id,hour,metric,dimension,value,data_json,projection_version) VALUES(?,?,0,'skip_compacted','',?,'{}',1) ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=max(value,excluded.value)`, scope.organization, scope.project, through); err != nil {
				return false, err
			}
		case "slots":
			if err := addAnalyticsHour(ctx, tx, scope, hour, "slots", "", 1, value, value, value, 0, 0); err != nil {
				return false, err
			}
		case "capacity_claim", "capacity_wait":
			var sample analyticsCapacitySample
			if err := json.Unmarshal([]byte(raw), &sample); err != nil {
				return false, &analyticsSampleDecodeError{rowid: rowid}
			}
			if metric == "capacity_wait" {
				dimension = strconv.FormatInt(sample.At.Truncate(time.Hour).Unix(), 10)
				if err := addAnalyticsHour(ctx, tx, scope, hour, metric, dimension, 1, value, value, value, 0, sample.AtNS); err != nil {
					return false, err
				}
			} else {
				unavailable := 0
				if !sample.Valid {
					unavailable = 1
				}
				seconds := float64(sample.SecondsNS) / 1e9
				if err := addAnalyticsHour(ctx, tx, scope, hour, metric, "", 1, seconds, seconds, seconds, unavailable, sample.AtNS); err != nil {
					return false, err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_legacy WHERE rowid=?", rowid); err != nil {
			return false, err
		}
		if metric == "slots" {
			_, err = tx.ExecContext(ctx, `INSERT INTO analytics_slot_retention VALUES(?,?,?)
ON CONFLICT(organization_id,project_id) DO UPDATE SET through_hour=max(through_hour,excluded.through_hour)`, scope.organization, scope.project, hour+3600)
		}
		return true, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	after.samplesDone = true
	return compactAnalyticsResidenceAfter(ctx, tx, through, after)
}

func compactAnalyticsResidenceAfter(ctx context.Context, tx *sql.Tx, through int64, after *analyticsSampleCursor) (bool, error) {
	var scope nativeScope
	var id string
	var hour int64
	err := tx.QueryRowContext(ctx, `SELECT r.organization_id,r.project_id,r.dimension,coalesce(p.through_hour,min(r.hour)) AS next
FROM analytics_rollup_legacy r LEFT JOIN analytics_residence_retention p ON p.organization_id=r.organization_id AND p.project_id=r.project_id AND p.work_item_id=r.dimension
WHERE r.metric='residence_hour' AND r.hour<? GROUP BY r.organization_id,r.project_id,r.dimension HAVING next<? AND (next,r.dimension,r.organization_id,r.project_id)>(?,?,?,?) ORDER BY next,r.dimension,r.organization_id,r.project_id LIMIT 1`, through, through, after.residenceHour, after.residenceID, after.residenceOrganization, after.residenceProject).Scan(&scope.organization, &scope.project, &id, &hour)
	if errors.Is(err, sql.ErrNoRows) {
		return cleanupAnalyticsResidence(ctx, tx, through)
	}
	if err != nil {
		return false, err
	}
	from, to := time.Unix(hour, 0).UTC(), time.Unix(hour+3600, 0).UTC()
	states, err := analyticsResidenceStates(ctx, tx, scope)
	if err != nil {
		return false, err
	}
	frames := analyticsResidenceFrames{ids: []string{id}, issues: map[string]int{id: 0}, points: map[int64]int{from.UnixNano(): 0, to.UnixNano(): 1}, samples: make([]*analyticsResidenceSample, 2), states: states}
	for i, at := range []time.Time{from, to} {
		var raw string
		var rowid int64
		err := tx.QueryRowContext(ctx, `SELECT rowid,data_json FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? AND hour<? ORDER BY hour DESC LIMIT 1`, scope.organization, scope.project, id, at.Unix()).Scan(&rowid, &raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		var sample analyticsResidenceSample
		if err := json.Unmarshal([]byte(raw), &sample); err != nil {
			after.residenceHour = hour
			after.residenceOrganization, after.residenceProject, after.residenceID = string(scope.organization), string(scope.project), id
			return false, &analyticsSampleDecodeError{rowid: rowid}
		}
		frames.samples[i] = &sample
	}
	summary, _ := summarizeResidenceFrames(frames, from, to)
	end := frames.at(to, id)
	unavailable := 0
	if end.Partial {
		unavailable = 1
	}
	save := func(metric, dimension string, duration operatortool.AnalyticsDuration) error {
		for _, seconds := range duration.Samples {
			if err := addAnalyticsHour(ctx, tx, scope, hour, metric, dimension, 1, seconds, seconds, seconds, unavailable, end.AtNS); err != nil {
				return err
			}
		}
		return nil
	}
	for _, lane := range summary.Lanes {
		if err := save("residence_lane", lane.Lane, lane.AnalyticsDuration); err != nil {
			return false, err
		}
	}
	for _, d := range []struct {
		name  string
		value operatortool.AnalyticsDuration
	}{{"residence_system", summary.SystemTotal}, {"residence_held", summary.HeldTotal}, {"residence_queue", summary.QueueTotal}, {"residence_lead", summary.LeadTotal}} {
		if err := save(d.name, "", d.value); err != nil {
			return false, err
		}
	}
	for _, cause := range summary.ReworkCauses {
		key, err := json.Marshal([2]string{cause.FromState, cause.ReasonDetail})
		if err != nil {
			return false, err
		}
		if err := addAnalyticsHour(ctx, tx, scope, hour, "residence_cause", string(key), cause.Count, 0, 0, 0, unavailable, end.AtNS); err != nil {
			return false, err
		}
	}
	begin := frames.at(from, id)
	if err := addAnalyticsHour(ctx, tx, scope, hour, "residence_coverage", "", 1, float64(end.Transitions-begin.Transitions), 0, 0, unavailable, end.AtNS); err != nil {
		return false, err
	}
	todo := 0
	if end.State == "Todo" {
		todo = 1
	}
	if err := addAnalyticsHour(ctx, tx, scope, hour, "diagnostics_todo", "", 1, float64(todo), 0, 0, unavailable, end.AtNS); err != nil {
		return false, err
	}
	next := hour + 3600
	if states[end.State].Terminal {
		var future sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT min(hour) FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? AND hour>=?`, scope.organization, scope.project, id, next).Scan(&future); err != nil {
			return false, err
		}
		if !future.Valid {
			next = through
		} else {
			next = min(through, future.Int64)
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO analytics_residence_retention VALUES(?,?,?,?) ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET through_hour=excluded.through_hour`, scope.organization, scope.project, id, next)
	return true, err
}

func cleanupAnalyticsResidence(ctx context.Context, tx *sql.Tx, through int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `DELETE FROM analytics_rollup_legacy WHERE rowid IN (
SELECT r.rowid FROM analytics_rollup_legacy r JOIN analytics_residence_retention p ON p.organization_id=r.organization_id AND p.project_id=r.project_id AND p.work_item_id=CASE WHEN r.metric='residence_hour' THEN r.dimension ELSE r.sample_id END
WHERE r.hour<min(p.through_hour,?) AND (r.metric='residence_edge' OR
(r.metric='residence_hour' AND r.hour<(SELECT max(b.hour) FROM analytics_rollup_legacy b WHERE b.organization_id=r.organization_id AND b.project_id=r.project_id AND b.metric='residence_hour' AND b.dimension=r.dimension AND b.hour<min(p.through_hour,?)))) LIMIT ?)`, through, through, analyticsSampleCompactionBatch)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count > 0, err
}

type analyticsMergeSample struct {
	FromNS int64

	ID       string
	From, To time.Time
	Claim    *time.Time
}

func residenceSampleItem(sample analyticsResidenceSample, states map[string]tracker.NativeState, at time.Time) operatortool.AnalyticsIssueResidence {
	item := operatortool.AnalyticsIssueResidence{WorkItemID: sample.ID, SystemStartedAt: sample.Started, LaneSeconds: map[string]float64{}, QueueSeconds: sample.Queue, Partial: sample.Partial}
	for lane, seconds := range sample.Lanes {
		item.LaneSeconds[lane] = seconds
	}
	if sample.Started != nil && !sample.Partial {
		from := maxTime(sample.Entered, *sample.Started)
		if analyticsResidenceGroup(sample.State, states) != "excluded" {
			item.LaneSeconds[sample.State] += max(0, at.Sub(from).Seconds())
		}
		if sample.State == "Todo" || sample.State == "Rework" {
			to := at
			if sample.QueueTo != nil {
				to = minTime(to, *sample.QueueTo)
			}
			item.QueueSeconds += max(0, to.Sub(from).Seconds())
		}
	}
	for lane, seconds := range item.LaneSeconds {
		if analyticsResidenceGroup(lane, states) == "held" {
			item.HeldSeconds += seconds
		} else {
			item.SystemSeconds += seconds
		}
	}
	return item
}

type analyticsResidenceFrames struct {
	retained map[string]time.Time
	states   map[string]tracker.NativeState
	ids      []string
	points   map[int64]int
	issues   map[string]int
	samples  []*analyticsResidenceSample
	cutoffs  map[string]residenceCutoff
	clipped  bool
}

type residenceRow struct {
	hour, atNS int64
	value      float64
	raw        string
	sample     *analyticsResidenceSample
}

func (r *residenceRow) decode() (*analyticsResidenceSample, error) {
	if r.sample == nil {
		r.sample = &analyticsResidenceSample{}
		if err := json.Unmarshal([]byte(r.raw), r.sample); err != nil {
			return nil, err
		}
	}
	return r.sample, nil
}

type residenceCutoff struct {
	allowed int
	sample  analyticsResidenceSample
}

func (f analyticsResidenceFrames) sample(at time.Time, id string) (analyticsResidenceSample, bool) {
	point, ok := f.points[at.UnixNano()]
	issue, known := f.issues[id]
	if !ok || !known {
		return analyticsResidenceSample{}, false
	}
	found := f.samples[point*len(f.ids)+issue]
	if found == nil {
		return analyticsResidenceSample{}, false
	}
	sample := *found
	if cutoff, clipped := f.cutoffs[id]; clipped {
		if sample.Events > cutoff.allowed {
			sample = cutoff.sample
		}
		sample.Partial = true
	}
	return sample, true
}

func (f analyticsResidenceFrames) at(at time.Time, id string) analyticsResidenceSample {
	sample, _ := f.sample(at, id)
	return sample
}

func residenceHour(ns int64) int64 {
	return time.Unix(0, ns).Truncate(time.Hour).Unix()
}

func summarizeResidenceFrames(frames analyticsResidenceFrames, from, to time.Time) (operatortool.AnalyticsResidenceSummary, []operatortool.AnalyticsIssueResidence) {
	summary := operatortool.AnalyticsResidenceSummary{Lanes: []operatortool.AnalyticsLaneDuration{}, ReworkCauses: []operatortool.AnalyticsReworkCause{}}
	items := []operatortool.AnalyticsIssueResidence{}
	lanes := map[string][]float64{}
	causes := map[string]operatortool.AnalyticsReworkCause{}
	var system, held, queue, lead []float64
	for _, id := range frames.ids {
		from := maxTime(from, frames.retained[id])
		if !from.Before(to) {
			continue
		}
		end := frames.at(to, id)
		begin := frames.at(from, id)
		item := residenceSampleItem(end, frames.states, to)
		item.WorkItemID = id
		endLanes, endQueue := residenceSampleNanoseconds(end, frames.states, to)
		beginLanes, beginQueue := residenceSampleNanoseconds(begin, frames.states, from)
		item.SystemSeconds = 0
		item.HeldSeconds = 0
		item.QueueSeconds = float64(max(int64(0), endQueue-beginQueue)) / 1e9
		for lane := range item.LaneSeconds {
			seconds := float64(endLanes[lane]-beginLanes[lane]) / 1e9
			if seconds <= 0 {
				delete(item.LaneSeconds, lane)
				continue
			}
			item.LaneSeconds[lane] = seconds
			lanes[lane] = append(lanes[lane], seconds)
			if analyticsResidenceGroup(lane, frames.states) == "held" {
				item.HeldSeconds += seconds
			} else {
				item.SystemSeconds += seconds
			}
		}
		if end.LastDone != nil && !end.LastDone.Before(from) && end.LastDone.Before(to) {
			seconds := end.LastDone.Sub(end.Created).Seconds()
			item.LeadSeconds = &seconds
			lead = append(lead, seconds)
		}
		if item.SystemSeconds > 0 {
			system = append(system, item.SystemSeconds)
		}
		if item.HeldSeconds > 0 {
			held = append(held, item.HeldSeconds)
		}
		if item.QueueSeconds > 0 {
			queue = append(queue, item.QueueSeconds)
		}
		for key, cause := range end.Causes {
			cause.Count -= begin.Causes[key].Count
			if cause.Count > 0 {
				aggregate := causes[key]
				aggregate.FromState, aggregate.ReasonDetail = cause.FromState, cause.ReasonDetail
				aggregate.Count += cause.Count
				causes[key] = aggregate
			}
		}
		items = append(items, item)
	}
	for lane, values := range lanes {
		summary.Lanes = append(summary.Lanes, operatortool.AnalyticsLaneDuration{Lane: lane, Group: analyticsResidenceGroup(lane, frames.states), AnalyticsDuration: sampledDuration(values)})
	}
	sort.Slice(summary.Lanes, func(i, j int) bool { return summary.Lanes[i].Lane < summary.Lanes[j].Lane })
	for _, cause := range causes {
		summary.ReworkCauses = append(summary.ReworkCauses, cause)
	}
	sort.Slice(summary.ReworkCauses, func(i, j int) bool {
		a, b := summary.ReworkCauses[i], summary.ReworkCauses[j]
		if a.FromState != b.FromState {
			return a.FromState < b.FromState
		}
		return a.ReasonDetail < b.ReasonDetail
	})
	summary.SystemTotal, summary.HeldTotal, summary.QueueTotal, summary.LeadTotal = sampledDuration(system), sampledDuration(held), sampledDuration(queue), sampledDuration(lead)
	return summary, items
}

func residenceSampleNanoseconds(sample analyticsResidenceSample, states map[string]tracker.NativeState, at time.Time) (map[string]int64, int64) {
	lanes := map[string]int64{}
	for lane, ns := range sample.LanesNS {
		lanes[lane] = ns
	}
	queue := sample.QueueNS
	if sample.Started != nil && !sample.Partial {
		from := maxTime(sample.Entered, *sample.Started)
		if analyticsResidenceGroup(sample.State, states) != "excluded" {
			lanes[sample.State] += max(int64(0), at.Sub(from).Nanoseconds())
		}
		if sample.State == "Todo" || sample.State == "Rework" {
			to := at
			if sample.QueueTo != nil {
				to = minTime(to, *sample.QueueTo)
			}
			queue += max(int64(0), to.Sub(from).Nanoseconds())
		}
	}
	return lanes, queue
}

func readAnalyticsSkipRollups(ctx context.Context, q nativeQueryer, scope nativeScope, out *nativeAnalyticsProject, compacted bool) (bool, error) {
	w := out.Window
	fullFrom, fullTo := w.From.Truncate(time.Hour), w.To.Truncate(time.Hour)
	if !fullFrom.Equal(w.From) {
		fullFrom = fullFrom.Add(time.Hour)
	}
	if compacted && (!fullFrom.Equal(w.From) || !fullTo.Equal(w.To)) {
		out.Partial = true
	}
	rows, err := q.QueryContext(ctx, `WITH samples AS (
 SELECT skip_source AS source,skip_reason AS reason,value,last_at AS last_at,last_ns AS last_ns FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='skipped' AND hour>=? AND hour<?
 UNION ALL SELECT skip_source,skip_reason,1,sample_at,sample_ns FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='skip_sample' AND sample_ns>=? AND sample_ns<?
 UNION ALL SELECT skip_source,skip_reason,1,sample_at,sample_ns FROM analytics_rollup_legacy WHERE organization_id=? AND project_id=? AND metric='skip_sample' AND sample_ns>=? AND sample_ns<?)
 SELECT source,reason,sum(value),last_at,max(last_ns) FROM samples GROUP BY source,reason ORDER BY source,reason`, scope.organization, scope.project, fullFrom.Unix(), fullTo.Unix(), scope.organization, scope.project, w.From.UnixNano(), minTime(fullFrom, w.To).UnixNano(), scope.organization, scope.project, maxTime(fullFrom, fullTo).UnixNano(), w.To.UnixNano())
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var skips []nativeAnalyticsSkip
	var count int
	sourceAt := out.SourceAt
	for rows.Next() {
		var skip nativeAnalyticsSkip
		var last string
		var epoch int64
		if err := rows.Scan(&skip.Source, &skip.Reason, &skip.Count, &last, &epoch); err != nil {
			return false, err
		}
		count += skip.Count
		if count > maxAnalyticsPopulation {
			if !compacted {
				return false, nil
			}
			out.Partial = true
		}
		at, err := parseTimeValue(last)
		if err != nil {
			return false, err
		}
		sourceAt = maxTime(sourceAt, at)
		skips = append(skips, skip)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	out.DecisionsObserved = count
	out.SourceAt = sourceAt
	out.SkipReasons = append(out.SkipReasons, skips...)
	if count == 0 {
		out.Unavailable = append(out.Unavailable, "recorded_skip_reasons")
	}
	return true, nil
}
