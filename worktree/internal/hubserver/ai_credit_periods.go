package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type includedAICredits struct {
	AllowanceMicros int64     `json:"allowance_micros"`
	UsedMicros      int64     `json:"used_micros"`
	PeriodStart     time.Time `json:"period_start"`
	PeriodEnd       time.Time `json:"period_end"`
	PlanName        string    `json:"plan_name"`
}

func (plan HostedPlan) includedAICreditMicros() *int64 {
	amount := int64(0)
	tier, ok := hostedTierFor(plan.PlanReference)
	if !ok || !tier.offered || plan.ID == "free" {
		return &amount
	}
	if plan.MonthlyUSDCents == nil {
		return nil
	}
	amount = max(0, *plan.MonthlyUSDCents) * 1000
	return &amount
}

func (credits includedAICredits) remaining() int64 {
	return max(0, credits.AllowanceMicros-credits.UsedMicros)
}

func (d *database) includedAICredits(ctx context.Context, query nativeQueryer, organization tracker.OrganizationID, at time.Time) (includedAICredits, error) {
	var result includedAICredits
	if d.plans() == nil {
		return result, nil
	}
	var raw string
	err := query.QueryRowContext(ctx, "SELECT state_json FROM hosted_billing_accounts WHERE organization_id=?", organization).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	var state hostedBillingState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return result, err
	}
	if !state.AccessUntil.After(at) || state.Snapshot.PeriodStart.IsZero() || state.Snapshot.PeriodStart.After(at) || !state.Snapshot.PeriodEnd.After(at) {
		return result, nil
	}
	plan, err := d.readHostedPlan(ctx, query, state.Plan)
	if err != nil {
		return result, err
	}
	amount := plan.includedAICreditMicros()
	if amount == nil || *amount == 0 {
		return result, nil
	}
	window := chatBillingWindow(at, state.Snapshot)
	result = includedAICredits{AllowanceMicros: *amount, PeriodStart: window.From, PeriodEnd: window.To, PlanName: plan.Name}
	err = query.QueryRowContext(ctx, `SELECT allowance_micros,used_micros FROM ai_credit_periods WHERE organization_id=? AND mode=? AND period_start=? AND period_end=?`, organization, d.aiCreditMode, window.From.UnixMicro(), window.To.UnixMicro()).Scan(&result.AllowanceMicros, &result.UsedMicros)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return result, err
}

func (d *database) spendIncludedAICredits(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, at time.Time, amount int64) (int64, error) {
	credits, err := d.includedAICredits(ctx, tx, organization, at)
	if err != nil || credits.AllowanceMicros == 0 {
		return 0, err
	}
	used := min(amount, credits.remaining())
	_, err = tx.ExecContext(ctx, `INSERT INTO ai_credit_periods(organization_id,mode,period_start,period_end,allowance_micros,used_micros) VALUES(?,?,?,?,?,?)
 ON CONFLICT(organization_id,mode,period_start,period_end) DO UPDATE SET used_micros=used_micros+excluded.used_micros`, organization, d.aiCreditMode, credits.PeriodStart.UnixMicro(), credits.PeriodEnd.UnixMicro(), credits.AllowanceMicros, used)
	return used, err
}
