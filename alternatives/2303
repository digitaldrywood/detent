package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var ErrMetricImport = errors.New("invalid request metric import")

type MetricsImportSource struct {
	DB         *sql.DB
	Catalog    storagecatalog.Catalog
	Source     storagecatalog.Source
	Schema     storagecatalog.Schema
	SnapshotID string
	RetainAt   time.Time
}

type MetricsImportProgress struct {
	Processed int64
	Done      bool
}

type MetricsImporter struct {
	repository   *Repository
	source       MetricsImportSource
	binding      *storagecatalog.Binding
	digest       string
	organization tracker.OrganizationID
}

var metricImportGrains = [...]metricsstore.Grain{metricsstore.Day, metricsstore.Minute, metricsstore.Hour}

func (r *Repository) NewMetricsImporter(ctx context.Context, source MetricsImportSource) (*MetricsImporter, error) {
	if source.DB == nil || source.DB.Stats().MaxOpenConnections < 1 || source.Source.Store != "metrics" || source.SnapshotID == "" || len(source.SnapshotID) > 200 || source.RetainAt.IsZero() || source.RetainAt.Nanosecond()%1000 != 0 {
		return nil, ErrMetricImport
	}
	encoded, err := json.Marshal(struct {
		Catalog storagecatalog.Catalog
		Schema  storagecatalog.Schema
	}{source.Catalog, source.Schema})
	if err != nil {
		return nil, ErrMetricImport
	}
	var frozen struct {
		Catalog storagecatalog.Catalog
		Schema  storagecatalog.Schema
	}
	if err := json.Unmarshal(encoded, &frozen); err != nil {
		return nil, ErrMetricImport
	}
	source.Catalog, source.Schema = frozen.Catalog, frozen.Schema
	binding, err := source.Catalog.Bind(source.Source, source.Schema)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	i := &MetricsImporter{repository: r, source: source, binding: binding, digest: hex.EncodeToString(digest[:]), organization: tracker.OrganizationID(source.Source.OrganizationID)}
	if err := i.validateSource(ctx); err != nil {
		return nil, mapError(ctx, err)
	}
	return i, nil
}

func (i *MetricsImporter) Step(ctx context.Context, limit int) (MetricsImportProgress, error) {
	if limit < 1 || limit > metricsstore.MergeLimit {
		return MetricsImportProgress{}, ErrMetricImport
	}
	var checkpoint pgquery.RequestMetricImport
	err := i.repository.transaction(ctx, i.organization, corestore.Background, false, func(s *session) error {
		if err := s.queries.StartRequestMetricImport(ctx, pgquery.StartRequestMetricImportParams{OrganizationID: i.organization, SourceSchema: i.source.Source.Schema, SnapshotID: i.source.SnapshotID, CatalogDigest: i.digest, RetainAt: i.source.RetainAt}); err != nil {
			return err
		}
		var err error
		checkpoint, err = s.queries.ReadRequestMetricImport(ctx, i.organization)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrMetricImport
		}
		return err
	})
	if err != nil {
		return MetricsImportProgress{}, err
	}
	if checkpoint.SourceSchema != i.source.Source.Schema || checkpoint.SnapshotID != i.source.SnapshotID || checkpoint.CatalogDigest != i.digest || !checkpoint.RetainAt.Equal(i.source.RetainAt) {
		return MetricsImportProgress{}, ErrMetricImport
	}
	progress := MetricsImportProgress{Processed: checkpoint.Processed, Done: checkpoint.Stage == 3}
	if progress.Done {
		return progress, nil
	}
	rows, err := i.readPage(ctx, checkpoint, limit)
	if err != nil {
		return progress, mapError(ctx, err)
	}
	err = i.repository.transaction(ctx, i.organization, corestore.Background, false, func(s *session) error {
		current, err := s.queries.LockRequestMetricImport(ctx, i.organization)
		if err != nil {
			return err
		}
		if current.Revision != checkpoint.Revision {
			return corestore.ErrRevisionConflict
		}
		advance := pgquery.AdvanceRequestMetricImportParams{OrganizationID: i.organization, Stage: checkpoint.Stage, Processed: int64(len(rows))}
		for _, row := range rows {
			if err := i.importMetric(ctx, s, metricImportGrains[checkpoint.Stage], row.metric); err != nil {
				return err
			}
			advance.AfterTime, advance.AfterRoute, advance.AfterKind, advance.AfterCaller, advance.AfterStatus = row.at, row.route, row.kind, row.caller, row.status
		}
		if len(rows) < limit {
			advance.Stage++
			advance.AfterTime, advance.AfterRoute, advance.AfterKind, advance.AfterCaller, advance.AfterStatus = "", "", "", "", ""
		}
		if err := s.queries.AdvanceRequestMetricImport(ctx, advance); err != nil {
			return err
		}
		progress.Processed += int64(len(rows))
		progress.Done = advance.Stage == 3
		return nil
	})
	if err != nil {
		return MetricsImportProgress{Processed: checkpoint.Processed}, err
	}
	return progress, nil
}

func (i *MetricsImporter) importMetric(ctx context.Context, s *session, grain metricsstore.Grain, m metricsstore.Metric) error {
	minuteCutoff := i.source.RetainAt.Add(-metricsstore.MinuteRetention).UTC().Truncate(time.Minute)
	hourCutoff := i.source.RetainAt.Add(-metricsstore.HourRetention).UTC().Truncate(time.Hour)
	dayCutoff := i.source.RetainAt.Add(-metricsstore.DayRetention).UTC().Truncate(24 * time.Hour)
	switch grain {
	case metricsstore.Day:
		if m.At.Before(dayCutoff) {
			return nil
		}
		return s.queries.ImportRequestDay(ctx, pgquery.ImportRequestDayParams{OrganizationID: i.organization, Day: m.At, Route: m.Route, CallerKind: m.CallerKind, StatusClass: m.StatusClass, Count: m.Count, SumMs: m.Sum, MaxMs: m.Max})
	case metricsstore.Hour:
		if m.At.Before(hourCutoff) {
			return nil
		}
		return s.queries.ImportRequestHour(ctx, pgquery.ImportRequestHourParams{OrganizationID: i.organization, Hour: m.At, Route: m.Route, StatusClass: m.StatusClass, Count: m.Count, SumMs: m.Sum, MaxMs: m.Max, Histogram: m.Histogram[:]})
	case metricsstore.Minute:
		if m.At.Before(minuteCutoff) {
			day := m.At.Truncate(24 * time.Hour)
			if day.Before(dayCutoff) {
				return nil
			}
			return s.queries.MergeRequestDay(ctx, pgquery.MergeRequestDayParams{OrganizationID: i.organization, Day: day, Route: m.Route, CallerKind: m.CallerKind, StatusClass: m.StatusClass, Count: m.Count, SumMs: m.Sum, MaxMs: m.Max})
		}
		if err := s.queries.LockRequestMetricWindow(ctx, pgquery.LockRequestMetricWindowParams{OrganizationID: i.organization, Minute: m.At}); err != nil {
			return err
		}
		if m.CallerKind != metricsstore.OverflowKind || m.CallerID != metricsstore.OverflowCaller {
			inserted, err := s.queries.ImportRequestMetricCaller(ctx, pgquery.ImportRequestMetricCallerParams{OrganizationID: i.organization, Minute: m.At, CallerKind: m.CallerKind, CallerID: m.CallerID})
			if err != nil {
				return err
			}
			if inserted != 0 {
				if err := s.queries.CountImportedRequestMetricCaller(ctx, pgquery.CountImportedRequestMetricCallerParams{OrganizationID: i.organization, Minute: m.At}); err != nil {
					return err
				}
			}
		}
		return s.queries.ImportRequestMinute(ctx, pgquery.ImportRequestMinuteParams{OrganizationID: i.organization, Minute: m.At, Route: m.Route, CallerKind: m.CallerKind, CallerID: m.CallerID, StatusClass: m.StatusClass, Count: m.Count, SumMs: m.Sum, MaxMs: m.Max, Histogram: m.Histogram[:]})
	}
	return ErrMetricImport
}
