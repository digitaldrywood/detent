package postgres

import (
	"context"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

type runnerCostState struct {
	revision          int64
	hash, attribution string
	amount            pgtype.Int8
	voided, found     bool
	overlap           bool
}

func (f *runnerEventSession) RecordBillingCosts(ctx context.Context, records []billingstore.CostObservation) ([]bool, error) {
	if len(records) < 1 || len(records) > 128 {
		return nil, billingstore.ErrInvalidUsage
	}
	batch := new(pgx.Batch)
	keys := make([]string, 0, len(records)*2)
	for _, record := range records {
		if err := f.session.validateBillingCost(record); err != nil {
			return nil, err
		}
		o := record.Observation
		key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout))
		keys = append(keys, costIdentity(string(f.session.organization), "source", key), costIdentity(string(f.session.organization), "resource", o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric))
		for _, prior := range records {
			p := prior.Observation
			other := costIdentity(p.Provider, p.ProviderAccount, p.SourceID, p.From.UTC().Format(costPeriodLayout), p.To.UTC().Format(costPeriodLayout))
			if key != other && o.Provider == p.Provider && o.ProviderAccount == p.ProviderAccount && o.ResourceID == p.ResourceID && o.Bucket == p.Bucket && o.Metric == p.Metric && !o.Voided && !p.Voided && o.From.Before(p.To) && p.From.Before(o.To) {
				return nil, mutation.ErrConflict
			}
		}
	}
	slices.Sort(keys)
	for _, key := range slices.Compact(keys) {
		batch.Queue(`SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key)
	}
	states := map[string]*runnerCostState{}
	for _, record := range records {
		o := record.Observation
		key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout))
		state := new(runnerCostState)
		states[key] = state
		batch.Queue(`SELECT revision,payload_hash,attribution_hash,amount_micros,voided FROM cost_sources WHERE organization_id=$1 AND source_key=$2 FOR UPDATE`, f.session.organization, key).Query(func(rows pgx.Rows) error {
			if rows.Next() {
				state.found = true
				if err := rows.Scan(&state.revision, &state.hash, &state.attribution, &state.amount, &state.voided); err != nil {
					return err
				}
			}
			return rows.Err()
		})
		batch.Queue(`SELECT EXISTS(SELECT 1 FROM cost_sources WHERE organization_id=$1 AND provider=$2 AND provider_account=$3 AND resource_id=$4 AND bucket=$5 AND metric=$6 AND NOT voided AND source_key<>$7 AND period_start<$8 AND period_end>$9)`, f.session.organization, o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric, key, o.To.UTC().Format(costPeriodLayout), o.From.UTC().Format(costPeriodLayout)).QueryRow(func(row pgx.Row) error { return row.Scan(&state.overlap) })
	}
	db, ok := f.session.db.(timedQueries)
	if !ok {
		return nil, corestore.ErrInvalidPage
	}
	executor, ok := db.DBTX.(interface {
		SendBatch(context.Context, *pgx.Batch) pgx.BatchResults
	})
	if !ok {
		return nil, corestore.ErrInvalidPage
	}
	started := time.Now()
	err := executor.SendBatch(ctx, batch).Close()
	for _, query := range batch.QueuedQueries {
		observeStatement(ctx, started, query.SQL)
	}
	if err != nil {
		return nil, mapError(ctx, err)
	}
	s := &session{organization: f.session.organization, db: runEventQueue{f.pending}}
	return s.recordBillingCosts(ctx, records, states)
}

func (f *runnerEventSession) RefreshReportUsage(ctx context.Context, records []billingstore.CostObservation) error {
	s := &session{organization: f.session.organization, db: runEventQueue{f.pending}}
	return s.RefreshReportUsage(ctx, records)
}
