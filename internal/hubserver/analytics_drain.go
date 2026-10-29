package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const analyticsDrainWrites = 32

const (
	analyticsDrainEvents = iota
	analyticsDrainAttempts
	analyticsDrainUsage
	analyticsDrainLeases
	analyticsDrainReportCleanup
	analyticsDrainReport
	analyticsDrainResidenceCleanup
)

const analyticsDrainComplete = -1

var errAnalyticsResidenceReset = errors.New("analytics residence source changed")

type projectionQuery interface {
	nativeQueryer
	nativeExecer
}

type projectionWrite struct {
	query string
	args  []any
}

type projectionPlan struct {
	nativeQueryer
	writes      []projectionWrite
	skipDeletes bool
}

func (p *projectionPlan) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	if !p.skipDeletes || !strings.HasPrefix(query, "DELETE FROM report_issue_") {
		p.writes = append(p.writes, projectionWrite{query, args})
	}
	return driver.RowsAffected(1), nil
}

func projectionUpper(before []int64) int64 {
	if len(before) > 0 {
		return before[0]
	}
	return math.MaxInt64
}

type analyticsDrainPosition struct {
	scope              nativeScope
	sequence, revision int64
	phase, offset      int
	source             string
}

func (d *database) drainAnalyticsItem(ctx context.Context, id string, now time.Time) error {
	snapshot, err := d.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin analytics source read: %w", err)
	}
	defer snapshot.Rollback()
	var p analyticsDrainPosition
	var storedRevision int64
	err = snapshot.QueryRowContext(ctx, `SELECT i.organization_id,i.project_id,coalesce(p.sequence,0),coalesce(r.analytics_revision,0),coalesce(p.refresh_revision,-1),coalesce(p.refresh_phase,-1),coalesce(p.refresh_offset,0),coalesce(p.refresh_source,'') FROM issues i LEFT JOIN analytics_rollup_positions p ON p.organization_id=i.organization_id AND p.project_id=i.project_id AND p.work_item_id=i.native_id LEFT JOIN derived_refresh_revisions r ON r.work_item_id=i.native_id WHERE i.native_id=?`, id).Scan(&p.scope.organization, &p.scope.project, &p.sequence, &p.revision, &storedRevision, &p.phase, &p.offset, &p.source)
	if errors.Is(err, sql.ErrNoRows) {
		if err := snapshot.Rollback(); err != nil {
			return err
		}
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_positions WHERE work_item_id=?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_dirty WHERE work_item_id=?", id); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	original := p
	if (storedRevision != p.revision && p.phase != analyticsDrainResidenceCleanup) || p.phase < 0 {
		source := ""
		if p.phase == analyticsDrainEvents {
			source = p.source
		}
		p.phase, p.offset, p.source = analyticsDrainEvents, 0, source
	}
	plan := &projectionPlan{nativeQueryer: snapshot}
	next := p
	for range analyticsDrainResidenceCleanup + 1 {
		phase := next.phase
		err = d.planAnalyticsPage(ctx, plan, id, now, &next)
		if err != nil || len(plan.writes) > 0 || next.phase < 0 || next.phase == phase {
			break
		}
	}
	if closeErr := snapshot.Rollback(); closeErr != nil {
		err = errors.Join(err, closeErr)
	}
	tx, beginErr := d.db.BeginTx(ctx, nil)
	if beginErr != nil {
		return errors.Join(err, beginErr)
	}
	defer tx.Rollback()
	var revision int64
	var currentSequence int64
	var currentPhase, currentOffset int
	var currentSource string
	if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT analytics_revision FROM derived_refresh_revisions WHERE work_item_id=?),0),coalesce((SELECT sequence FROM analytics_rollup_positions WHERE work_item_id=?),0),coalesce((SELECT refresh_phase FROM analytics_rollup_positions WHERE work_item_id=?),-1),coalesce((SELECT refresh_offset FROM analytics_rollup_positions WHERE work_item_id=?),0),coalesce((SELECT refresh_source FROM analytics_rollup_positions WHERE work_item_id=?),'')`, id, id, id, id, id).Scan(&revision, &currentSequence, &currentPhase, &currentOffset, &currentSource); err != nil {
		return err
	}
	if revision != p.revision || currentSequence != original.sequence || currentPhase != original.phase || currentOffset != original.offset || currentSource != original.source {
		return nil
	}
	if err == nil {
		if _, saveErr := tx.ExecContext(ctx, "SAVEPOINT analytics_rollup"); saveErr != nil {
			return saveErr
		}
		for _, w := range plan.writes {
			if _, writeErr := d.analytics.exec(ctx, tx, w.query, w.args...); writeErr != nil {
				err = writeErr
				break
			}
		}
		if err != nil {
			if _, rollbackErr := tx.ExecContext(ctx, "ROLLBACK TO analytics_rollup"); rollbackErr != nil {
				return errors.Join(err, rollbackErr)
			}
		}
		if _, releaseErr := tx.ExecContext(ctx, "RELEASE analytics_rollup"); releaseErr != nil {
			return errors.Join(err, releaseErr)
		}
	}
	if err != nil {
		if _, markErr := tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions(organization_id,project_id,work_item_id,sequence,failed) VALUES(?,?,?,?,1) ON CONFLICT DO UPDATE SET failed=1`, p.scope.organization, p.scope.project, id, p.sequence); markErr != nil {
			return errors.Join(err, markErr)
		}
	} else {
		if _, err = tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions(organization_id,project_id,work_item_id,sequence,refresh_revision,refresh_phase,refresh_offset,refresh_source) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO UPDATE SET sequence=excluded.sequence,failed=0,refresh_revision=excluded.refresh_revision,refresh_phase=excluded.refresh_phase,refresh_offset=excluded.refresh_offset,refresh_source=excluded.refresh_source`, p.scope.organization, p.scope.project, id, next.sequence, p.revision, next.phase, next.offset, next.source); err != nil {
			return err
		}
	}
	if err != nil || next.phase < 0 {
		if _, clearErr := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_dirty WHERE work_item_id=?", id); clearErr != nil {
			return errors.Join(err, clearErr)
		}
	} else {
		if _, queueErr := tx.ExecContext(ctx, "INSERT INTO analytics_rollup_dirty(work_item_id) SELECT ? WHERE NOT EXISTS(SELECT 1 FROM analytics_rollup_dirty WHERE work_item_id=?)", id, id); queueErr != nil {
			return queueErr
		}
	}
	return errors.Join(err, tx.Commit())
}

func (d *database) planAnalyticsPage(ctx context.Context, plan *projectionPlan, id string, now time.Time, p *analyticsDrainPosition) error {
	switch p.phase {
	case analyticsDrainEvents:
		var baseline bool
		if err := plan.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=?)`, p.scope.organization, p.scope.project, id).Scan(&baseline); err != nil {
			return err
		}
		if !baseline {
			var raw string
			err := plan.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?`, p.scope.organization, p.scope.project, id+":0").Scan(&raw)
			if err == nil {
				var sample analyticsResidenceSample
				if err := json.Unmarshal([]byte(raw), &sample); err != nil {
					return err
				}
				return writeAnalyticsResidenceSample(ctx, plan, p.scope, "residence_hour", id, sample)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return refreshAnalyticsResidence(ctx, plan, p.scope, id, 0, 0)
		}
		var last sql.NullInt64
		var complete bool
		if err := plan.QueryRowContext(ctx, `SELECT max(sequence),count(*)=count(analytics_projection_version) FROM (SELECT sequence,analytics_projection_version FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? ORDER BY sequence LIMIT ?)`, p.scope.organization, p.scope.project, id, p.sequence, analyticsDrainWrites).Scan(&last, &complete); err != nil {
			return err
		}
		if !last.Valid {
			if err := plan.QueryRowContext(ctx, "SELECT max(event_sequence,?) FROM issues WHERE native_id=?", p.sequence, id).Scan(&p.sequence); err != nil {
				return err
			}
			p.phase = analyticsDrainAttempts
			return nil
		}
		if !complete {
			return errors.New("analytics event projection incomplete")
		}
		sequence := last.Int64
		if _, err := plan.ExecContext(ctx, analyticsEventsRefresh, p.scope.organization, p.scope.project, id, p.sequence, sequence); err != nil {
			return err
		}
		if err := refreshAnalyticsSkipSamples(ctx, plan, p.scope, id, p.sequence, sequence); err != nil {
			return err
		}
		if err := refreshAnalyticsClaims(ctx, plan, p.scope, id, p.sequence, sequence); err != nil {
			return err
		}
		var changed bool
		if err := plan.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=?) OR EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed')))`, p.scope.organization, p.scope.project, id, p.scope.organization, p.scope.project, id, p.sequence, sequence).Scan(&changed); err != nil {
			return err
		}
		if changed {
			checkpoint, err := planAnalyticsResidenceCheckpoint(ctx, plan, id, p)
			if err != nil {
				return err
			}
			if checkpoint {
				return nil
			}
			if err := refreshAnalyticsResidence(ctx, plan, p.scope, id, p.sequence, sequence); err != nil {
				if errors.Is(err, errAnalyticsResidenceReset) {
					plan.writes = nil
					p.phase, p.sequence, p.offset = analyticsDrainResidenceCleanup, 0, 0
					p.source = ""
					return nil
				}
				return err
			}
		}
		if pageProjectionWrites(plan, &p.offset) {
			if p.source != "" {
				if _, err := plan.ExecContext(ctx, `DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?`, p.scope.organization, p.scope.project, p.source); err != nil {
					return err
				}
				p.source = ""
			}
			p.sequence, p.offset = sequence, 0
		}
	case analyticsDrainAttempts:
		return planAnalyticsAttempts(ctx, plan, id, p)
	case analyticsDrainUsage:
		return planAnalyticsUsage(ctx, plan, id, p)
	case analyticsDrainLeases:
		return planAnalyticsLease(ctx, plan, id, now, p)
	case analyticsDrainResidenceCleanup:
		rows, err := plan.QueryContext(ctx, `SELECT rowid FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric IN ('residence_hour','residence_edge','completion','residence_merge','merge_entry') AND sample_id=? LIMIT ?`, p.scope.organization, p.scope.project, id, analyticsDrainWrites)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row int64
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				return err
			}
			if _, err := plan.ExecContext(ctx, "DELETE FROM analytics_hourly_rollups WHERE rowid=?", row); err != nil {
				rows.Close()
				return err
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(plan.writes) == 0 {
			p.phase = analyticsDrainEvents
		}
	case analyticsDrainReportCleanup:
		for _, table := range []string{"report_issue_labels", "report_issue_hours", "report_issue_runners"} {
			rows, err := plan.QueryContext(ctx, "SELECT rowid FROM "+table+" WHERE organization_id=? AND project_id=? AND work_item_id=? LIMIT ?", p.scope.organization, p.scope.project, id, analyticsDrainWrites)
			if err != nil {
				return err
			}
			defer rows.Close()
			var ids []int64
			for rows.Next() {
				var row int64
				if err := rows.Scan(&row); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, row)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			if len(ids) > 0 {
				for _, row := range ids {
					if _, err := plan.ExecContext(ctx, "DELETE FROM "+table+" WHERE rowid=?", row); err != nil {
						return err
					}
				}
				return nil
			}
		}
		p.phase = analyticsDrainReport
	case analyticsDrainReport:
		plan.skipDeletes = true
		if err := refreshProductivitySummary(ctx, plan, p.scope, id); err != nil {
			return err
		}
		if pageProjectionWrites(plan, &p.offset) {
			p.phase, p.offset = analyticsDrainComplete, 0
			if _, err := plan.ExecContext(ctx, analyticsIssuesRefresh, p.scope.organization, p.scope.project, id); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid analytics refresh phase")
	}
	return nil
}

func planAnalyticsResidenceCheckpoint(ctx context.Context, plan *projectionPlan, id string, p *analyticsDrainPosition) (bool, error) {
	var prior int64
	if err := plan.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed'))`, p.scope.organization, p.scope.project, id, p.sequence).Scan(&prior); err != nil {
		return false, err
	}
	dimension := id + ":" + strconv.FormatInt(prior, 10)
	var exists bool
	if err := plan.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?)`, p.scope.organization, p.scope.project, dimension).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	var raw string
	if err := plan.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? ORDER BY hour DESC LIMIT 1`, p.scope.organization, p.scope.project, id).Scan(&raw); err != nil {
		return false, err
	}
	var sample analyticsResidenceSample
	if err := json.Unmarshal([]byte(raw), &sample); err != nil {
		return false, err
	}
	if sample.Sequence > p.sequence {
		return false, errors.New("analytics residence predecessor unavailable")
	}
	plan.writes = nil
	p.source = dimension
	return true, writeAnalyticsResidenceSample(ctx, plan, p.scope, "residence_edge", dimension, sample)
}

func pageProjectionWrites(plan *projectionPlan, offset *int) bool {
	end := min(*offset+analyticsDrainWrites, len(plan.writes))
	done := end == len(plan.writes)
	plan.writes = plan.writes[min(*offset, end):end]
	*offset = end
	return done
}

func planAnalyticsAttempts(ctx context.Context, plan *projectionPlan, id string, p *analyticsDrainPosition) error {
	rows, err := plan.QueryContext(ctx, `SELECT a.id,a.work_item_id,a.lease_id,a.status,a.started_at,a.updated_at,a.data_json,a.analytics_revision FROM native_attempts a WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups r WHERE r.organization_id=a.organization_id AND r.project_id=a.project_id AND r.metric='attempt' AND r.work_item_id=a.work_item_id AND r.dimension=a.id AND r.source_revision=a.analytics_revision) ORDER BY a.fencing_token LIMIT ?`, p.scope.organization, p.scope.project, id, analyticsDrainWrites/2)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var attempt, item, lease, status, started, updated, raw string
		var revision int64
		if err := rows.Scan(&attempt, &item, &lease, &status, &started, &updated, &raw, &revision); err != nil {
			return err
		}
		var observation analyticsAttemptProjection
		if err := json.Unmarshal([]byte(raw), &observation); err != nil {
			return err
		}
		payload, err := projectAnalyticsAttempt(raw, attempt, item, lease, status, started, updated, analyticsFailureTime(observation.Runtime.Landing, status, updated))
		if err != nil {
			return err
		}
		at, err := parseTimeValue(started)
		if err != nil {
			return err
		}
		if err := writeAnalyticsSample(ctx, plan, string(p.scope.organization), string(p.scope.project), at.Unix()/3600*3600, "attempt", attempt, 1, payload); err != nil {
			return err
		}
		if _, err := plan.ExecContext(ctx, `UPDATE analytics_hourly_rollups SET source_revision=? WHERE organization_id=? AND project_id=? AND metric='attempt' AND dimension=?`, revision, p.scope.organization, p.scope.project, attempt); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		p.phase = analyticsDrainUsage
	}
	return rows.Err()
}

func planAnalyticsUsage(ctx context.Context, plan *projectionPlan, id string, p *analyticsDrainPosition) error {
	rows, err := plan.QueryContext(ctx, `SELECT u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency,u.updated_at FROM attempt_usage u WHERE u.organization_id=? AND u.project_id=? AND (EXISTS(SELECT 1 FROM native_attempts a WHERE a.id=u.attempt_id AND a.work_item_id=?) OR EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=u.organization_id AND e.project_id=u.project_id AND e.work_item_id=? AND e.analytics_attempt_id=u.attempt_id)) AND NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups r WHERE r.organization_id=u.organization_id AND r.project_id=u.project_id AND r.metric='usage' AND r.attempt_id=u.attempt_id AND r.period=u.period AND r.provider=u.provider AND r.model=u.model AND r.currency=u.currency AND r.input=u.input AND r.cached_input=u.cached_input AND r.output=u.output AND r.value=u.cost_estimate AND r.updated_at=u.updated_at) LIMIT ?`, p.scope.organization, p.scope.project, id, id, analyticsDrainWrites)
	if err != nil {
		return err
	}
	if err := writeAnalyticsUsageRows(ctx, plan, string(p.scope.organization), string(p.scope.project), rows); err != nil {
		return err
	}
	if len(plan.writes) == 0 {
		p.phase = analyticsDrainLeases
	}
	return nil
}

func planAnalyticsLease(ctx context.Context, plan *projectionPlan, id string, now time.Time, p *analyticsDrainPosition) error {
	var lease, acquired, expires string
	var released sql.NullString
	var issue int64
	var err error
	if p.source == "" {
		err = plan.QueryRowContext(ctx, `SELECT l.lease_id FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND NOT EXISTS(SELECT 1 FROM analytics_slot_retention t WHERE t.organization_id=i.organization_id AND t.project_id=i.project_id AND unixepoch(coalesce(l.released_at,l.expires_at))<=t.through_hour) AND NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups r WHERE r.organization_id=i.organization_id AND r.project_id=i.project_id AND r.metric='lease' AND r.dimension=l.lease_id AND r.acquired_at=l.acquired_at AND r.released_at IS l.released_at AND r.expires_at=l.expires_at AND (l.released_at IS NOT NULL OR unixepoch(l.expires_at)<=? OR r.source_revision>=?)) ORDER BY l.lease_id LIMIT 1`, p.scope.organization, p.scope.project, id, now.Unix(), now.Truncate(time.Hour).Unix()).Scan(&p.source)
		if errors.Is(err, sql.ErrNoRows) {
			p.phase = analyticsDrainReportCleanup
			return nil
		}
		if err != nil {
			return err
		}
	}
	err = plan.QueryRowContext(ctx, `SELECT lease_id,issue_id,acquired_at,released_at,expires_at FROM leases WHERE lease_id=?`, p.source).Scan(&lease, &issue, &acquired, &released, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		p.source, p.offset = "", 0
		return nil
	}
	if err != nil {
		return err
	}
	var retained sql.NullInt64
	if err := plan.QueryRowContext(ctx, "SELECT max(through_hour) FROM analytics_slot_retention WHERE organization_id=? AND project_id=?", p.scope.organization, p.scope.project).Scan(&retained); err != nil {
		return err
	}
	if p.offset >= 0 {
		rows, err := plan.QueryContext(ctx, `SELECT rowid FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='slots' AND dimension=? AND hour>=? LIMIT ?`, p.scope.organization, p.scope.project, lease, retained.Int64, analyticsDrainWrites)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row int64
			if err := rows.Scan(&row); err != nil {
				rows.Close()
				return err
			}
			if _, err := plan.ExecContext(ctx, "DELETE FROM analytics_hourly_rollups WHERE rowid=?", row); err != nil {
				rows.Close()
				return err
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if len(plan.writes) > 0 {
			return nil
		}
		p.offset = -1
	}
	start, err := parseTimeValue(acquired)
	if err != nil {
		return err
	}
	finish, err := parseTimeValue(expires)
	if err != nil {
		return err
	}
	if released.Valid {
		end, err := parseTimeValue(released.String)
		if err != nil {
			return err
		}
		finish = minTime(finish, end)
	}
	hour := maxTime(start.Truncate(time.Hour), time.Unix(retained.Int64, 0)).Add(time.Duration(-p.offset-1) * time.Hour)
	horizon := now.Truncate(time.Hour).Add(analyticsSlotHorizon)
	for hour.Before(finish) && hour.Before(horizon) && len(plan.writes) < analyticsDrainWrites {
		seconds := minTime(finish, hour.Add(time.Hour)).Sub(maxTime(start, hour)).Seconds()
		if seconds > 0 {
			if _, err := plan.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,acquired_at,released_at,expires_at,projection_version) VALUES(?,?,?,'slots',?,?,'{}',?,?,?,1) ON CONFLICT DO UPDATE SET value=excluded.value,acquired_at=excluded.acquired_at,released_at=excluded.released_at,expires_at=excluded.expires_at`, p.scope.organization, p.scope.project, hour.Unix(), lease, seconds/3600, acquired, released, expires); err != nil {
				return err
			}
		}
		hour = hour.Add(time.Hour)
		p.offset--
	}
	if !hour.Before(finish) || !hour.Before(horizon) {
		at := start.Unix() / 3600 * 3600
		if len(plan.writes) == analyticsDrainWrites {
			return nil
		}
		if _, err := plan.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,lease_id,issue_id,acquired_at,released_at,expires_at,projection_version,source_revision) VALUES(?,?,?,'lease',?,1,'{}',?,?,?,?,?,1,?) ON CONFLICT DO UPDATE SET hour=excluded.hour,lease_id=excluded.lease_id,issue_id=excluded.issue_id,acquired_at=excluded.acquired_at,released_at=excluded.released_at,expires_at=excluded.expires_at,projection_version=1,source_revision=excluded.source_revision`, p.scope.organization, p.scope.project, at, lease, lease, issue, acquired, released, expires, now.Truncate(time.Hour).Unix()); err != nil {
			return err
		}
		p.source, p.offset = "", 0
	}
	return nil
}
