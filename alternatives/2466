package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (s *session) BillingPrice(ctx context.Context, priceID string) (billingstore.Price, error) {
	var row billingstore.Price
	row.OrganizationID = s.organization
	err := s.db.QueryRowContext(ctx, `SELECT price_id,plan_id,plan_version FROM hosted_billing_prices WHERE organization_id=$1 AND price_id=$2`, s.organization, priceID).Scan(&row.PriceID, &row.Plan.ID, &row.Plan.Version)
	return row, mapError(ctx, err)
}

func (s *session) CreateBillingPrice(ctx context.Context, row billingstore.Price) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO hosted_billing_prices(organization_id,price_id,plan_id,plan_version) VALUES($1,$2,$3,$4)`, s.organization, row.PriceID, row.Plan.ID, row.Plan.Version)
	return mapError(ctx, err)
}

func (s *session) CreditPack(ctx context.Context, priceID, mode string) (billingstore.CreditPack, error) {
	var row billingstore.CreditPack
	row.OrganizationID = s.organization
	err := s.db.QueryRowContext(ctx, `SELECT price_id,mode,usd_cents FROM ai_credit_packs WHERE organization_id=$1 AND price_id=$2 AND mode=$3`, s.organization, priceID, mode).Scan(&row.PriceID, &row.Mode, &row.USDCents)
	return row, mapError(ctx, err)
}

func (s *session) CreateCreditPack(ctx context.Context, row billingstore.CreditPack) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO ai_credit_packs(organization_id,price_id,mode,usd_cents) VALUES($1,$2,$3,$4)`, s.organization, row.PriceID, row.Mode, row.USDCents)
	return mapError(ctx, err)
}

func (s *session) BillingCustomerIntent(ctx context.Context) (billingstore.CustomerIntent, error) {
	var row billingstore.CustomerIntent
	row.OrganizationID = s.organization
	err := s.db.QueryRowContext(ctx, `SELECT account_id,mode,idempotency_key,state,customer_id,created_at,updated_at FROM hosted_billing_customer_intents WHERE organization_id=$1`, s.organization).Scan(&row.AccountID, &row.Mode, &row.IdempotencyKey, &row.State, &row.CustomerID, &row.CreatedAt, &row.UpdatedAt)
	return row, mapError(ctx, err)
}

func (s *session) CreateBillingCustomerIntent(ctx context.Context, row billingstore.CustomerIntent) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO hosted_billing_customer_intents(organization_id,account_id,mode,idempotency_key,state,customer_id,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(organization_id) DO NOTHING`, s.organization, row.AccountID, row.Mode, row.IdempotencyKey, row.State, row.CustomerID, row.CreatedAt, row.UpdatedAt)
	if err != nil {
		return mapError(ctx, err)
	}
	stored, err := s.BillingCustomerIntent(ctx)
	if err != nil {
		return err
	}
	if stored.AccountID != row.AccountID || stored.Mode != row.Mode || stored.IdempotencyKey != row.IdempotencyKey {
		return corestore.ErrRevisionConflict
	}
	return nil
}

func (s *session) SetBillingCustomerIntent(ctx context.Context, row billingstore.CustomerIntent) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE hosted_billing_customer_intents SET state=$1,customer_id=$2,updated_at=$3 WHERE organization_id=$4 AND account_id=$5 AND mode=$6 AND idempotency_key=$7`, row.State, row.CustomerID, row.UpdatedAt, s.organization, row.AccountID, row.Mode, row.IdempotencyKey)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	return affected(ctx, count, err)
}

func (s *session) DeleteBillingCustomerIntent(ctx context.Context, mode string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM hosted_billing_customer_intents WHERE organization_id=$1 AND mode=$2`, s.organization, mode)
	return mapError(ctx, err)
}

func (s *session) BillingEvents(ctx context.Context, after, through int64, limit int) ([]billingstore.Event, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	return s.billingEvents(ctx, limit, `SELECT sequence,event_id,event_type,received_at,processed_at,credit_processed FROM hosted_billing_events WHERE organization_id=$1 AND sequence>$2 AND sequence<=$3 AND (processed_at IS NULL OR NOT credit_processed) ORDER BY sequence LIMIT $4`, s.organization, after, through, limit)
}

func (s *session) PendingBillingEvents(ctx context.Context, limit int) ([]billingstore.Event, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	return s.billingEvents(ctx, limit, `SELECT sequence,event_id,event_type,received_at,processed_at,credit_processed FROM hosted_billing_events WHERE organization_id=$1 AND NOT credit_processed ORDER BY sequence LIMIT $2`, s.organization, limit)
}

func (s *session) UnprocessedBillingEvents(ctx context.Context, limit int) ([]billingstore.Event, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	return s.billingEvents(ctx, limit, `SELECT sequence,event_id,event_type,received_at,processed_at,credit_processed FROM hosted_billing_events WHERE organization_id=$1 AND processed_at IS NULL ORDER BY sequence LIMIT $2`, s.organization, limit)
}

func (s *session) billingEvents(ctx context.Context, limit int, query string, args ...any) ([]billingstore.Event, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := make([]billingstore.Event, 0, limit)
	for rows.Next() {
		var row billingstore.Event
		var processed sql.NullTime
		row.OrganizationID = s.organization
		if err := rows.Scan(&row.Sequence, &row.ID, &row.Type, &row.ReceivedAt, &processed, &row.CreditProcessed); err != nil {
			return nil, mapError(ctx, err)
		}
		if processed.Valid {
			row.ProcessedAt = &processed.Time
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) BillingEvent(ctx context.Context, eventID string) (billingstore.Event, error) {
	var row billingstore.Event
	var processed sql.NullTime
	row.OrganizationID = s.organization
	err := s.db.QueryRowContext(ctx, `SELECT sequence,event_id,event_type,received_at,processed_at,credit_processed FROM hosted_billing_events WHERE organization_id=$1 AND event_id=$2`, s.organization, eventID).Scan(&row.Sequence, &row.ID, &row.Type, &row.ReceivedAt, &processed, &row.CreditProcessed)
	if processed.Valid {
		row.ProcessedAt = &processed.Time
	}
	return row, mapError(ctx, err)
}

func (s *session) CreateBillingEvent(ctx context.Context, row billingstore.Event) (bool, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO hosted_billing_events(organization_id,event_id,event_type,received_at,credit_processed) VALUES($1,$2,$3,$4,$5) ON CONFLICT(organization_id,event_id) DO NOTHING`, s.organization, row.ID, row.Type, row.ReceivedAt, row.CreditProcessed)
	if err != nil {
		return false, mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	return count == 1, mapError(ctx, err)
}

func (s *session) SetBillingEventProcessed(ctx context.Context, eventID string, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE hosted_billing_events SET processed_at=$1 WHERE organization_id=$2 AND event_id=$3 AND processed_at IS NULL`, at, s.organization, eventID)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		var exists bool
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hosted_billing_events WHERE organization_id=$1 AND event_id=$2 AND processed_at IS NOT NULL)`, s.organization, eventID).Scan(&exists)
		if err == nil && !exists {
			return sql.ErrNoRows
		}
	}
	if err != nil {
		return mapError(ctx, err)
	}
	return nil
}

func (s *session) SetBillingEventCreditProcessed(ctx context.Context, eventID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE hosted_billing_events SET credit_processed=true WHERE organization_id=$1 AND event_id=$2 AND NOT credit_processed`, s.organization, eventID)
	if err != nil {
		return mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err == nil && count == 0 {
		var exists bool
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM hosted_billing_events WHERE organization_id=$1 AND event_id=$2 AND credit_processed)`, s.organization, eventID).Scan(&exists)
		if err == nil && !exists {
			return sql.ErrNoRows
		}
	}
	if err != nil {
		return mapError(ctx, err)
	}
	return nil
}

func (s *session) BillingPlanAudit(ctx context.Context, commandID string) (billingstore.PlanAudit, error) {
	var row billingstore.PlanAudit
	row.OrganizationID = s.organization
	err := s.db.QueryRowContext(ctx, `SELECT id,command_id,request_hash,actor_id,action,recorded_at,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention FROM hosted_plan_audit WHERE organization_id=$1 AND command_id=$2`, s.organization, commandID).Scan(&row.ID, &row.CommandID, &row.RequestHash, &row.ActorID, &row.Action, &row.RecordedAt, &row.Reference.BindingID, &row.Reference.CustomerOwned, &row.Reference.Key, &row.Reference.SHA256, &row.Reference.CompressedBytes, &row.Reference.ContentBytes, &row.Reference.Codec, &row.Reference.FormatVersion, &row.Reference.Kind, &row.Reference.Retention)
	return row, mapError(ctx, err)
}

func (s *session) BillingPlanAudits(ctx context.Context, after int64, limit int) ([]billingstore.PlanAudit, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,command_id,request_hash,actor_id,action,recorded_at,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention FROM hosted_plan_audit WHERE organization_id=$1 AND id>$2 ORDER BY id LIMIT $3`, s.organization, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := make([]billingstore.PlanAudit, 0, limit)
	for rows.Next() {
		var row billingstore.PlanAudit
		row.OrganizationID = s.organization
		if err := rows.Scan(&row.ID, &row.CommandID, &row.RequestHash, &row.ActorID, &row.Action, &row.RecordedAt, &row.Reference.BindingID, &row.Reference.CustomerOwned, &row.Reference.Key, &row.Reference.SHA256, &row.Reference.CompressedBytes, &row.Reference.ContentBytes, &row.Reference.Codec, &row.Reference.FormatVersion, &row.Reference.Kind, &row.Reference.Retention); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) CreateBillingPlanAudit(ctx context.Context, row billingstore.PlanAudit) (bool, error) {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO hosted_plan_audit(organization_id,command_id,request_hash,actor_id,action,recorded_at,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) ON CONFLICT(organization_id,command_id) DO NOTHING`, s.organization, row.CommandID, row.RequestHash, row.ActorID, row.Action, row.RecordedAt, row.Reference.BindingID, row.Reference.CustomerOwned, row.Reference.Key, row.Reference.SHA256, row.Reference.CompressedBytes, row.Reference.ContentBytes, row.Reference.Codec, row.Reference.FormatVersion, row.Reference.Kind, row.Reference.Retention)
	if err != nil {
		return false, mapError(ctx, err)
	}
	count, err := result.RowsAffected()
	if err != nil || count != 0 {
		return count == 1, mapError(ctx, err)
	}
	previous, err := s.BillingPlanAudit(ctx, row.CommandID)
	if err != nil {
		return false, err
	}
	if previous.RequestHash != row.RequestHash || previous.ActorID != row.ActorID || previous.Action != row.Action || previous.Reference != row.Reference {
		return false, corestore.ErrRevisionConflict
	}
	return false, nil
}

func (s *session) CreateBillingAudit(ctx context.Context, row billingstore.Audit) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO hosted_billing_audit(organization_id,actor_id,action,recorded_at,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, s.organization, row.ActorID, row.Action, row.RecordedAt, row.Reference.BindingID, row.Reference.CustomerOwned, row.Reference.Key, row.Reference.SHA256, row.Reference.CompressedBytes, row.Reference.ContentBytes, row.Reference.Codec, row.Reference.FormatVersion, row.Reference.Kind, row.Reference.Retention)
	return mapError(ctx, err)
}

func (s *session) BillingAudits(ctx context.Context, after int64, limit int) ([]billingstore.Audit, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,actor_id,action,recorded_at,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention FROM hosted_billing_audit WHERE organization_id=$1 AND id<$2 ORDER BY id DESC LIMIT $3`, s.organization, after, limit)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	result := make([]billingstore.Audit, 0, limit)
	for rows.Next() {
		var row billingstore.Audit
		row.OrganizationID = s.organization
		if err := rows.Scan(&row.ID, &row.ActorID, &row.Action, &row.RecordedAt, &row.Reference.BindingID, &row.Reference.CustomerOwned, &row.Reference.Key, &row.Reference.SHA256, &row.Reference.CompressedBytes, &row.Reference.ContentBytes, &row.Reference.Codec, &row.Reference.FormatVersion, &row.Reference.Kind, &row.Reference.Retention); err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, row)
	}
	return result, mapError(ctx, rows.Err())
}

func (s *session) CreateRetiredBillingAccount(ctx context.Context, row billingstore.RetiredAccount) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO hosted_billing_retired(organization_id,account_id,customer_id,mode,retired_at,binding_id,customer_owned,object_key,sha256,compressed_bytes,content_bytes,codec,format_version,kind,retention) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, s.organization, row.AccountID, row.CustomerID, row.Mode, row.RetiredAt, row.Reference.BindingID, row.Reference.CustomerOwned, row.Reference.Key, row.Reference.SHA256, row.Reference.CompressedBytes, row.Reference.ContentBytes, row.Reference.Codec, row.Reference.FormatVersion, row.Reference.Kind, row.Reference.Retention)
	return mapError(ctx, err)
}
