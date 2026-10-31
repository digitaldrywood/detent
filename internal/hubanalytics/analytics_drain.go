package hubanalytics

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

	"github.com/digitaldrywood/detent/internal/hubsql"
)

const DrainWrites = 32

const (
	analyticsDrainEvents = iota
	analyticsDrainAttempts
	DrainUsage
	DrainLeases
	analyticsDrainReportCleanup
	analyticsDrainReport
	DrainResidenceCleanup
)

const DrainComplete = -1

var errAnalyticsResidenceReset = errors.New("analytics residence source changed")

type ProjectionQuery interface {
	hubsql.Queryer
	hubsql.Execer
}

type projectionWrite struct {
	Query string
	Args  []any
}

type ProjectionPlan struct {
	hubsql.Queryer
	Writes      []projectionWrite
	skipDeletes bool
}

func (p *ProjectionPlan) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	if !p.skipDeletes || !strings.HasPrefix(query, "DELETE FROM report_issue_") {
		p.Writes = append(p.Writes, projectionWrite{query, args})
	}
	return driver.RowsAffected(1), nil
}

func projectionUpper(before []int64) int64 {
	if len(before) > 0 {
		return before[0]
	}
	return math.MaxInt64
}

type DrainPosition struct {
	Scope              Scope
	sequence, revision int64
	Phase, offset      int
	source             string
}

func (d *Store) DrainItem(ctx context.Context, id string, now time.Time) error {
	snapshot, err := d.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin analytics source read: %w", err)
	}
	defer snapshot.Rollback()
	var p DrainPosition
	var storedRevision int64
	err = snapshot.QueryRowContext(ctx, `SELECT i.organization_id,i.project_id,coalesce(p.sequence,0),coalesce(r.analytics_revision,0),coalesce(p.refresh_revision,-1),coalesce(p.refresh_phase,-1),coalesce(p.refresh_offset,0),coalesce(p.refresh_source,'') FROM issues i LEFT JOIN analytics_rollup_positions p ON p.organization_id=i.organization_id AND p.project_id=i.project_id AND p.work_item_id=i.native_id LEFT JOIN derived_refresh_revisions r ON r.work_item_id=i.native_id WHERE i.native_id=?`, id).Scan(&p.Scope.Organization, &p.Scope.Project, &p.sequence, &p.revision, &storedRevision, &p.Phase, &p.offset, &p.source)
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
	if (storedRevision != p.revision && p.Phase != DrainResidenceCleanup) || p.Phase < 0 {
		source := ""
		if p.Phase == analyticsDrainEvents {
			source = p.source
		}
		p.Phase, p.offset, p.source = analyticsDrainEvents, 0, source
	}
	plan := &ProjectionPlan{Queryer: snapshot}
	next := p
	for range DrainResidenceCleanup + 1 {
		phase := next.Phase
		err = d.planPage(ctx, plan, id, now, &next)
		if err != nil || len(plan.Writes) > 0 || next.Phase < 0 || next.Phase == phase {
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
	if revision != p.revision || currentSequence != original.sequence || currentPhase != original.Phase || currentOffset != original.offset || currentSource != original.source {
		return nil
	}
	if err == nil {
		if _, saveErr := tx.ExecContext(ctx, "SAVEPOINT analytics_rollup"); saveErr != nil {
			return saveErr
		}
		for _, w := range plan.Writes {
			if _, writeErr := d.analytics.Exec(ctx, tx, w.Query, w.Args...); writeErr != nil {
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
		if _, markErr := tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions(organization_id,project_id,work_item_id,sequence,failed) VALUES(?,?,?,?,1) ON CONFLICT DO UPDATE SET failed=1`, p.Scope.Organization, p.Scope.Project, id, p.sequence); markErr != nil {
			return errors.Join(err, markErr)
		}
	} else {
		if _, err = tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions(organization_id,project_id,work_item_id,sequence,refresh_revision,refresh_phase,refresh_offset,refresh_source) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO UPDATE SET sequence=excluded.sequence,failed=0,refresh_revision=excluded.refresh_revision,refresh_phase=excluded.refresh_phase,refresh_offset=excluded.refresh_offset,refresh_source=excluded.refresh_source`, p.Scope.Organization, p.Scope.Project, id, next.sequence, p.revision, next.Phase, next.offset, next.source); err != nil {
			return err
		}
	}
	if err != nil || next.Phase < 0 {
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

func (d *Store) planPage(ctx context.Context, plan *ProjectionPlan, id string, now time.Time, p *DrainPosition) error {
	switch p.Phase {
	case analyticsDrainEvents:
		var baseline bool
		if err := plan.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=?)`, p.Scope.Organization, p.Scope.Project, id).Scan(&baseline); err != nil {
			return err
		}
		if !baseline {
			var raw string
			err := plan.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?`, p.Scope.Organization, p.Scope.Project, id+":0").Scan(&raw)
			if err == nil {
				var sample ResidenceSample
				if err := d.hooks.DecodeStorageJSON(ctx, []byte(raw), &sample); err != nil {
					return err
				}
				return WriteResidenceSample(ctx, plan, p.Scope, "residence_hour", id, sample)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			return refreshResidence(ctx, plan, p.Scope, id, 0, 0)
		}
		var last sql.NullInt64
		var complete bool
		if err := plan.QueryRowContext(ctx, `SELECT max(sequence),count(*)=count(analytics_projection_version) FROM (SELECT sequence,analytics_projection_version FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? ORDER BY sequence LIMIT ?)`, p.Scope.Organization, p.Scope.Project, id, p.sequence, DrainWrites).Scan(&last, &complete); err != nil {
			return err
		}
		if !last.Valid {
			if err := plan.QueryRowContext(ctx, "SELECT max(event_sequence,?) FROM issues WHERE native_id=?", p.sequence, id).Scan(&p.sequence); err != nil {
				return err
			}
			p.Phase = analyticsDrainAttempts
			return nil
		}
		if !complete {
			return errors.New("analytics event projection incomplete")
		}
		sequence := last.Int64
		if _, err := plan.ExecContext(ctx, analyticsEventsRefresh, p.Scope.Organization, p.Scope.Project, id, p.sequence, sequence); err != nil {
			return err
		}
		if err := refreshSkipSamples(ctx, plan, p.Scope, id, p.sequence, sequence); err != nil {
			return err
		}
		if err := refreshClaims(ctx, plan, p.Scope, id, p.sequence, sequence); err != nil {
			return err
		}
		var changed bool
		if err := plan.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=?) OR EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed')))`, p.Scope.Organization, p.Scope.Project, id, p.Scope.Organization, p.Scope.Project, id, p.sequence, sequence).Scan(&changed); err != nil {
			return err
		}
		if changed {
			checkpoint, err := d.planResidenceCheckpoint(ctx, plan, id, p)
			if err != nil {
				return err
			}
			if checkpoint {
				return nil
			}
			if err := refreshResidence(ctx, plan, p.Scope, id, p.sequence, sequence); err != nil {
				if errors.Is(err, errAnalyticsResidenceReset) {
					plan.Writes = nil
					p.Phase, p.sequence, p.offset = DrainResidenceCleanup, 0, 0
					p.source = ""
					return nil
				}
				return err
			}
		}
		if pageProjectionWrites(plan, &p.offset) {
			if p.source != "" {
				if _, err := plan.ExecContext(ctx, `DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?`, p.Scope.Organization, p.Scope.Project, p.source); err != nil {
					return err
				}
				p.source = ""
			}
			p.sequence, p.offset = sequence, 0
		}
	case analyticsDrainAttempts:
		return planAnalyticsAttempts(ctx, plan, id, p)
	case DrainUsage:
		return PlanAnalyticsUsage(ctx, plan, id, p)
	case DrainLeases:
		return planAnalyticsLease(ctx, plan, id, now, p)
	case DrainResidenceCleanup:
		rows, err := plan.QueryContext(ctx, `SELECT rowid FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric IN ('residence_hour','residence_edge','completion','residence_merge','merge_entry') AND sample_id=? LIMIT ?`, p.Scope.Organization, p.Scope.Project, id, DrainWrites)
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
		if len(plan.Writes) == 0 {
			p.Phase = analyticsDrainEvents
		}
	case analyticsDrainReportCleanup:
		for _, table := range []string{"report_issue_labels", "report_issue_hours", "report_issue_runners"} {
			rows, err := plan.QueryContext(ctx, "SELECT rowid FROM "+table+" WHERE organization_id=? AND project_id=? AND work_item_id=? LIMIT ?", p.Scope.Organization, p.Scope.Project, id, DrainWrites)
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
		p.Phase = analyticsDrainReport
	case analyticsDrainReport:
		plan.skipDeletes = true
		if err := d.hooks.RefreshProductivity(ctx, plan, p.Scope, id); err != nil {
			return err
		}
		if pageProjectionWrites(plan, &p.offset) {
			p.Phase, p.offset = DrainComplete, 0
			if _, err := plan.ExecContext(ctx, IssuesRefresh, p.Scope.Organization, p.Scope.Project, id); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid analytics refresh phase")
	}
	return nil
}

func (d *Store) planResidenceCheckpoint(ctx context.Context, plan *ProjectionPlan, id string, p *DrainPosition) (bool, error) {
	var prior int64
	if err := plan.QueryRowContext(ctx, `SELECT coalesce(max(sequence),0) FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence<=? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND scheduler_source='native_claim' AND scheduler_outcome='claimed'))`, p.Scope.Organization, p.Scope.Project, id, p.sequence).Scan(&prior); err != nil {
		return false, err
	}
	dimension := id + ":" + strconv.FormatInt(prior, 10)
	var exists bool
	if err := plan.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_edge' AND dimension=?)`, p.Scope.Organization, p.Scope.Project, dimension).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	var raw string
	if err := plan.QueryRowContext(ctx, `SELECT data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=? ORDER BY hour DESC LIMIT 1`, p.Scope.Organization, p.Scope.Project, id).Scan(&raw); err != nil {
		return false, err
	}
	var sample ResidenceSample
	if err := d.hooks.DecodeStorageJSON(ctx, []byte(raw), &sample); err != nil {
		return false, err
	}
	if sample.Sequence > p.sequence {
		return false, errors.New("analytics residence predecessor unavailable")
	}
	plan.Writes = nil
	p.source = dimension
	return true, WriteResidenceSample(ctx, plan, p.Scope, "residence_edge", dimension, sample)
}

func pageProjectionWrites(plan *ProjectionPlan, offset *int) bool {
	end := min(*offset+DrainWrites, len(plan.Writes))
	done := end == len(plan.Writes)
	plan.Writes = plan.Writes[min(*offset, end):end]
	*offset = end
	return done
}

func planAnalyticsAttempts(ctx context.Context, plan *ProjectionPlan, id string, p *DrainPosition) error {
	rows, err := plan.QueryContext(ctx, `SELECT a.id,a.work_item_id,a.lease_id,a.status,a.started_at,a.updated_at,CASE WHEN a.payload_object_key='' THEN a.data_json ELSE '{}' END,a.analytics_revision,a.payload_object_key,a.report_failed_at,coalesce(nullif(a.role,''),a.execution_role,'') FROM native_attempts a WHERE a.organization_id=? AND a.project_id=? AND a.work_item_id=? AND NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups r WHERE r.organization_id=a.organization_id AND r.project_id=a.project_id AND r.metric='attempt' AND r.work_item_id=a.work_item_id AND r.dimension=a.id AND r.source_revision=a.analytics_revision) ORDER BY a.fencing_token LIMIT ?`, p.Scope.Organization, p.Scope.Project, id, DrainWrites/2)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var attempt, item, lease, status, started, updated, raw, key, role string
		var failed sql.NullString
		var revision int64
		if err := rows.Scan(&attempt, &item, &lease, &status, &started, &updated, &raw, &revision, &key, &failed, &role); err != nil {
			return err
		}
		var observation AttemptProjection
		if err := json.Unmarshal([]byte(raw), &observation); err != nil {
			return err
		}
		if role == "" {
			role = observation.Runtime.Identity.Role
			if role == "" {
				role = observation.Identity.Role
			}
		}
		if !failed.Valid {
			failed = FailureTime(observation.Runtime.Landing, status, updated)
		}
		payload, err := projectAttempt(raw, attempt, item, lease, status, started, updated, failed)
		if err != nil {
			return err
		}
		at, err := parseTimeValue(started)
		if err != nil {
			return err
		}
		if err := writeAnalyticsSample(ctx, plan, string(p.Scope.Organization), string(p.Scope.Project), at.Unix()/3600*3600, "attempt", attempt, 1, payload); err != nil {
			return err
		}
		if _, err := plan.ExecContext(ctx, `UPDATE analytics_hourly_rollups SET source_revision=?,payload_object_key=?,attempt_role=? WHERE organization_id=? AND project_id=? AND metric='attempt' AND dimension=?`, revision, key, role, p.Scope.Organization, p.Scope.Project, attempt); err != nil {
			return err
		}
		count++
	}
	if count == 0 {
		p.Phase, p.source, p.offset = DrainUsage, "", 0
	}
	return rows.Err()
}

func PlanAnalyticsUsage(ctx context.Context, plan *ProjectionPlan, id string, p *DrainPosition) error {
	if p.source == "" {
		return advanceAnalyticsUsageAttempt(ctx, plan, id, p)
	}
	var through sql.NullInt64
	if err := plan.QueryRowContext(ctx, `SELECT max(rowid) FROM (SELECT rowid FROM attempt_usage INDEXED BY attempt_usage_drain WHERE attempt_id=? AND rowid>? ORDER BY rowid LIMIT ?)`, p.source, p.offset, DrainWrites).Scan(&through); err != nil {
		return err
	}
	if !through.Valid {
		return advanceAnalyticsUsageAttempt(ctx, plan, id, p)
	}
	rows, err := plan.QueryContext(ctx, `SELECT attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at FROM attempt_usage INDEXED BY attempt_usage_drain WHERE attempt_id=? AND rowid>? AND rowid<=? AND organization_id=? AND project_id=? ORDER BY rowid`, p.source, p.offset, through.Int64, p.Scope.Organization, p.Scope.Project)
	if err != nil {
		return err
	}
	if err := writeAnalyticsUsageRows(ctx, plan, string(p.Scope.Organization), string(p.Scope.Project), rows); err != nil {
		return err
	}
	p.offset = int(through.Int64)
	return nil
}

func advanceAnalyticsUsageAttempt(ctx context.Context, plan *ProjectionPlan, id string, p *DrainPosition) error {
	var next sql.NullString
	err := plan.QueryRowContext(ctx, `SELECT min(id) FROM (
 SELECT id FROM (SELECT id FROM native_attempts INDEXED BY native_attempts_usage_drain WHERE organization_id=? AND project_id=? AND work_item_id=? AND id>? ORDER BY id LIMIT 1)
 UNION ALL SELECT analytics_attempt_id FROM (SELECT analytics_attempt_id FROM collaboration_events INDEXED BY collaboration_events_usage_drain WHERE organization_id=? AND project_id=? AND work_item_id=? AND analytics_attempt_id>? ORDER BY analytics_attempt_id LIMIT 1)
)`, p.Scope.Organization, p.Scope.Project, id, p.source, p.Scope.Organization, p.Scope.Project, id, p.source).Scan(&next)
	if err != nil {
		return err
	}
	p.source, p.offset = next.String, 0
	if !next.Valid {
		p.Phase = DrainLeases
	}
	return nil
}

func planAnalyticsLease(ctx context.Context, plan *ProjectionPlan, id string, now time.Time, p *DrainPosition) error {
	var lease, acquired, expires string
	var released sql.NullString
	var issue int64
	var err error
	if p.source == "" {
		err = plan.QueryRowContext(ctx, `SELECT l.lease_id FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.organization_id=? AND i.project_id=? AND i.native_id=? AND NOT EXISTS(SELECT 1 FROM analytics_slot_retention t WHERE t.organization_id=i.organization_id AND t.project_id=i.project_id AND unixepoch(coalesce(l.released_at,l.expires_at))<=t.through_hour) AND NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups r WHERE r.organization_id=i.organization_id AND r.project_id=i.project_id AND r.metric='lease' AND r.dimension=l.lease_id AND r.acquired_at=l.acquired_at AND r.released_at IS l.released_at AND r.expires_at=l.expires_at AND (l.released_at IS NOT NULL OR unixepoch(l.expires_at)<=? OR r.source_revision>=?)) ORDER BY l.lease_id LIMIT 1`, p.Scope.Organization, p.Scope.Project, id, now.Unix(), now.Truncate(time.Hour).Unix()).Scan(&p.source)
		if errors.Is(err, sql.ErrNoRows) {
			p.Phase = analyticsDrainReportCleanup
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
	if err := plan.QueryRowContext(ctx, "SELECT max(through_hour) FROM analytics_slot_retention WHERE organization_id=? AND project_id=?", p.Scope.Organization, p.Scope.Project).Scan(&retained); err != nil {
		return err
	}
	if p.offset >= 0 {
		rows, err := plan.QueryContext(ctx, `SELECT rowid FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='slots' AND dimension=? AND hour>=? LIMIT ?`, p.Scope.Organization, p.Scope.Project, lease, retained.Int64, DrainWrites)
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
		if len(plan.Writes) > 0 {
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
	for hour.Before(finish) && hour.Before(horizon) && len(plan.Writes) < DrainWrites {
		seconds := minTime(finish, hour.Add(time.Hour)).Sub(maxTime(start, hour)).Seconds()
		if seconds > 0 {
			if _, err := plan.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,acquired_at,released_at,expires_at,projection_version) VALUES(?,?,?,'slots',?,?,'{}',?,?,?,1) ON CONFLICT DO UPDATE SET value=excluded.value,acquired_at=excluded.acquired_at,released_at=excluded.released_at,expires_at=excluded.expires_at`, p.Scope.Organization, p.Scope.Project, hour.Unix(), lease, seconds/3600, acquired, released, expires); err != nil {
				return err
			}
		}
		hour = hour.Add(time.Hour)
		p.offset--
	}
	if !hour.Before(finish) || !hour.Before(horizon) {
		at := start.Unix() / 3600 * 3600
		if len(plan.Writes) == DrainWrites {
			return nil
		}
		if _, err := plan.ExecContext(ctx, `INSERT INTO analytics_hourly_rollups(organization_id,project_id,hour,metric,dimension,value,data_json,lease_id,issue_id,acquired_at,released_at,expires_at,projection_version,source_revision) VALUES(?,?,?,'lease',?,1,'{}',?,?,?,?,?,1,?) ON CONFLICT DO UPDATE SET hour=excluded.hour,lease_id=excluded.lease_id,issue_id=excluded.issue_id,acquired_at=excluded.acquired_at,released_at=excluded.released_at,expires_at=excluded.expires_at,projection_version=1,source_revision=excluded.source_revision`, p.Scope.Organization, p.Scope.Project, at, lease, lease, issue, acquired, released, expires, now.Truncate(time.Hour).Unix()); err != nil {
			return err
		}
		p.source, p.offset = "", 0
	}
	return nil
}
