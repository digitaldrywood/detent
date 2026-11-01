package hubserver

import (
	"context"
	"database/sql"
	"errors"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func readRunnerIdentityFacts(ctx context.Context, q nativeQueryer, r *runnerauth.Runner, reports []providercapacity.Report) error {
	rows, err := q.QueryContext(ctx, `SELECT g.project_id FROM token_grants g JOIN runner_identities r ON r.token_id=g.token_id WHERE r.id=? ORDER BY g.project_id`, r.RunnerID)
	if err != nil {
		return err
	}
	defer rows.Close()
	r.ProjectIDs = []tracker.ProjectID{}
	for rows.Next() {
		var project tracker.ProjectID
		if err := rows.Scan(&project); err != nil {
			rows.Close()
			return err
		}
		r.ProjectIDs = append(r.ProjectIDs, project)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	rows, err = q.QueryContext(ctx, `SELECT project_id,capacity_limit FROM runner_project_capacity_limits WHERE runner_id=? AND organization_id=?`, r.RunnerID, r.OrganizationID)
	if err != nil {
		return err
	}
	defer rows.Close()
	r.ProjectCapacityLimits = make(map[tracker.ProjectID]int)
	for rows.Next() {
		var project tracker.ProjectID
		var limit int
		if err := rows.Scan(&project, &limit); err != nil {
			rows.Close()
			return err
		}
		r.ProjectCapacityLimits[project] = limit
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	billing, err := readProviderBilling(ctx, q, r.RunnerID)
	if err != nil {
		return err
	}
	windows, err := readProviderUsage(ctx, q, r.RunnerID)
	if err != nil {
		return err
	}
	for i := range reports {
		reports[i].Billing = (providercapacity.Report{Billing: billing[reports[i].Backend]}).BillingBasis()
		reports[i].UsageWindows = append(reports[i].UsageWindows, windows[reports[i].Backend]...)
	}

	r.ProjectConfigurationCommand, err = readConfigurationCommandFacts(ctx, q, r.OrganizationID, r.RunnerID)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	if err != nil {
		return err
	}
	r.Routing = r.Normalized()
	return nil
}
