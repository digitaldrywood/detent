package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const (
	analyticsAttemptFailedAt = `CASE WHEN status IN ('failed','interrupted') OR json_extract(data_json,'$.runtime.landing.refusal_kind') IN ('conflict','base_protected') OR coalesce(json_extract(data_json,'$.runtime.landing.gate_failed'),0)=1
THEN CASE WHEN (json_extract(data_json,'$.runtime.landing.refusal_kind') IN ('conflict','base_protected') AND coalesce(json_extract(data_json,'$.runtime.landing.landed'),0)=0) OR coalesce(json_extract(data_json,'$.runtime.landing.gate_failed'),0)=1
THEN coalesce(nullif(json_extract(data_json,'$.runtime.landing.observed_at'),'0001-01-01T00:00:00Z'),updated_at) ELSE updated_at END END`
)

const (
	analyticsSourceIssues = "WITH analytics_source_issues AS " + `(SELECT organization_id,project_id,unixepoch(created_at)/3600*3600 AS hour,'issue' AS metric,native_id AS dimension,1 AS value,json_object('id',id,'native_id',native_id,'title',title,'created_at',created_at,'updated_at',updated_at,'workflow_state_id',workflow_state_id) AS data_json FROM issues WHERE organization_id IS NOT NULL AND project_id IS NOT NULL AND native_id IS NOT NULL)` + "\n"

	analyticsSourceAttemptUsage = "WITH analytics_source_attempt_usage AS " + `(SELECT organization_id,project_id,unixepoch(period)/3600*3600 AS hour,'usage' AS metric,json_array(attempt_id,period,provider,model,currency) AS dimension,cost_estimate AS value,json_object('attempt_id',attempt_id,'period',period,'provider',provider,'model',model,'input',input,'cached_input',cached_input,'output',output,'cost_estimate',cost_estimate,'currency',currency,'updated_at',updated_at) AS data_json,attempt_id FROM attempt_usage WHERE organization_id IS NOT NULL AND project_id IS NOT NULL)` + "\n"
	analyticsSourceEvents       = "WITH analytics_source_events AS " + `(SELECT e.organization_id,e.project_id,unixepoch(e.recorded_at)/3600*3600 AS hour,'event' AS metric,e.id AS dimension,1 AS value,
json_object('work_item_id',e.work_item_id,'sequence',e.sequence,'type',e.type,'recorded_at',e.recorded_at,'outcome',json_extract(e.data_json,'$.decision.outcome'),'source',json_extract(e.data_json,'$.decision.source'),'reason',json_extract(e.data_json,'$.decision.reason'),'to_state',json_extract(e.data_json,'$.to_state')) AS data_json,e.work_item_id,e.sequence
FROM collaboration_events e WHERE e.type IN ('workflow.transitioned','scheduler.decision','issue.created'))` + "\n"
	analyticsSourceLeases = "WITH analytics_source_leases AS " + `(SELECT i.organization_id,i.project_id,unixepoch(l.acquired_at)/3600*3600 AS hour,'lease' AS metric,l.lease_id AS dimension,1 AS value,
json_object('lease_id',l.lease_id,'issue_id',l.issue_id,'acquired_at',l.acquired_at,'released_at',l.released_at,'expires_at',l.expires_at) AS data_json,l.issue_id
FROM leases l JOIN issues i ON i.id=l.issue_id WHERE i.native_id IS NOT NULL
AND NOT EXISTS(SELECT 1 FROM analytics_slot_retention p WHERE p.organization_id=i.organization_id AND p.project_id=i.project_id AND unixepoch(coalesce(l.released_at,l.expires_at))<=p.through_hour))` + "\n"
)

const (
	analyticsEventsRefresh = analyticsSourceEvents + `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? ON CONFLICT(organization_id,project_id,metric,hour,dimension) DO NOTHING`
	analyticsIssuesRefresh = analyticsSourceIssues + `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_issues WHERE organization_id=? AND project_id=? AND dimension=? ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json WHERE data_json<>excluded.data_json`

	analyticsUsageRefresh = analyticsSourceAttemptUsage + `,analytics_item_attempts(id) AS (SELECT id FROM native_attempts WHERE organization_id=? AND project_id=? AND work_item_id=? UNION SELECT json_extract(data_json,'$.run.attempt_id') FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>?)
INSERT INTO analytics_hourly_rollups SELECT u.organization_id,u.project_id,u.hour,u.metric,u.dimension,u.value,u.data_json FROM analytics_item_attempts a CROSS JOIN analytics_source_attempt_usage u WHERE u.attempt_id=a.id AND +u.organization_id=? AND +u.project_id=? ON CONFLICT DO UPDATE SET hour=excluded.hour,value=excluded.value,data_json=excluded.data_json WHERE data_json<>excluded.data_json`
	analyticsLeaseRefresh = analyticsSourceLeases + `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_leases WHERE organization_id=? AND project_id=? AND issue_id=(SELECT id FROM issues WHERE organization_id=? AND project_id=? AND native_id=?) ON CONFLICT DO UPDATE SET hour=excluded.hour,data_json=excluded.data_json WHERE data_json<>excluded.data_json RETURNING dimension,data_json`
)

type analyticsStatements map[string]*sql.Stmt

func prepareAnalyticsStatements(ctx context.Context, db *sql.DB, logger *slog.Logger) analyticsStatements {
	statements := analyticsStatements{}
	var failures error
	for _, query := range []string{analyticsEventsRefresh, analyticsIssuesRefresh, analyticsUsageRefresh, analyticsAttemptUpsert} {
		stmt, err := db.PrepareContext(ctx, query) //nolint:sqlclosecheck // database.Close closes the cached statements.
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		statements[query] = stmt
	}
	if failures != nil {
		logger.WarnContext(ctx, "Prepare analytics rollup statements", "error", fmt.Errorf("prepare analytics rollup statements: %w", failures))
	}
	return statements
}

func (s analyticsStatements) exec(ctx context.Context, tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	stmt, ok := s[query]
	if !ok {
		return tx.ExecContext(ctx, query, args...)
	}
	defer analyticsStatementTiming(ctx, time.Now(), query, args)
	bound := tx.StmtContext(ctx, stmt)
	defer bound.Close()
	return bound.ExecContext(ctx, args...)
}

func analyticsStatementTiming(ctx context.Context, start time.Time, query string, args []any) {
	timing := requestTimingFrom(ctx)
	if timing == nil {
		return
	}
	named := make([]driver.NamedValue, len(args))
	for i, arg := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: arg}
	}
	timing.statement(start, query, named)
}

func (s analyticsStatements) close() error {
	var err error
	for _, stmt := range s {
		err = errors.Join(err, stmt.Close())
	}
	return err
}

const (
	analyticsSlotHorizon    = 24 * time.Hour
	analyticsBackfillBatch  = 20
	analyticsBackfillBudget = 20 * time.Second
	analyticsBackfillEvents = 5000

	analyticsSampleRetention       = 30 * 24 * time.Hour
	analyticsSampleCompactionBatch = 5000
)

type analyticsRollupReader struct {
	nativeQueryer
}

var analyticsRollupTables = strings.NewReplacer(
	"native_attempts", "analytics_native_attempts",
	"attempt_usage", "analytics_attempt_usage",
	"FROM issues", "FROM analytics_issues",
	"JOIN issues", "JOIN analytics_issues",
	"FROM leases", "FROM analytics_leases",
)

var analyticsEventTable = regexp.MustCompile(`(?i)\b(FROM|JOIN)\s+collaboration_events\b(\s+(?:AS\s+)?([a-z_][a-z0-9_]*))?`)

var analyticsSQLKeywords = map[string]bool{"where": true, "join": true, "left": true, "inner": true, "cross": true, "on": true, "order": true, "group": true, "limit": true, "union": true, "except": true, "intersect": true, "having": true, "window": true, "using": true, "natural": true, "indexed": true, "not": true, "outer": true}

const analyticsEventSource = `(SELECT r.organization_id,r.project_id,r.dimension AS id,r.json_work_item_id AS work_item_id,r.json_sequence AS sequence,r.json_type AS type,r.json_recorded_at AS recorded_at,e.data_json FROM analytics_hourly_rollups r JOIN collaboration_events e ON e.id=r.dimension WHERE r.metric='event')`

func analyticsRollupStatement(statement string) string {
	return analyticsEventTable.ReplaceAllStringFunc(analyticsRollupTables.Replace(statement), func(match string) string {
		parts := analyticsEventTable.FindStringSubmatch(match)
		alias, rest := parts[3], ""
		if alias == "" || analyticsSQLKeywords[strings.ToLower(alias)] {
			alias, rest = "collaboration_events", parts[2]
		}
		return parts[1] + " " + analyticsEventSource + " AS " + alias + rest
	})
}

func (q analyticsRollupReader) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	return q.nativeQueryer.QueryContext(ctx, analyticsRollupStatement(statement), args...)
}

func (q analyticsRollupReader) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	return q.nativeQueryer.QueryRowContext(ctx, analyticsRollupStatement(statement), args...)
}

func analyticsProjectReader(ctx context.Context, q nativeQueryer, scope nativeScope) (nativeQueryer, error) {
	rollups, ok := q.(analyticsRollupReader)
	if ok && scope.credential.HostedRole == "reporter" {
		return rollups.nativeQueryer, nil
	}
	if !ok {
		return q, nil
	}
	var complete bool
	if err := q.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM analytics_rollup_backfill WHERE organization_id=? AND project_id=?) AND NOT EXISTS(SELECT 1 FROM analytics_rollup_positions WHERE organization_id=? AND project_id=? AND failed=1)`, scope.organization, scope.project, scope.organization, scope.project).Scan(&complete); err != nil {
		return nil, fmt.Errorf("read analytics rollup status: %w", err)
	}
	if complete {
		return q, nil
	}
	return rollups.nativeQueryer, nil
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

func (s *Service) maintainAnalyticsRollups(ctx context.Context, now time.Time, retryFailed bool) {
	deadline := time.Now().Add(analyticsBackfillBudget)
	for _, step := range []struct {
		name string
		run  func(context.Context, time.Time) (bool, error)
	}{
		{"drain analytics rollups", s.database.drainAnalyticsDirty},
		{"backfill analytics rollups", s.database.backfillAnalyticsRollups},
	} {
		for time.Now().Before(deadline) && ctx.Err() == nil {
			more, err := step.run(ctx, now)
			if err != nil && !errors.Is(err, context.Canceled) {
				s.config.Logger.ErrorContext(ctx, step.name, "error", err)
			}
			if !more {
				break
			}
		}
	}
	if !retryFailed || ctx.Err() != nil {
		return
	}
	if err := s.database.retryFailedAnalyticsRollups(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
		s.config.Logger.ErrorContext(ctx, "retry failed analytics rollups", "error", err)
	}
}

func (d *database) backfillAnalyticsRollups(ctx context.Context, now time.Time) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin analytics backfill: %w", err)
	}
	defer tx.Rollback()
	var organization, project string
	if err := tx.QueryRowContext(ctx, "SELECT organization_id,project_id FROM analytics_rollup_backfill ORDER BY organization_id,project_id LIMIT 1").Scan(&organization, &project); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("select analytics backfill project: %w", err)
	}
	ids, err := analyticsBackfillItems(ctx, tx, organization, project)
	if err != nil {
		return false, err
	}
	var failures error
	for _, id := range ids {
		if err := refreshAnalyticsIssueIsolated(ctx, tx, d.analytics, id, now); err != nil {
			failures = errors.Join(failures, fmt.Errorf("backfill analytics item %s: %w", id, err))
		}
	}
	if len(ids) == 0 {
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_backfill WHERE organization_id=? AND project_id=?", organization, project); err != nil {
			return false, fmt.Errorf("complete analytics backfill: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit analytics backfill: %w", err)
	}
	return true, failures
}

func analyticsBackfillItems(ctx context.Context, tx *sql.Tx, organization, project string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.native_id,i.event_sequence FROM issues i WHERE i.organization_id=? AND i.project_id=? AND i.native_id IS NOT NULL
AND NOT EXISTS(SELECT 1 FROM analytics_rollup_positions p WHERE p.organization_id=i.organization_id AND p.project_id=i.project_id AND p.work_item_id=i.native_id)
ORDER BY i.native_id LIMIT ?`, organization, project, analyticsBackfillBatch)
	if err != nil {
		return nil, fmt.Errorf("select analytics backfill items: %w", err)
	}
	defer rows.Close()
	var ids []string
	var events int64
	for rows.Next() {
		var id string
		var sequence int64
		if err := rows.Scan(&id, &sequence); err != nil {
			return nil, fmt.Errorf("scan analytics backfill item: %w", err)
		}
		if len(ids) > 0 && events+sequence > analyticsBackfillEvents {
			break
		}
		ids = append(ids, id)
		events += sequence
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read analytics backfill items: %w", err)
	}
	return ids, nil
}

func (d *database) drainAnalyticsDirty(ctx context.Context, now time.Time) (bool, error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin analytics drain: %w", err)
	}
	defer tx.Rollback()
	failures := d.projectAnalyticsBatch(ctx, tx, now, 1)
	var more bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM analytics_rollup_dirty)").Scan(&more); err != nil {
		return false, errors.Join(failures, fmt.Errorf("read analytics rollup work: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return false, errors.Join(failures, fmt.Errorf("commit analytics drain: %w", err))
	}
	return more, failures
}

func (d *database) retryFailedAnalyticsRollups(ctx context.Context, now time.Time) error {
	d.analyticsRetryMu.Lock()
	defer d.analyticsRetryMu.Unlock()
	ids, err := nativePageIDs(ctx, d.reader, "SELECT work_item_id FROM analytics_rollup_positions WHERE failed=1 AND work_item_id>? ORDER BY work_item_id LIMIT ?", d.analyticsRetryCursor, analyticsBackfillBatch)
	if err != nil {
		return fmt.Errorf("select failed analytics items: %w", err)
	}
	d.analyticsRetryCursor = ""
	if len(ids) == analyticsBackfillBatch {
		d.analyticsRetryCursor = ids[len(ids)-1]
	}
	var failures error
	for _, id := range ids {
		tx, err := d.db.BeginTx(ctx, nil)
		if err != nil {
			return errors.Join(failures, fmt.Errorf("begin analytics retry: %w", err))
		}
		if err := refreshAnalyticsIssueIsolated(ctx, tx, d.analytics, id, now); err != nil {
			failures = errors.Join(failures, fmt.Errorf("retry analytics item %s: %w", id, err))
		}
		if err := tx.Commit(); err != nil {
			return errors.Join(failures, fmt.Errorf("commit analytics retry: %w", err))
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

func refreshAnalyticsIssue(ctx context.Context, tx *sql.Tx, statements analyticsStatements, id string, now time.Time) error {
	var organization, project string
	var sequence int64
	if err := tx.QueryRowContext(ctx, "SELECT i.organization_id,i.project_id,max(i.event_sequence,coalesce((SELECT max(e.sequence) FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.work_item_id=i.native_id),0)) FROM issues i WHERE i.native_id=?", id).Scan(&organization, &project, &sequence); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.ExecContext(ctx, "DELETE FROM analytics_rollup_positions WHERE work_item_id=?", id)
		}
		return err
	}
	var through int64
	err := tx.QueryRowContext(ctx, "SELECT sequence FROM analytics_rollup_positions WHERE organization_id=? AND project_id=? AND work_item_id=?", organization, project, id).Scan(&through)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := statements.exec(ctx, tx, analyticsEventsRefresh, organization, project, id, through); err != nil {
		return err
	}
	for _, source := range []struct{ name, statement string }{
		{"issues", analyticsIssuesRefresh},
		{"attempt_usage", analyticsUsageRefresh},
	} {
		args := []any{organization, project, id}
		if source.name == "attempt_usage" {
			args = []any{organization, project, id, organization, project, id, through, organization, project}
		}
		if _, err := statements.exec(ctx, tx, source.statement, args...); err != nil {
			return fmt.Errorf("refresh analytics %s: %w", source.name, err)
		}
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
	if err := tx.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='residence_hour' AND dimension=?) OR EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND work_item_id=? AND sequence>? AND (type='workflow.transitioned' OR (type='scheduler.decision' AND json_extract(data_json,'$.decision.source')='native_claim' AND json_extract(data_json,'$.decision.outcome')='claimed')))`, organization, project, id, organization, project, id, through).Scan(&residenceChanged); err != nil {
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
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_backfill WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		return err
	}
	for _, statement := range []string{
		analyticsSourceIssues + `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_issues WHERE organization_id=? AND project_id=?`,
		analyticsSourceAttemptUsage + `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_attempt_usage WHERE organization_id=? AND project_id=?`,
		analyticsSourceEvents + `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_events WHERE organization_id=? AND project_id=?`,
		analyticsSourceLeases + `INSERT INTO analytics_hourly_rollups SELECT organization_id,project_id,hour,metric,dimension,value,data_json FROM analytics_source_leases WHERE organization_id=? AND project_id=?`,
	} {
		if _, err := tx.ExecContext(ctx, statement, scope.organization, scope.project); err != nil {
			return err
		}
	}
	if err := refreshAnalyticsAttempts(ctx, tx, nil, string(scope.organization), string(scope.project), ""); err != nil {
		return err
	}
	if err := refreshAnalyticsSlots(ctx, tx, string(scope.organization), string(scope.project), now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_rollup_positions WHERE organization_id=? AND project_id=?", scope.organization, scope.project); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO analytics_rollup_positions(organization_id,project_id,work_item_id,sequence) SELECT organization_id,project_id,native_id,event_sequence FROM issues WHERE organization_id=? AND project_id=? AND native_id IS NOT NULL ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET sequence=excluded.sequence,failed=0`, scope.organization, scope.project); err != nil {
		return err
	}
	ids, err := nativePageIDs(ctx, tx, "SELECT native_id FROM issues WHERE organization_id=? AND project_id=? AND native_id IS NOT NULL", scope.organization, scope.project)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := refreshAnalyticsSkipSamples(ctx, tx, scope, id, 0); err != nil {
			return err
		}
		if err := refreshAnalyticsClaims(ctx, tx, scope, id, 0); err != nil {
			return err
		}
		if err := refreshAnalyticsResidence(ctx, tx, scope, id, 0); err != nil {
			return err
		}
		if err := refreshProductivitySummary(ctx, tx, scope, id); err != nil {
			return err
		}
	}
	return nil
}

func readAnalyticsCounter(ctx context.Context, q nativeQueryer, scope nativeScope, metric string, from, to time.Time) (int, error) {
	var count int
	fullFrom, fullTo := from.Truncate(time.Hour), to.Truncate(time.Hour)
	if !fullFrom.Equal(from) {
		fullFrom = fullFrom.Add(time.Hour)
	}
	if err := q.QueryRowContext(ctx, `SELECT coalesce(sum(value),0) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric=? AND hour>=? AND hour<?`, scope.organization, scope.project, metric, fullFrom.Unix(), fullTo.Unix()).Scan(&count); err != nil {
		return 0, err
	}
	filter := "type='workflow.transitioned' AND json_extract(data_json,'$.to_state')='Merging'"
	if metric == "ready" {
		filter = "type='scheduler.decision' AND json_extract(data_json,'$.decision.outcome')='ready'"
	}
	for _, edge := range [][2]time.Time{{from, minTime(fullFrom, to)}, {maxTime(fullFrom, fullTo), to}} {
		if !edge[1].After(edge[0]) {
			continue
		}
		var observed int
		if metric == "merge_entered" {
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='merge_entry' AND json_extract(data_json,'$.AtNS')>=? AND json_extract(data_json,'$.AtNS')<?`, scope.organization, scope.project, edge[0].UnixNano(), edge[1].UnixNano()).Scan(&observed); err != nil {
				return 0, err
			}
		} else {
			if err := q.QueryRowContext(ctx, `SELECT count(*) FROM collaboration_events WHERE organization_id=? AND project_id=? AND `+filter+` AND julianday(recorded_at)>=julianday(?) AND julianday(recorded_at)<julianday(?)`, scope.organization, scope.project, formatHubTime(edge[0]), formatHubTime(edge[1])).Scan(&observed); err != nil {
				return 0, err
			}
		}
		count += observed
	}
	return count, nil
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
 SELECT json_extract(data_json,'$.source') AS source,json_extract(data_json,'$.reason') AS reason,value,json_extract(data_json,'$.last_at') AS last_at,json_extract(data_json,'$.last_ns') AS last_ns FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='skipped' AND hour>=? AND hour<?
 UNION ALL SELECT json_extract(data_json,'$.Source'),json_extract(data_json,'$.Reason'),1,json_extract(data_json,'$.At'),json_extract(data_json,'$.AtNS') FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='skip_sample' AND json_extract(data_json,'$.AtNS')>=? AND json_extract(data_json,'$.AtNS')<?
 UNION ALL SELECT json_extract(data_json,'$.Source'),json_extract(data_json,'$.Reason'),1,json_extract(data_json,'$.At'),json_extract(data_json,'$.AtNS') FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='skip_sample' AND json_extract(data_json,'$.AtNS')>=? AND json_extract(data_json,'$.AtNS')<?)
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

func analyticsSkipSamplesCompacted(ctx context.Context, q nativeQueryer, scope nativeScope, from time.Time) (bool, error) {
	var through int64
	err := q.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='skip_compacted' AND hour=0 AND dimension=''`, scope.organization, scope.project).Scan(&through)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read skip sample compaction: %w", err)
	}
	return from.Unix() < through, nil
}

func refreshAnalyticsSlots(ctx context.Context, tx *sql.Tx, organization, project string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT dimension,data_json FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='lease'`, organization, project)
	if err != nil {
		return err
	}
	return writeAnalyticsSlotRows(ctx, tx, organization, project, rows, now)
}

func refreshAnalyticsLeaseSnapshots(ctx context.Context, tx *sql.Tx, organization, project, item string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, analyticsLeaseRefresh, organization, project, organization, project, item)
	if err != nil {
		return err
	}
	return writeAnalyticsSlotRows(ctx, tx, organization, project, rows, now)
}

func writeAnalyticsSlotRows(ctx context.Context, tx *sql.Tx, organization, project string, rows *sql.Rows, now time.Time) error {
	type lease struct{ id, raw string }
	var leases []lease
	for rows.Next() {
		var l lease
		if err := rows.Scan(&l.id, &l.raw); err != nil {
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
		var data struct {
			Acquired string  `json:"acquired_at"`
			Released *string `json:"released_at"`
			Expires  string  `json:"expires_at"`
		}
		if err := json.Unmarshal([]byte(l.raw), &data); err != nil {
			return err
		}
		start, err := parseTimeValue(data.Acquired)
		if err != nil {
			return err
		}
		finish, err := parseTimeValue(data.Expires)
		if err != nil {
			return err
		}
		if data.Released != nil {
			released, err := parseTimeValue(*data.Released)
			if err != nil {
				return err
			}
			finish = minTime(finish, released)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND metric='slots' AND dimension=? AND hour>=?", organization, project, l.id, retained.Int64); err != nil {
			return err
		}
		horizon := now.Truncate(time.Hour).Add(analyticsSlotHorizon)
		for hour := maxTime(start.Truncate(time.Hour), time.Unix(retained.Int64, 0)); hour.Before(finish) && hour.Before(horizon); hour = hour.Add(time.Hour) {
			seconds := minTime(finish, hour.Add(time.Hour)).Sub(maxTime(start, hour)).Seconds()
			if seconds <= 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, "INSERT INTO analytics_hourly_rollups VALUES(?,?,?,'slots',?,?,?)", organization, project, hour.Unix(), l.id, seconds/3600, l.raw); err != nil {
				return err
			}
		}
	}
	return nil
}
