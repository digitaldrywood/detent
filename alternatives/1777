package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

type hostedBillingState struct {
	Snapshot    billing.Snapshot `json:"subscription"`
	Status      string           `json:"status"`
	Plan        PlanReference    `json:"plan"`
	PaidThrough time.Time        `json:"paid_through"`
	AccessUntil time.Time        `json:"access_until"`
	GraceUntil  time.Time        `json:"grace_until"`
}

func resolveHostedBilling(config *HostedBillingConfig, previous hostedBillingState, snapshot billing.Snapshot, now time.Time) hostedBillingState {
	state := hostedBillingState{Snapshot: snapshot, Status: snapshot.Status}
	for _, price := range config.Prices {
		if price.PriceID == snapshot.PriceID {
			state.Plan = price.Plan
		}
	}
	if state.Plan.ID == "" {
		if snapshot.SubscriptionID != "" {
			state.Status = "unapproved_plan"
		}
		return state
	}
	if snapshot.PaymentHold != "" {
		state.Status = snapshot.PaymentHold
		return state
	}
	grace := time.Duration(config.GraceSeconds) * time.Second
	switch snapshot.Status {
	case "active":
		if snapshot.InvoiceID != "" && snapshot.InvoiceStatus == "paid" {
			state.PaidThrough = snapshot.PeriodEnd
			state.GraceUntil = snapshot.PeriodEnd.Add(grace)
			state.AccessUntil = state.GraceUntil
			break
		}
		fallthrough
	case "past_due":
		state.Status = "payment_failed"
		if previous.Snapshot.SubscriptionID == snapshot.SubscriptionID && !previous.PaidThrough.IsZero() {
			state.PaidThrough = previous.PaidThrough
			state.GraceUntil = previous.PaidThrough.Add(grace)
			state.AccessUntil = state.GraceUntil
			state.Plan = previous.Plan
			if state.GraceUntil.After(now) {
				state.Status = "grace"
			}
		}
	case "trialing":
		if !snapshot.TrialEnd.IsZero() && !snapshot.TrialEnd.After(snapshot.PeriodEnd) {
			state.AccessUntil = snapshot.TrialEnd
		}
	}
	if !state.AccessUntil.IsZero() && (snapshot.CancelAtPeriodEnd || !snapshot.CancelAt.IsZero()) {
		end := snapshot.CancelAt
		if end.IsZero() || snapshot.CancelAtPeriodEnd && snapshot.PeriodEnd.Before(end) {
			end = snapshot.PeriodEnd
		}
		if end.Before(state.AccessUntil) {
			state.AccessUntil = end
			state.GraceUntil = time.Time{}
		}
		if state.Status == "active" || state.Status == "trialing" {
			state.Status = "canceling"
		}
	}
	return state
}

func (d *database) readHostedBilling(ctx context.Context) (hostedBillingState, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()

	var result hostedBillingState
	err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		account, err := reader.BillingAccount(ctx)
		if errors.Is(err, corestore.ErrNotFound) {
			result.Status = "free"
			return nil
		}
		if err != nil {
			return err
		}
		result = hostedBillingState{Snapshot: account.Snapshot, Status: account.Status, Plan: PlanReference{ID: account.Plan.ID, Version: account.Plan.Version}, PaidThrough: account.PaidThrough, AccessUntil: account.AccessUntil, GraceUntil: account.GraceUntil}
		return nil
	})
	return result, err
}

func (d *database) commitHostedBilling(ctx context.Context, state hostedBillingState, through int64, unprocessedEventIDs []string, now time.Time) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}

	if d.billingAuditStore == nil {
		return errors.New("hosted billing audit storage is unavailable")
	}
	if len(unprocessedEventIDs) > corestore.MaxPageSize {
		return errors.New("hosted billing reconcile batch exceeds its page size")
	}
	archive, err := d.billingAuditStore.StoreDetail(ctx, string(d.hostedOrganization), raw)
	if err != nil {
		return fmt.Errorf("store hosted billing reconciliation audit: %w", err)
	}
	ref := billingstore.AuditReferenceFromArchive(archive)
	return d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		account, err := writer.BillingAccount(ctx)
		if err != nil {
			return err
		}
		previous := hostedBillingState{Snapshot: account.Snapshot, Status: account.Status, Plan: PlanReference{ID: account.Plan.ID, Version: account.Plan.Version}, PaidThrough: account.PaidThrough, AccessUntil: account.AccessUntil, GraceUntil: account.GraceUntil}
		if !reflect.DeepEqual(previous, state) {
			if err := writer.CreateBillingAudit(ctx, billingstore.Audit{OrganizationID: d.hostedOrganization, ActorID: "stripe", Action: "subscription_reconciled", RecordedAt: now, Reference: ref}); err != nil {
				return err
			}
		}
		account.Status = state.Status
		account.Plan = billingstore.PlanReference{ID: state.Plan.ID, Version: state.Plan.Version}
		account.PaidThrough, account.AccessUntil, account.GraceUntil = state.PaidThrough, state.AccessUntil, state.GraceUntil
		account.Snapshot = state.Snapshot
		account.ReconciledAt = &now
		if state.Snapshot.SubscriptionID != "" {
			account.Checkout = billingstore.Checkout{}
		}
		if err := writer.SetBillingAccount(ctx, account); err != nil {
			return err
		}
		assignment, err := writer.BillingAssignment(ctx)
		if err != nil {
			return err
		}
		var subscription *billingstore.PlanReference
		var expiry *time.Time
		if state.Plan.ID != "" && !state.AccessUntil.IsZero() {
			subscription = &billingstore.PlanReference{ID: state.Plan.ID, Version: state.Plan.Version}
			expiry = &state.AccessUntil
		}
		if !reflect.DeepEqual(assignment.Subscription, subscription) || !reflect.DeepEqual(assignment.ExpiresAt, expiry) {
			assignment.Subscription, assignment.ExpiresAt = subscription, expiry
			if err := writer.SetBillingAssignment(ctx, assignment, assignment.Revision); err != nil {
				return err
			}
		}
		for _, eventID := range unprocessedEventIDs {
			if err := writer.SetBillingEventProcessed(ctx, eventID, now); err != nil {
				return err
			}
		}
		return nil
	})
}

func (d *database) insertBillingAudit(ctx context.Context, tx *sql.Tx, actor, action, raw string, now time.Time) error {
	if m, ok := mutation.FromContext(ctx); ok {
		m.OrganizationID = string(d.hostedOrganization)
		m.RetryIdentity, m.InputHash = "", ""
		summary, err := json.Marshal(struct {
			mutation.Metadata
			Outcome string `json:"outcome"`
		}{m, action})
		if err != nil {
			return err
		}
		raw = string(summary)
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO hosted_billing_audit(organization_id,actor_id,action,record_json,recorded_at) VALUES(?,?,?,?,?)", d.hostedOrganization, actor, action, raw, formatHubTime(now))
	return err
}
