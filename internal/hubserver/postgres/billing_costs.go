package postgres

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

const costPeriodLayout = "2006-01-02T15:04:05.000000000Z"

func costIdentity(values ...string) string {
	var raw []byte
	for _, value := range values {
		raw = strconv.AppendInt(raw, int64(len(value)), 10)
		raw = append(raw, ':')
		raw = append(raw, value...)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func (s *session) BillingCostRevision(ctx context.Context, source billingstore.CostSource) (billingstore.CostRevision, error) {
	key := costIdentity(source.Provider, source.ProviderAccount, source.SourceID, source.From.UTC().Format(costPeriodLayout), source.To.UTC().Format(costPeriodLayout))
	var result billingstore.CostRevision
	var received string
	r := &result.Reference
	err := s.db.QueryRow(ctx, `SELECT s.project_id,s.revision,r.received_at,r.binding_id,r.customer_owned,r.object_key,r.sha256,r.compressed_bytes,r.content_bytes,r.codec,r.format_version,r.kind,r.retention FROM cost_sources s JOIN cost_observation_revisions r ON r.organization_id=s.organization_id AND r.source_key=s.source_key AND r.revision=s.revision WHERE s.organization_id=$1 AND s.source_key=$2`, s.organization, key).Scan(&result.ProjectID, &result.Revision, &received, &r.BindingID, &r.CustomerOwned, &r.Key, &r.SHA256, &r.CompressedBytes, &r.ContentBytes, &r.Codec, &r.FormatVersion, &r.Kind, &r.Retention)
	if err != nil {
		return result, mapError(ctx, err)
	}
	result.ReceivedAt, err = time.Parse(costPeriodLayout, received)
	return result, err
}

func (s *session) BillingMonthlySpend(ctx context.Context, project tracker.ProjectID, month time.Time) (budget.MonthlySpend, error) {
	month = month.UTC()
	month = time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := s.db.Query(ctx, `SELECT bucket, GREATEST(amount_micros,0)::bigint FROM cost_monthly_totals WHERE organization_id=$1 AND project_id=$2 AND month=$3 AND currency='USD' AND bucket IN ('luna_api','runner_api','sprite_infrastructure')`, s.organization, project, month)
	if err != nil {
		return budget.MonthlySpend{}, mapError(ctx, err)
	}
	defer rows.Close()
	var spend budget.MonthlySpend
	for rows.Next() {
		var bucket string
		var amount int64
		if err := rows.Scan(&bucket, &amount); err != nil {
			return spend, err
		}
		switch bucket {
		case "luna_api":
			spend.LunaMicros = amount
		case "runner_api":
			spend.RunnerMicros = amount
		case "sprite_infrastructure":
			spend.SpriteMicros = amount
		}
	}
	return spend, rows.Err()
}

func (s *session) RecordBillingCost(ctx context.Context, record billingstore.CostObservation) (bool, error) {
	results, err := s.RecordBillingCosts(ctx, []billingstore.CostObservation{record})
	if err != nil {
		return false, err
	}
	return results[0], nil
}

func (s *session) RecordBillingCosts(ctx context.Context, records []billingstore.CostObservation) ([]bool, error) {
	return s.recordBillingCosts(ctx, records, nil)
}

func (s *session) recordBillingCosts(ctx context.Context, records []billingstore.CostObservation, states map[string]*runnerCostState) ([]bool, error) {
	if len(records) < 1 || len(records) > 128 {
		return nil, billingstore.ErrInvalidUsage
	}
	keys := make([]string, 0, len(records)*2)
	for _, record := range records {
		if err := s.validateBillingCost(record); err != nil {
			return nil, err
		}
		o := record.Observation
		key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout))
		keys = append(keys, costIdentity(string(s.organization), "source", key), costIdentity(string(s.organization), "resource", o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric))
	}
	slices.Sort(keys)
	for _, key := range slices.Compact(keys) {
		if _, err := s.db.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return nil, mapError(ctx, err)
		}
	}
	results := make([]bool, len(records))
	var deltas []costMonthlyDelta
	for i, record := range records {
		amount := int64(0)
		if record.Observation.AmountMicros != nil && !record.Observation.Voided {
			amount = *record.Observation.AmountMicros
		}
		applied, changes, err := s.recordBillingCost(ctx, record, amount, states)
		if err != nil {
			return nil, err
		}
		results[i] = applied
		deltas = append(deltas, changes...)
	}
	slices.SortFunc(deltas, func(a, b costMonthlyDelta) int {
		if order := a.month.Compare(b.month); order != 0 {
			return order
		}
		if order := cmp.Compare(a.project, b.project); order != 0 {
			return order
		}
		if order := cmp.Compare(a.bucket, b.bucket); order != 0 {
			return order
		}
		return cmp.Compare(a.currency, b.currency)
	})
	for _, delta := range deltas {
		_, err := s.db.Exec(ctx, `INSERT INTO cost_monthly_totals (organization_id,project_id,month,bucket,currency,amount_micros) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (organization_id,project_id,month,bucket,currency) DO UPDATE SET amount_micros=cost_monthly_totals.amount_micros+EXCLUDED.amount_micros`, s.organization, delta.project, delta.month, delta.bucket, delta.currency, delta.amount)
		if err != nil {
			return nil, mapError(ctx, err)
		}
	}
	return results, nil
}

type costMonthlyDelta struct {
	project  tracker.ProjectID
	month    time.Time
	bucket   string
	currency string
	amount   int64
}

func (s *session) validateBillingCost(record billingstore.CostObservation) error {
	if err := s.checkOrganization(record.OrganizationID); err != nil {
		return err
	}
	o := record.Observation
	raw, err := json.Marshal(o)
	if err != nil {
		return billingstore.ErrInvalidUsage
	}
	archive, _, err := attachment.PrepareArchive(string(s.organization), 1, bytes.NewReader(raw))
	if err != nil || archive.SHA256 != record.Reference.SHA256 || archive.UncompressedSize != record.Reference.ContentBytes || archive.CompressedSize != record.Reference.CompressedBytes {
		return billingstore.ErrInvalidUsage
	}
	if record.Reference.ArchiveReference().Validate(string(s.organization)) != nil || record.ReceivedAt.IsZero() || o.Revision < 1 || o.Provider == "" || o.ProviderAccount == "" || o.SourceID == "" || o.ResourceID == "" || o.Bucket == "" || o.Metric == "" || o.Currency == "" {
		return billingstore.ErrInvalidUsage
	}
	if o.AmountMicros != nil && (*o.AmountMicros < -1e15 || *o.AmountMicros > 1e15) {
		return billingstore.ErrInvalidAmount
	}
	_, err = usagecost.AllocateMonths(0, o.From, o.To)
	return err
}

func (s *session) recordBillingCost(ctx context.Context, record billingstore.CostObservation, amount int64, states map[string]*runnerCostState) (bool, []costMonthlyDelta, error) {
	o := record.Observation
	allocations, err := usagecost.AllocateMonths(amount, o.From, o.To)
	if err != nil {
		return false, nil, err
	}
	from, to := o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout)
	key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, from, to)
	attribution := costIdentity(string(record.ProjectID), o.ResourceID, o.Bucket, o.Metric, o.Unit, o.Currency, o.AttemptID, o.WorkItemID, o.Model, o.UsageKind, o.RunnerID, o.MachineID, o.Placement)
	var revision int64
	var previousHash, previousAttribution string
	var previousAmount pgtype.Int8
	var previousVoided bool
	state, cached := states[key]
	if cached {
		revision, previousHash, previousAttribution, previousAmount, previousVoided = state.revision, state.hash, state.attribution, state.amount, state.voided
		if !state.found {
			err = pgx.ErrNoRows
		}
	} else {
		err = s.db.QueryRow(ctx, `SELECT revision,payload_hash,attribution_hash,amount_micros,voided FROM cost_sources WHERE organization_id=$1 AND source_key=$2 FOR UPDATE`, s.organization, key).Scan(&revision, &previousHash, &previousAttribution, &previousAmount, &previousVoided)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, nil, mapError(ctx, err)
	}
	if err == nil {
		if previousAttribution != attribution {
			return false, nil, mutation.ErrConflict
		}
		if o.Revision < revision {
			return false, nil, nil
		}
		if o.Revision == revision {
			if previousHash != record.Reference.SHA256 {
				return false, nil, mutation.ErrConflict
			}
			return false, nil, nil
		}
	}
	var overlap bool
	if cached {
		overlap = state.overlap
		err = nil
	} else {
		err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM cost_sources WHERE organization_id=$1 AND provider=$2 AND provider_account=$3 AND resource_id=$4 AND bucket=$5 AND metric=$6 AND NOT voided AND source_key<>$7 AND period_start<$8 AND period_end>$9)`, s.organization, o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric, key, to, from).Scan(&overlap)
	}
	if err != nil {
		return false, nil, mapError(ctx, err)
	}
	if overlap && !o.Voided {
		return false, nil, mutation.ErrConflict
	}
	_, err = s.db.Exec(ctx, `INSERT INTO cost_sources (organization_id,source_key,project_id,provider,provider_account,resource_id,bucket,metric,period_start,period_end,revision,attribution_hash,payload_hash,amount_micros,currency,voided) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT (organization_id,source_key) DO UPDATE SET revision=EXCLUDED.revision,payload_hash=EXCLUDED.payload_hash,amount_micros=EXCLUDED.amount_micros,voided=EXCLUDED.voided`, s.organization, key, record.ProjectID, o.Provider, o.ProviderAccount, o.ResourceID, o.Bucket, o.Metric, from, to, o.Revision, attribution, record.Reference.SHA256, o.AmountMicros, o.Currency, o.Voided)
	if err != nil {
		return false, nil, mapError(ctx, err)
	}
	r := record.Reference
	_, err = s.db.Exec(ctx, `INSERT INTO cost_observation_revisions (organization_id,source_key,revision,received_at,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, s.organization, key, o.Revision, record.ReceivedAt.UTC().Format(costPeriodLayout), r.BindingID, r.CustomerOwned, r.Key, r.SHA256, r.CompressedBytes, r.ContentBytes, r.Codec, r.FormatVersion, r.Kind, r.Retention)
	if err != nil {
		return false, nil, mapError(ctx, err)
	}
	oldAmount := int64(0)
	if previousAmount.Valid && !previousVoided {
		oldAmount = previousAmount.Int64
	}
	previous, err := usagecost.AllocateMonths(oldAmount, o.From, o.To)
	if err != nil {
		return false, nil, err
	}
	projects := []tracker.ProjectID{""}
	if record.ProjectID != "" {
		projects = append(projects, record.ProjectID)
	}
	var deltas []costMonthlyDelta
	for i, allocation := range allocations {
		for _, project := range projects {
			deltas = append(deltas, costMonthlyDelta{project: project, month: allocation.Month, bucket: o.Bucket, currency: o.Currency, amount: allocation.Micros - previous[i].Micros})
		}
	}
	if cached {
		state.found, state.revision, state.hash, state.attribution, state.voided = true, o.Revision, record.Reference.SHA256, attribution, o.Voided
		state.amount = pgtype.Int8{}
		if o.AmountMicros != nil {
			state.amount = pgtype.Int8{Int64: *o.AmountMicros, Valid: true}
		}
	}
	return true, deltas, nil
}

func (s *session) ImportBillingCostRevision(ctx context.Context, record billingstore.CostObservation) (bool, error) {
	if err := s.validateBillingCost(record); err != nil {
		return false, err
	}
	o := record.Observation
	key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout))
	attribution := costIdentity(string(record.ProjectID), o.ResourceID, o.Bucket, o.Metric, o.Unit, o.Currency, o.AttemptID, o.WorkItemID, o.Model, o.UsageKind, o.RunnerID, o.MachineID, o.Placement)
	var latest int64
	var currentAttribution string
	err := s.db.QueryRow(ctx, `SELECT revision,attribution_hash FROM cost_sources WHERE organization_id=$1 AND source_key=$2 FOR UPDATE`, s.organization, key).Scan(&latest, &currentAttribution)
	if err != nil {
		return false, mapError(ctx, err)
	}
	if o.Revision > latest || attribution != currentAttribution {
		return false, mutation.ErrConflict
	}
	var digest string
	err = s.db.QueryRow(ctx, `SELECT sha256 FROM cost_observation_revisions WHERE organization_id=$1 AND source_key=$2 AND revision=$3`, s.organization, key, o.Revision).Scan(&digest)
	if err == nil {
		if digest != record.Reference.SHA256 {
			return false, mutation.ErrConflict
		}
		return false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, mapError(ctx, err)
	}
	r := record.Reference
	_, err = s.db.Exec(ctx, `INSERT INTO cost_observation_revisions (organization_id,source_key,revision,received_at,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, s.organization, key, o.Revision, record.ReceivedAt.UTC().Format(costPeriodLayout), r.BindingID, r.CustomerOwned, r.Key, r.SHA256, r.CompressedBytes, r.ContentBytes, r.Codec, r.FormatVersion, r.Kind, r.Retention)
	return err == nil, mapError(ctx, err)
}
