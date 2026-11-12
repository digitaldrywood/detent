package credits

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Included struct {
	AllowanceMicros int64     `json:"allowance_micros"`
	UsedMicros      int64     `json:"used_micros"`
	PeriodStart     time.Time `json:"period_start"`
	PeriodEnd       time.Time `json:"period_end"`
	PlanName        string    `json:"plan_name"`
}

func (credits Included) Remaining() int64 {
	return max(0, credits.AllowanceMicros-credits.UsedMicros)
}

func (s *Service) Included(ctx context.Context, organization tracker.OrganizationID, at time.Time) (Included, error) {
	var result Included
	err := s.deps.Repository.ReadTenant(ctx, organization, corestore.Serving, func(reader tenantstore.Reader) error {
		var err error
		result, err = s.IncludedFromReader(ctx, reader, at)
		return err
	})
	return result, err
}

func (s *Service) IncludedFromReader(ctx context.Context, reader billingstore.Reader, at time.Time) (Included, error) {
	var result Included
	account, err := reader.BillingAccount(ctx)
	if errors.Is(err, corestore.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	state := account
	if !state.AccessUntil.After(at) || state.Snapshot.PeriodStart.IsZero() || state.Snapshot.PeriodStart.After(at) || !state.Snapshot.PeriodEnd.After(at) {
		return result, nil
	}
	plan, err := s.deps.Plan(ctx, reader, state.Plan)
	if err != nil {
		return result, err
	}
	amount := plan.AllowanceMicros
	if amount == nil || *amount == 0 {
		return result, nil
	}
	window := BillingWindow(at, state.Snapshot)
	result = Included{AllowanceMicros: *amount, PeriodStart: window.From, PeriodEnd: window.To, PlanName: plan.Name}
	period, err := reader.BillingCreditPeriod(ctx, s.deps.Mode, window.From, window.To)
	if errors.Is(err, corestore.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.UsedMicros = period.UsedMicros
	return result, nil
}
