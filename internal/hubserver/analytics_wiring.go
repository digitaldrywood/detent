package hubserver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/digitaldrywood/detent/internal/hubanalytics"
	"github.com/digitaldrywood/detent/internal/hubsql"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func projectAnalyticsCapacityRollups(ctx context.Context, q nativeQueryer, scope nativeScope, out *hubanalytics.Project) error {
	return analyticsStoreFor(nil, nil, nil, nil).ProjectCapacityRollups(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, out)
}

func (d *database) drainAnalyticsItem(ctx context.Context, id string, now time.Time) error {
	return newAnalyticsStore(d).DrainItem(ctx, id, now)
}

func analyticsResidenceStates(ctx context.Context, q nativeQueryer, scope nativeScope) (map[string]tracker.NativeState, error) {
	return hubanalytics.ResidenceStates(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project})
}

func writeAnalyticsResidenceSample(ctx context.Context, tx nativeExecer, scope nativeScope, metric, dimension string, sample hubanalytics.ResidenceSample) error {
	return hubanalytics.WriteResidenceSample(ctx, tx, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, metric, dimension, sample)
}

func readAnalyticsResidenceFrames(ctx context.Context, q nativeQueryer, scope nativeScope, w operatortool.AnalyticsWindow, points []time.Time) (hubanalytics.ResidenceFrames, error) {
	issues, issueArgs := reportsIssueClause(ctx, "i.native_id")
	return hubanalytics.ReadResidenceFrames(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, w, points, issues, issueArgs)
}

func projectAnalyticsResidenceRollups(ctx context.Context, q nativeQueryer, scope nativeScope, r operatortool.AnalyticsRequest, out *hubanalytics.Project) error {
	return analyticsStoreFor(nil, nil, nil, nil).ProjectResidenceRollups(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, r, out)
}

func readDiagnosticsResidenceRollups(ctx context.Context, q nativeQueryer, scope nativeScope, out *diagnosticsReport) (int, bool, error) {
	view := hubanalytics.DiagnosticsView{From: out.From, To: out.To, Partial: out.Partial, Capacity: out.Capacity}
	n, partial, err := analyticsStoreFor(nil, nil, nil, nil).ReadDiagnosticsResidenceRollups(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, &view)
	out.Partial = view.Partial
	return n, partial, err
}

func readReportsCompletionRollups(ctx context.Context, q nativeQueryer, scope nativeScope, states map[string]tracker.NativeState, ids []string, to time.Time) (hubanalytics.Completion, error) {
	return analyticsStoreFor(nil, nil, nil, nil).ReadReportsCompletionRollups(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, states, ids, to)
}

func readShippedResidenceIssues(ctx context.Context, q nativeQueryer, scope nativeScope, ids []string, to time.Time) ([]hubanalytics.ResidenceIssue, error) {
	return analyticsStoreFor(nil, nil, nil, nil).ReadShippedResidenceIssues(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, ids, to)
}

func prepareAnalyticsStatements(ctx context.Context, db *sql.DB, logger *slog.Logger) *hubanalytics.Statements {
	return hubanalytics.PrepareStatements(ctx, db, logger, analyticsStatementTiming)
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

type analyticsRollupReader struct {
	nativeQueryer
}

func (q analyticsRollupReader) QueryContext(ctx context.Context, statement string, args ...any) (*sql.Rows, error) {
	return q.nativeQueryer.QueryContext(ctx, hubanalytics.RollupStatement(statement), args...)
}

func (q analyticsRollupReader) QueryRowContext(ctx context.Context, statement string, args ...any) *sql.Row {
	return q.nativeQueryer.QueryRowContext(ctx, hubanalytics.RollupStatement(statement), args...)
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
	if err := rollups.nativeQueryer.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM analytics_rollup_backfill WHERE organization_id=? AND project_id=?) AND NOT EXISTS(SELECT 1 FROM analytics_rollup_positions WHERE organization_id=? AND project_id=? AND (failed=1 OR refresh_phase>=0)) AND NOT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE organization_id=? AND project_id=? AND projection_version IS NULL) AND NOT EXISTS(SELECT 1 FROM collaboration_events WHERE organization_id=? AND project_id=? AND analytics_projection_version IS NULL)`, scope.organization, scope.project, scope.organization, scope.project, scope.organization, scope.project, scope.organization, scope.project).Scan(&complete); err != nil {
		return nil, fmt.Errorf("read analytics rollup status: %w", err)
	}
	if complete {
		return q, nil
	}
	return rollups.nativeQueryer, nil
}

func (d *database) projectAnalyticsBatch(ctx context.Context, tx *sql.Tx, now time.Time, limit int) error {
	return newAnalyticsStore(d).ProjectBatch(ctx, tx, now, limit)
}

func (s *Service) maintainAnalyticsRollups(ctx context.Context, now time.Time, retryFailed bool) {
	if err := s.convertSchedulerDecisions(ctx); err != nil {
		s.config.Logger.ErrorContext(ctx, "convert analytics events", "error", err)
		return
	}
	if err := s.database.convertAnalyticsProjections(ctx); err != nil {
		s.config.Logger.ErrorContext(ctx, "convert analytics projections", "error", err)
		return
	}
	var pending bool
	if err := s.database.reader.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM analytics_hourly_rollups WHERE projection_version IS NULL) OR EXISTS(SELECT 1 FROM collaboration_events WHERE analytics_projection_version IS NULL)").Scan(&pending); err != nil {
		s.config.Logger.ErrorContext(ctx, "read analytics projection progress", "error", err)
		return
	}
	if pending {
		return
	}
	deadline := time.Now().Add(hubanalytics.BackfillBudget)
	for _, step := range []struct {
		name string
		run  func(context.Context, time.Time) (bool, error)
	}{
		{"drain analytics rollups", s.database.drainAnalyticsDirty},
		{"backfill analytics rollups", s.database.backfillAnalyticsRollups},
	} {
		for range hubanalytics.BackfillBatch {
			if !time.Now().Before(deadline) || ctx.Err() != nil {
				break
			}
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
	return newAnalyticsStore(d).Backfill(ctx, now)
}

func (d *database) drainAnalyticsDirty(ctx context.Context, now time.Time) (bool, error) {
	return newAnalyticsStore(d).DrainDirty(ctx, now)
}

func (d *database) retryFailedAnalyticsRollups(ctx context.Context, now time.Time) error {
	return newAnalyticsStore(d).RetryFailed(ctx, now)
}

func refreshAnalyticsIssue(ctx context.Context, tx *sql.Tx, statements *hubanalytics.Statements, id string, now time.Time) error {
	return analyticsStoreFor(nil, nil, statements, nil).RefreshIssue(ctx, tx, statements, id, now)
}

func rebuildAnalyticsRollups(ctx context.Context, tx *sql.Tx, scope nativeScope, now time.Time) error {
	return analyticsStoreFor(nil, nil, nil, nil).Rebuild(ctx, tx, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, now)
}

func readAnalyticsCounter(ctx context.Context, q nativeQueryer, scope nativeScope, metric string, from, to time.Time) (int, error) {
	return analyticsStoreFor(nil, nil, nil, nil).ReadCounter(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, metric, from, to)
}

func readAnalyticsSkipRollups(ctx context.Context, q nativeQueryer, scope nativeScope, out *hubanalytics.Project, compacted bool) (bool, error) {
	return analyticsStoreFor(nil, nil, nil, nil).ReadSkipRollups(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, out, compacted)
}

func analyticsSkipSamplesCompacted(ctx context.Context, q nativeQueryer, scope nativeScope, from time.Time) (bool, error) {
	return analyticsStoreFor(nil, nil, nil, nil).SkipSamplesCompacted(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, from)
}

func readAnalyticsSlotHours(ctx context.Context, q nativeQueryer, scope nativeScope, out *diagnosticsReport) error {
	view := hubanalytics.DiagnosticsView{From: out.From, To: out.To, Partial: out.Partial, Capacity: out.Capacity}
	err := analyticsStoreFor(nil, nil, nil, nil).ReadSlotHours(ctx, q, hubanalytics.Scope{Organization: scope.organization, Project: scope.project}, &view)
	out.Partial = view.Partial
	return err
}

func (d *database) compactAnalyticsSamples(ctx context.Context, now time.Time) error {
	return newAnalyticsStore(d).CompactSamples(ctx, now)
}

func (d *database) compactAnalyticsSamplesWithClock(ctx context.Context, now time.Time, clock func() time.Time) error {
	return newAnalyticsStore(d).CompactSamplesWithClock(ctx, now, clock)
}

func analyticsStoreFor(writer, reader *sql.DB, statements *hubanalytics.Statements, retry *hubanalytics.RetryState) *hubanalytics.Store {
	if retry == nil {
		retry = new(hubanalytics.RetryState)
	}
	return hubanalytics.NewStore(writer, reader, statements, retry, hubanalytics.Hooks{IssueClause: reportsIssueClause, HasIssueFilter: func(ctx context.Context) bool { scope, ok := reportsScopeFrom(ctx); return ok && scope.issues != "" }, DecodeRunHistory: func(ctx context.Context, q hubsql.Queryer, raw string, target *tracker.CollaborationData) error {
		return decodeRunHistory(ctx, q, raw, target)
	}, DecodeStorageJSON: decodeStorageJSON, ProjectionRows: storageConversionRows, RefreshProductivity: func(ctx context.Context, q hubanalytics.ProjectionQuery, scope hubanalytics.Scope, id string) error {
		return refreshProductivitySummary(ctx, q, nativeScope{organization: scope.Organization, project: scope.Project}, id)
	}})
}

func newAnalyticsStore(d *database) *hubanalytics.Store {
	return analyticsStoreFor(d.db, d.reader, d.analytics, &d.analyticsRetry)
}

func (d *database) convertAnalyticsProjections(ctx context.Context) error {
	return newAnalyticsStore(d).ConvertProjections(ctx)
}
