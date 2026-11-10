package hubserver

import (
	"context"
	"fmt"
	"time"

	"github.com/digitaldrywood/detent/internal/budget"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func checkMonthlyLeasePG(ctx context.Context, reader tenantstore.Reader, project tracker.ProjectID, lease string, now time.Time) error {
	input, err := readBillingMonthlyBudgetInput(ctx, reader, nativeScope{project: project}, now)
	if err != nil {
		return err
	}
	if !input.OrganizationPolicy.Policy.Enabled && !input.ProjectPolicy.Policy.Enabled {
		return nil
	}
	exposure, err := reader.BillingMonthlyLease(ctx, tracker.LeaseID(lease))
	if err != nil {
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
