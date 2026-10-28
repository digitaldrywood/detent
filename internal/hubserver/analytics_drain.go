package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
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
	err = snapshot.QueryRowContext(ctx, `SELECT i.organization_id,i.project_id,CASE WHEN p.aggregate_version=1 THEN coalesce(p.sequence,0) ELSE 0 END,coalesce(r.analytics_revision,0),CASE WHEN p.aggregate_version=1 THEN coalesce(p.refresh_revision,-1) ELSE -1 END,CASE WHEN p.aggregate_version=1 THEN coalesce(p.refresh_phase,-1) ELSE -1 END,coalesce(p.refresh_offset,0),coalesce(p.refresh_source,'') FROM issues i LEFT JOIN analytics_rollup_positions p ON p.organization_id=i.organization_id AND p.project_id=i.project_id AND p.work_item_id=i.native_id LEFT JOIN derived_refresh_revisions r ON r.work_item_id=i.native_id WHERE i.native_id=?`, id).Scan(&p.scope.organization, &p.scope.project, &p.sequence, &p.revision, &storedRevision, &p.phase, &p.offset, &p.source)
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
	if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT analytics_revision FROM derived_refresh_revisions WHERE work_item_id=?),0),coalesce((SELECT CASE WHEN aggregate_version=1 THEN sequence ELSE 0 END FROM analytics_rollup_positions WHERE work_item_id=?),0),coalesce((SELECT CASE WHEN aggregate_version=1 THEN refresh_phase ELSE -1 END FROM analytics_rollup_positions WHERE work_item_id=?),-1),coalesce((SELECT refresh_offset FROM analytics_rollup_positions WHERE work_item_id=?),0),coalesce((SELECT refresh_source FROM analytics_rollup_positions WHERE work_item_id=?),'')`, id, id, id, id, id).Scan(&revision, &currentSequence, &currentPhase, &currentOffset, &currentSource); err != nil {
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
		if _, markErr := tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions(organization_id,project_id,work_item_id,sequence,failed,aggregate_version) VALUES(?,?,?,?,1,1) ON CONFLICT DO UPDATE SET sequence=excluded.sequence,failed=1,aggregate_version=1,refresh_phase=-1,refresh_offset=0,refresh_source='' `, p.scope.organization, p.scope.project, id, p.sequence); markErr != nil {
			return errors.Join(err, markErr)
		}
	} else {
		if _, err = tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions(organization_id,project_id,work_item_id,sequence,refresh_revision,refresh_phase,refresh_offset,refresh_source,aggregate_version) VALUES(?,?,?,?,?,?,?,?,1) ON CONFLICT DO UPDATE SET sequence=excluded.sequence,aggregate_version=1,failed=0,refresh_revision=excluded.refresh_revision,refresh_phase=excluded.refresh_phase,refresh_offset=excluded.refresh_offset,refresh_source=excluded.refresh_source`, p.scope.organization, p.scope.project, id, next.sequence, p.revision, next.phase, next.offset, next.source); err != nil {
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
		var last sql.NullInt64
		var complete bool
		if err := plan.QueryRowContext(ctx, `SELECT max(sequence),count(*)=count(analytics_projection_version) FROM (SELECT sequence,analytics_projection_version FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? ORDER BY sequence LIMIT ?)`, p.scope.organization, p.scope.project, id, p.sequence, analyticsDrainWrites).Scan(&last, &complete); err != nil {
			return err
		}
		if !last.Valid {
			p.phase = analyticsDrainReportCleanup
			return nil
		}
		if !complete {
			return errors.New("analytics event projection incomplete")
		}
		if err := planAnalyticsCounters(ctx, plan, p.scope, id, p.sequence, last.Int64); err != nil {
			return err
		}
		p.sequence, p.offset, p.source = last.Int64, 0, ""
	case analyticsDrainAttempts, analyticsDrainUsage, analyticsDrainLeases, analyticsDrainResidenceCleanup:
		p.phase, p.offset, p.source = analyticsDrainEvents, 0, ""
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
		}
	default:
		return errors.New("invalid analytics refresh phase")
	}
	return nil
}

func pageProjectionWrites(plan *projectionPlan, offset *int) bool {
	end := min(*offset+analyticsDrainWrites, len(plan.writes))
	done := end == len(plan.writes)
	plan.writes = plan.writes[min(*offset, end):end]
	*offset = end
	return done
}

func planAnalyticsCounters(ctx context.Context, plan projectionQuery, scope nativeScope, id string, after, through int64) error {
	rows, err := plan.QueryContext(ctx, `SELECT unixepoch(recorded_at)/3600*3600,
 CASE WHEN type='workflow.transitioned' THEN 'merge_entered' WHEN scheduler_outcome='ready' THEN 'ready' ELSE 'skipped' END,
 CASE WHEN scheduler_outcome='skipped' THEN coalesce(scheduler_source,'') ELSE '' END,
 CASE WHEN scheduler_outcome='skipped' THEN coalesce(scheduler_reason,'') ELSE '' END,
 recorded_at,analytics_recorded_ns
 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<=?
 AND ((type='workflow.transitioned' AND analytics_to_state='Merging') OR (type='scheduler.decision' AND scheduler_outcome IN ('ready','skipped')))
 ORDER BY sequence`, scope.organization, scope.project, id, after, through)
	if err != nil {
		return err
	}
	defer rows.Close()
	type counter struct {
		hour, ns                   int64
		metric, source, reason, at string
	}
	var counters []counter
	for rows.Next() {
		var c counter
		if err := rows.Scan(&c.hour, &c.metric, &c.source, &c.reason, &c.at, &c.ns); err != nil {
			return err
		}
		counters = append(counters, c)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, c := range counters {
		dimension := ""
		if c.metric == "skipped" {
			dimension = analyticsDimension(c.source, c.reason)
		}
		if _, err := plan.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,skip_source,skip_reason,last_at,last_ns)
  VALUES(?,?,?,?,?,1,?,?,?,?) ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO UPDATE SET value=value+1,
  last_at=CASE WHEN excluded.last_ns>last_ns THEN excluded.last_at ELSE last_at END,last_ns=max(last_ns,excluded.last_ns)`, scope.organization, scope.project, c.hour, c.metric, dimension, c.source, c.reason, c.at, c.ns); err != nil {
			return err
		}
	}
	return nil
}
