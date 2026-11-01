package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (s *session) BillingCreditAccount(ctx context.Context, mode string) (billingstore.CreditAccount, error) {
	var row pgquery.AiCreditAccount
	var err error
	params := pgquery.BillingCreditAccountParams{OrganizationID: s.organization, Mode: mode}
	if s.readOnly {
		row, err = s.queries.BillingCreditAccount(ctx, params)
	} else {
		row, err = s.queries.LockBillingCreditAccount(ctx, pgquery.LockBillingCreditAccountParams{OrganizationID: s.organization, Mode: mode})
	}
	return billingstore.CreditAccount(row), mapError(ctx, err)
}

func (s *session) CreateBillingCreditAccount(ctx context.Context, row billingstore.CreditAccount) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	return mapError(ctx, s.queries.CreateBillingCreditAccount(ctx, pgquery.CreateBillingCreditAccountParams(row)))
}

func (s *session) SetBillingCreditFunding(ctx context.Context, row billingstore.CreditAccount) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	n, err := s.queries.SetBillingCreditFunding(ctx, pgquery.SetBillingCreditFundingParams{OrganizationID: s.organization, Mode: row.Mode, AutoEnabled: row.AutoEnabled, ThresholdCents: row.ThresholdCents, PriceID: row.PriceID, PaymentMethod: row.PaymentMethod, Failure: row.Failure})
	return affected(ctx, n, err)
}

func (s *session) PostBillingCredit(ctx context.Context, row billingstore.CreditTransaction) (bool, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return false, err
	}
	if row.Reason != "" || row.ReasonReference != (billingstore.AuditReference{}) && row.ReasonReference.ArchiveReference().Validate(string(s.organization)) != nil {
		return false, billingstore.ErrInvalidAmount
	}
	if _, err := s.queries.LockBillingCreditAccount(ctx, pgquery.LockBillingCreditAccountParams{OrganizationID: s.organization, Mode: row.Mode}); err != nil {
		return false, mapError(ctx, err)
	}
	previous, err := s.queries.BillingCreditTransactionBySource(ctx, pgquery.BillingCreditTransactionBySourceParams{OrganizationID: s.organization, Mode: row.Mode, Source: row.Source})
	if err == nil {
		stored := creditTransaction(previous.OrganizationID, previous.ID, previous.Mode, previous.Source, previous.AmountMicros, previous.IncludedAmountMicros, previous.Kind, previous.Actor, previous.RecordedAt, previous.BindingID, previous.CustomerOwned, previous.ObjectKey, previous.Sha256, previous.CompressedBytes, previous.ContentBytes, previous.Codec, previous.FormatVersion, previous.ReferenceKind, previous.Retention)
		if stored.AmountMicros != row.AmountMicros || stored.IncludedAmountMicros != row.IncludedAmountMicros || stored.Kind != row.Kind || stored.Actor != row.Actor || stored.ReasonReference != row.ReasonReference {
			return false, mutation.ErrConflict
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, mapError(ctx, err)
	}
	err = s.queries.CreateBillingCreditTransaction(ctx, pgquery.CreateBillingCreditTransactionParams{OrganizationID: s.organization, Mode: row.Mode, Source: row.Source, AmountMicros: row.AmountMicros, IncludedAmountMicros: row.IncludedAmountMicros, Kind: row.Kind, Actor: row.Actor, RecordedAt: row.RecordedAt})
	if err != nil {
		return false, mapError(ctx, err)
	}
	if row.ReasonReference.Key != "" {
		ref := row.ReasonReference
		if err := s.queries.CreateBillingCreditReasonReference(ctx, pgquery.CreateBillingCreditReasonReferenceParams{OrganizationID: s.organization, Mode: row.Mode, Source: row.Source, BindingID: ref.BindingID, CustomerOwned: ref.CustomerOwned, ObjectKey: ref.Key, Sha256: ref.SHA256, CompressedBytes: ref.CompressedBytes, ContentBytes: ref.ContentBytes, Codec: ref.Codec, FormatVersion: int32(ref.FormatVersion), Kind: ref.Kind, Retention: ref.Retention}); err != nil {
			return false, mapError(ctx, err)
		}
	}
	balanceDelta := row.AmountMicros
	if row.Kind == "usage" {
		if row.AmountMicros > 0 || row.IncludedAmountMicros < 0 || row.IncludedAmountMicros > -row.AmountMicros {
			return false, billingstore.ErrInvalidAmount
		}
		balanceDelta += row.IncludedAmountMicros
	}
	n, err := s.queries.AddBillingCreditBalance(ctx, pgquery.AddBillingCreditBalanceParams{OrganizationID: s.organization, Mode: row.Mode, BalanceMicros: balanceDelta})
	if err := affected(ctx, n, err); err != nil {
		return false, err
	}
	return true, nil
}

func (s *session) PostBillingUsageCredit(ctx context.Context, period billingstore.CreditPeriod, row billingstore.CreditTransaction, amount int64) (bool, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return false, err
	}
	if err := s.checkOrganization(period.OrganizationID); err != nil {
		return false, err
	}
	if row.Kind != "usage" || row.AmountMicros >= 0 || amount < 0 || amount != -row.AmountMicros || row.IncludedAmountMicros != 0 || period.Mode != row.Mode || period.StartsAt.IsZero() || !period.EndsAt.After(period.StartsAt) || row.RecordedAt.Before(period.StartsAt) || !row.RecordedAt.Before(period.EndsAt) || period.AllowanceMicros < 0 {
		return false, billingstore.ErrInvalidAmount
	}
	if _, err := s.queries.LockBillingCreditAccount(ctx, pgquery.LockBillingCreditAccountParams{OrganizationID: s.organization, Mode: row.Mode}); err != nil {
		return false, mapError(ctx, err)
	}
	previous, err := s.BillingCreditTransactionBySource(ctx, row.Mode, row.Source)
	if err == nil {
		if previous.AmountMicros != row.AmountMicros || previous.Kind != row.Kind || previous.Actor != row.Actor || previous.ReasonReference != row.ReasonReference || previous.IncludedAmountMicros < 0 || previous.IncludedAmountMicros > -row.AmountMicros {
			return false, mutation.ErrConflict
		}
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, corestore.ErrNotFound) {
		return false, err
	}
	if period.AllowanceMicros > 0 {
		row.IncludedAmountMicros, err = s.SpendBillingIncludedCredit(ctx, period, amount)
		if err != nil {
			return false, err
		}
	}
	return s.PostBillingCredit(ctx, row)
}

func (s *session) BillingCreditTransactionBySource(ctx context.Context, mode, source string) (billingstore.CreditTransaction, error) {
	row, err := s.queries.BillingCreditTransactionBySource(ctx, pgquery.BillingCreditTransactionBySourceParams{OrganizationID: s.organization, Mode: mode, Source: source})
	if err != nil {
		return billingstore.CreditTransaction{}, mapError(ctx, err)
	}
	return creditTransaction(row.OrganizationID, row.ID, row.Mode, row.Source, row.AmountMicros, row.IncludedAmountMicros, row.Kind, row.Actor, row.RecordedAt, row.BindingID, row.CustomerOwned, row.ObjectKey, row.Sha256, row.CompressedBytes, row.ContentBytes, row.Codec, row.FormatVersion, row.ReferenceKind, row.Retention), nil
}

func creditTransaction(organization tracker.OrganizationID, id int64, mode, source string, amount, included int64, kind, actor string, recordedAt time.Time, bindingID *string, customerOwned sql.NullBool, key, digest *string, compressed, content sql.NullInt64, codec *string, version sql.NullInt32, referenceKind, retention *string) billingstore.CreditTransaction {
	result := billingstore.CreditTransaction{OrganizationID: organization, ID: id, Mode: mode, Source: source, AmountMicros: amount, IncludedAmountMicros: included, Kind: kind, Actor: actor, RecordedAt: recordedAt}
	result.ReasonReference = creditReference(bindingID, customerOwned, key, digest, compressed, content, codec, version, referenceKind, retention)
	return result
}

func creditReference(bindingID *string, customerOwned sql.NullBool, key, digest *string, compressed, content sql.NullInt64, codec *string, version sql.NullInt32, referenceKind, retention *string) billingstore.AuditReference {
	if key == nil {
		return billingstore.AuditReference{}
	}
	return billingstore.AuditReference{BindingID: stringValue(bindingID), CustomerOwned: customerOwned.Bool, Key: *key, SHA256: stringValue(digest), CompressedBytes: compressed.Int64, ContentBytes: content.Int64, Codec: stringValue(codec), FormatVersion: int(version.Int32), Kind: stringValue(referenceKind), Retention: stringValue(retention)}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (s *session) BillingCreditUsage(ctx context.Context, mode string, start, end time.Time) (int64, error) {
	var amount int64
	err := s.db.QueryRowContext(ctx, `SELECT coalesce(-sum(amount_micros),0) FROM ai_credit_transactions WHERE organization_id=$1 AND mode=$2 AND kind='usage' AND recorded_at >= $3 AND recorded_at < $4`, s.organization, mode, start, end).Scan(&amount)
	return amount, mapError(ctx, err)
}

func (s *session) BillingCreditTransactions(ctx context.Context, mode string, after int64, limit int) ([]billingstore.CreditTransaction, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.BillingCreditTransactions(ctx, pgquery.BillingCreditTransactionsParams{OrganizationID: s.organization, Mode: mode, AfterID: after, PageSize: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]billingstore.CreditTransaction, len(rows))
	for i, row := range rows {
		result[i] = creditTransaction(row.OrganizationID, row.ID, row.Mode, row.Source, row.AmountMicros, row.IncludedAmountMicros, row.Kind, row.Actor, row.RecordedAt, row.BindingID, row.CustomerOwned, row.ObjectKey, row.Sha256, row.CompressedBytes, row.ContentBytes, row.Codec, row.FormatVersion, row.ReferenceKind, row.Retention)
	}
	return result, nil
}

func (s *session) BillingRecentCreditTransactions(ctx context.Context, mode string, limit int) ([]billingstore.CreditTransaction, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.BillingRecentCreditTransactions(ctx, pgquery.BillingRecentCreditTransactionsParams{OrganizationID: s.organization, Mode: mode, Limit: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]billingstore.CreditTransaction, len(rows))
	for i, row := range rows {
		result[i] = creditTransaction(row.OrganizationID, row.ID, row.Mode, row.Source, row.AmountMicros, row.IncludedAmountMicros, row.Kind, row.Actor, row.RecordedAt, row.BindingID, row.CustomerOwned, row.ObjectKey, row.Sha256, row.CompressedBytes, row.ContentBytes, row.Codec, row.FormatVersion, row.ReferenceKind, row.Retention)
	}
	return result, nil
}

func (s *session) BillingCreditHistory(ctx context.Context, mode string, snapshot, from, to, beforeAt, beforeID int64, limit int) (int64, int64, []billingstore.CreditHistoryEntry, error) {
	if err := checkPage(limit); err != nil {
		return snapshot, 0, nil, err
	}
	if snapshot == 0 {
		if err := s.db.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM ai_credit_transactions WHERE organization_id=$1 AND mode=$2`, s.organization, mode).Scan(&snapshot); err != nil {
			return 0, 0, nil, mapError(ctx, err)
		}
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM (
 SELECT CASE WHEN kind='usage' THEN date_trunc('day',recorded_at AT TIME ZONE 'UTC')::text ELSE 'id:'||id::text END AS group_key,kind
 FROM ai_credit_transactions WHERE organization_id=$1 AND mode=$2 AND id<=$3 AND recorded_at >= to_timestamp($4::double precision/1000000) AND recorded_at < to_timestamp($5::double precision/1000000)
 GROUP BY group_key,kind
) grouped`, s.organization, mode, snapshot, from, to).Scan(&total); err != nil {
		return snapshot, 0, nil, mapError(ctx, err)
	}
	query := `WITH grouped AS (
 SELECT max(id) AS row_id,max(source) AS source,sum(amount_micros) AS amount,sum(included_amount_micros) AS included,kind,max(actor) AS actor,max(recorded_at) AS recorded_at,CASE WHEN kind='usage' THEN count(*) ELSE 0 END AS turns
 FROM ai_credit_transactions
 WHERE organization_id=$1 AND mode=$2 AND id<=$3 AND recorded_at >= to_timestamp($4::double precision/1000000) AND recorded_at < to_timestamp($5::double precision/1000000)
 GROUP BY CASE WHEN kind='usage' THEN date_trunc('day',recorded_at AT TIME ZONE 'UTC')::text ELSE 'id:'||id::text END,kind
)
	SELECT g.row_id,g.amount,g.included,g.kind,g.actor,r.binding_id,r.customer_owned,r.object_key,r.sha256,r.compressed_bytes,r.content_bytes,r.codec,r.format_version,r.kind,r.retention,g.recorded_at,g.turns FROM grouped g
	LEFT JOIN ai_credit_reason_references r ON r.organization_id=$1 AND r.mode=$2 AND r.source=g.source
	WHERE g.recorded_at < to_timestamp($6::double precision/1000000) OR (g.recorded_at = to_timestamp($6::double precision/1000000) AND g.row_id < $7)
	ORDER BY g.recorded_at DESC,g.row_id DESC LIMIT $8`
	rows, err := s.db.QueryContext(ctx, query, s.organization, mode, snapshot, from, to, beforeAt, beforeID, limit)
	if err != nil {
		return snapshot, total, nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := make([]billingstore.CreditHistoryEntry, 0, limit)
	for rows.Next() {
		var row billingstore.CreditHistoryEntry
		var bindingID, objectKey, digest, codec, referenceKind, retention sql.NullString
		var customerOwned sql.NullBool
		var compressed, content sql.NullInt64
		var version sql.NullInt32
		if err := rows.Scan(&row.ID, &row.AmountMicros, &row.IncludedAmountMicros, &row.Kind, &row.Actor, &bindingID, &customerOwned, &objectKey, &digest, &compressed, &content, &codec, &version, &referenceKind, &retention, &row.RecordedAt, &row.Turns); err != nil {
			return snapshot, total, nil, mapError(ctx, err)
		}
		var bindingValue, keyValue, digestValue, codecValue, kindValue, retentionValue *string
		if bindingID.Valid {
			bindingValue = &bindingID.String
		}
		if objectKey.Valid {
			keyValue = &objectKey.String
		}
		if digest.Valid {
			digestValue = &digest.String
		}
		if codec.Valid {
			codecValue = &codec.String
		}
		if referenceKind.Valid {
			kindValue = &referenceKind.String
		}
		if retention.Valid {
			retentionValue = &retention.String
		}
		row.ReasonReference = creditReference(bindingValue, customerOwned, keyValue, digestValue, compressed, content, codecValue, version, kindValue, retentionValue)
		result = append(result, row)
	}
	return snapshot, total, result, mapError(ctx, rows.Err())
}

func (s *session) BillingCreditPeriod(ctx context.Context, mode string, start, end time.Time) (billingstore.CreditPeriod, error) {
	row, err := s.queries.BillingCreditPeriod(ctx, pgquery.BillingCreditPeriodParams{OrganizationID: s.organization, Mode: mode, PeriodStart: start, PeriodEnd: end})
	return billingstore.CreditPeriod{OrganizationID: row.OrganizationID, Mode: row.Mode, StartsAt: row.PeriodStart, EndsAt: row.PeriodEnd, AllowanceMicros: row.AllowanceMicros, UsedMicros: row.UsedMicros}, mapError(ctx, err)
}

func (s *session) SpendBillingIncludedCredit(ctx context.Context, row billingstore.CreditPeriod, amount int64) (int64, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return 0, err
	}
	if amount < 0 {
		return 0, billingstore.ErrInvalidAmount
	}
	if err := s.queries.EnsureBillingCreditPeriod(ctx, pgquery.EnsureBillingCreditPeriodParams{OrganizationID: s.organization, Mode: row.Mode, PeriodStart: row.StartsAt, PeriodEnd: row.EndsAt, AllowanceMicros: row.AllowanceMicros}); err != nil {
		return 0, mapError(ctx, err)
	}
	current, err := s.queries.LockBillingCreditPeriod(ctx, pgquery.LockBillingCreditPeriodParams{OrganizationID: s.organization, Mode: row.Mode, PeriodStart: row.StartsAt, PeriodEnd: row.EndsAt})
	if err != nil {
		return 0, mapError(ctx, err)
	}
	used := min(amount, max(0, current.AllowanceMicros-current.UsedMicros))
	err = s.queries.SpendBillingCreditPeriod(ctx, pgquery.SpendBillingCreditPeriodParams{OrganizationID: s.organization, Mode: row.Mode, PeriodStart: row.StartsAt, PeriodEnd: row.EndsAt, UsedMicros: used})
	return used, mapError(ctx, err)
}
