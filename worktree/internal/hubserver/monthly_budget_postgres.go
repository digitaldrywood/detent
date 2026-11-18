package hubserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func checkMonthlyLeasePG(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, lease string, now time.Time) error {
	input, err := readBillingMonthlyBudgetInput(ctx, reader, hubscope.NativeScope{Project: project}, now)
	if err != nil {
		return err
	}
	if !input.OrganizationPolicy.Policy.Enabled && !input.ProjectPolicy.Policy.Enabled {
		return nil
	}
	exposure, err := reader.BillingMonthlyLease(ctx, tracker.LeaseID(lease))
	if err != nil {
		if errors.Is(err, corestore.ErrNotFound) {
			return fmt.Errorf("read monthly lease exposure: %w", errors.Join(ErrBillingBudgetUnavailable, &hubapi.NativeError{Code: "storage_owner_unavailable", Message: "Monthly budget lease exposure is unavailable", Status: http.StatusServiceUnavailable, PublicMessage: true}))
		}
		return fmt.Errorf("read monthly lease exposure: %w", err)
	}
	input.Exposure = budget.CostExposure{LunaAPI: exposure.LunaAPI, RunnerAPI: exposure.RunnerAPI, SpriteInfrastructure: exposure.SpriteInfrastructure}
	input.Admitted = true
	decision, err := billingstore.EvaluateMonthlyBudget(input)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return fmt.Errorf("%w: %s", tracker.ErrLeaseConflict, monthlyBudgetRefusal(decision))
	}
	return nil
}

func monthlyCostExposure(machine leasestore.Machine, workspace, runnerAPI bool) budget.CostExposure {
	return budget.CostExposure{RunnerAPI: !workspace && runnerAPI, SpriteInfrastructure: machine.SpriteName != "" && machine.SpriteName == machine.Hostname}
}

func monthlySpriteAllowedPG(ctx context.Context, repository tenantstore.Repository, scope hubscope.NativeScope, now time.Time) (bool, error) {
	var allowed bool
	err := repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		input, err := readBillingMonthlyBudgetInput(ctx, r, scope, now)
		if err != nil {
			return err
		}
		input.Exposure = budget.CostExposure{SpriteInfrastructure: true}
		decision, err := billingstore.EvaluateMonthlyBudget(input)
		if err != nil || decision.Allowed {
			allowed = decision.Allowed
			return err
		}
		input.Admitted = true
		decision, err = billingstore.EvaluateMonthlyBudget(input)
		if err != nil || !decision.Allowed {
			return err
		}
		allowed, err = r.BillingMonthlyAdmittedCandidate(ctx, scope.Project, now)
		return err
	})
	return allowed, err
}
