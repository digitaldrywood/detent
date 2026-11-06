package postgres

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (s *session) BillingPlan(ctx context.Context, ref billingstore.PlanReference) (billingstore.Plan, error) {
	row, err := s.queries.BillingPlan(ctx, pgquery.BillingPlanParams{OrganizationID: s.organization, ID: ref.ID, Version: ref.Version})
	if err != nil {
		return billingstore.Plan{}, mapError(ctx, err)
	}
	return s.billingPlan(ctx, row)
}

func (s *session) billingPlan(ctx context.Context, row pgquery.HostedPlan) (billingstore.Plan, error) {
	plan := billingstore.Plan{OrganizationID: row.OrganizationID, PlanReference: billingstore.PlanReference{ID: row.ID, Version: row.Version}, Name: row.Name, RuntimeID: row.RuntimeID, Allowances: map[string]int64{}}
	if row.MonthlyUsdCents.Valid {
		plan.MonthlyUSDCents = new(row.MonthlyUsdCents.Int64)
	}
	allowances, err := s.queries.BillingPlanAllowances(ctx, pgquery.BillingPlanAllowancesParams{OrganizationID: s.organization, PlanID: row.ID, PlanVersion: row.Version})
	if err != nil {
		return plan, mapError(ctx, err)
	}
	for _, allowance := range allowances {
		plan.Allowances[allowance.Metric] = allowance.Amount
	}
	plan.Features, err = s.queries.BillingPlanFeatures(ctx, pgquery.BillingPlanFeaturesParams{OrganizationID: s.organization, PlanID: row.ID, PlanVersion: row.Version})
	return plan, mapError(ctx, err)
}

func (s *session) BillingPlans(ctx context.Context, after billingstore.PlanReference, limit int) ([]billingstore.Plan, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.BillingPlans(ctx, pgquery.BillingPlansParams{OrganizationID: s.organization, AfterID: after.ID, AfterVersion: after.Version, PageSize: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	plans := make([]billingstore.Plan, 0, len(rows))
	for _, row := range rows {
		plan, err := s.billingPlan(ctx, row)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func (s *session) CreateBillingPlan(ctx context.Context, plan billingstore.Plan) error {
	if err := s.checkOrganization(plan.OrganizationID); err != nil {
		return err
	}
	var price pgtype.Int8
	if plan.MonthlyUSDCents != nil {
		price = pgtype.Int8{Int64: *plan.MonthlyUSDCents, Valid: true}
	}
	if err := s.queries.CreateBillingPlan(ctx, pgquery.CreateBillingPlanParams{OrganizationID: s.organization, ID: plan.ID, Version: plan.Version, Name: plan.Name, MonthlyUsdCents: price, RuntimeID: plan.RuntimeID}); err != nil {
		return mapError(ctx, err)
	}
	return s.createBillingPlanContents(ctx, plan)
}

func (s *session) createBillingPlanContents(ctx context.Context, plan billingstore.Plan) error {
	for _, metric := range slices.Sorted(maps.Keys(plan.Allowances)) {
		if err := s.queries.CreateBillingPlanAllowance(ctx, pgquery.CreateBillingPlanAllowanceParams{OrganizationID: s.organization, PlanID: plan.ID, PlanVersion: plan.Version, Metric: metric, Amount: plan.Allowances[metric]}); err != nil {
			return mapError(ctx, err)
		}
	}
	for _, feature := range plan.Features {
		if err := s.queries.CreateBillingPlanFeature(ctx, pgquery.CreateBillingPlanFeatureParams{OrganizationID: s.organization, PlanID: plan.ID, PlanVersion: plan.Version, Feature: feature}); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) ReplaceBillingPlan(ctx context.Context, plan billingstore.Plan) error {
	if err := s.checkOrganization(plan.OrganizationID); err != nil {
		return err
	}
	result, err := s.db.Exec(ctx, `UPDATE hosted_plans SET name=$4,monthly_usd_cents=$5,runtime_id=$6 WHERE organization_id=$1 AND id=$2 AND version=$3`, s.organization, plan.ID, plan.Version, plan.Name, plan.MonthlyUSDCents, plan.RuntimeID)
	if err != nil {
		return mapError(ctx, err)
	}
	if err := affected(ctx, result.RowsAffected(), nil); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM hosted_plan_allowances WHERE organization_id=$1 AND plan_id=$2 AND plan_version=$3`, s.organization, plan.ID, plan.Version); err != nil {
		return mapError(ctx, err)
	}
	if _, err := s.db.Exec(ctx, `DELETE FROM hosted_plan_features WHERE organization_id=$1 AND plan_id=$2 AND plan_version=$3`, s.organization, plan.ID, plan.Version); err != nil {
		return mapError(ctx, err)
	}
	return s.createBillingPlanContents(ctx, plan)
}

func (s *session) SetBillingPlanRuntime(ctx context.Context, ref billingstore.PlanReference, runtimeID string) error {
	_, err := s.db.Exec(ctx, `UPDATE hosted_plans SET runtime_id=$1 WHERE organization_id=$2 AND id=$3 AND version=$4`, runtimeID, s.organization, ref.ID, ref.Version)
	return mapError(ctx, err)
}

func (s *session) BillingAssignment(ctx context.Context) (billingstore.Assignment, error) {
	row, err := s.queries.BillingAssignment(ctx, s.organization)
	result := billingstore.Assignment{OrganizationID: row.OrganizationID, Base: billingstore.PlanReference{ID: row.BaseID, Version: row.BaseVersion}, ExpiresAt: row.SubscriptionExpiresAt, Revision: row.Revision}
	if row.SubscriptionID != nil && row.SubscriptionVersion.Valid {
		result.Subscription = &billingstore.PlanReference{ID: *row.SubscriptionID, Version: row.SubscriptionVersion.Int64}
	}
	return result, mapError(ctx, err)
}

func (s *session) SetBillingAssignment(ctx context.Context, row billingstore.Assignment, expected int64) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	var subscription *string
	var version pgtype.Int8
	if row.Subscription != nil {
		subscription = &row.Subscription.ID
		version = pgtype.Int8{Int64: row.Subscription.Version, Valid: true}
	}
	if expected == 0 {
		return mapError(ctx, s.queries.CreateBillingAssignment(ctx, pgquery.CreateBillingAssignmentParams{OrganizationID: s.organization, BaseID: row.Base.ID, BaseVersion: row.Base.Version, SubscriptionID: subscription, SubscriptionVersion: version, SubscriptionExpiresAt: row.ExpiresAt}))
	}
	n, err := s.queries.SetBillingAssignment(ctx, pgquery.SetBillingAssignmentParams{OrganizationID: s.organization, BaseID: row.Base.ID, BaseVersion: row.Base.Version, SubscriptionID: subscription, SubscriptionVersion: version, SubscriptionExpiresAt: row.ExpiresAt, ExpectedRevision: expected})
	if err == nil && n == 0 {
		return corestore.ErrRevisionConflict
	}
	return mapError(ctx, err)
}

func (s *session) BillingGrants(ctx context.Context, after string, limit int) ([]billingstore.Grant, error) {
	if err := checkPage(limit); err != nil {
		return nil, err
	}
	rows, err := s.queries.BillingGrants(ctx, pgquery.BillingGrantsParams{OrganizationID: s.organization, AfterID: after, PageSize: int32(limit)})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]billingstore.Grant, 0, len(rows))
	for _, row := range rows {
		scopes, err := s.queries.BillingGrantScopes(ctx, pgquery.BillingGrantScopesParams{OrganizationID: s.organization, GrantID: row.ID})
		if err != nil {
			return nil, mapError(ctx, err)
		}
		result = append(result, billingstore.Grant{OrganizationID: row.OrganizationID, ID: row.ID, Plan: billingstore.PlanReference{ID: row.PlanID, Version: row.PlanVersion}, Scope: scopes, StartsAt: row.StartsAt, ExpiresAt: row.ExpiresAt, RevokedAt: row.RevokedAt})
	}
	return result, nil
}

func (s *session) BillingGrant(ctx context.Context, id string) (billingstore.Grant, error) {
	row, err := s.queries.BillingGrant(ctx, pgquery.BillingGrantParams{OrganizationID: s.organization, ID: id})
	if err != nil {
		return billingstore.Grant{}, mapError(ctx, err)
	}
	scopes, err := s.queries.BillingGrantScopes(ctx, pgquery.BillingGrantScopesParams{OrganizationID: s.organization, GrantID: row.ID})
	if err != nil {
		return billingstore.Grant{}, mapError(ctx, err)
	}
	return billingstore.Grant{OrganizationID: row.OrganizationID, ID: row.ID, Plan: billingstore.PlanReference{ID: row.PlanID, Version: row.PlanVersion}, Scope: scopes, StartsAt: row.StartsAt, ExpiresAt: row.ExpiresAt, RevokedAt: row.RevokedAt}, nil
}

func (s *session) BillingEffectiveGrants(ctx context.Context, now time.Time) ([]billingstore.Grant, error) {
	rows, err := s.queries.BillingEffectiveGrants(ctx, pgquery.BillingEffectiveGrantsParams{OrganizationID: s.organization, StartsAt: now.UTC()})
	if err != nil {
		return nil, mapError(ctx, err)
	}
	result := make([]billingstore.Grant, 0, len(rows))
	for _, row := range rows {
		if len(result) == 0 || result[len(result)-1].ID != row.ID {
			result = append(result, billingstore.Grant{OrganizationID: row.OrganizationID, ID: row.ID, Plan: billingstore.PlanReference{ID: row.PlanID, Version: row.PlanVersion}, StartsAt: row.StartsAt})
			grant := &result[len(result)-1]
			grant.ExpiresAt = row.ExpiresAt
			grant.RevokedAt = row.RevokedAt
		}
		if row.Scope != nil {
			grant := &result[len(result)-1]
			grant.Scope = append(grant.Scope, *row.Scope)
		}
	}
	return result, nil
}

func (s *session) BillingFeatureEnabled(ctx context.Context, feature string, now time.Time, capacityPlanRefs []string) (bool, error) {
	asOf := now.UTC()
	enabled, err := s.queries.BillingFeatureEnabled(ctx, pgquery.BillingFeatureEnabledParams{OrganizationID: s.organization, Feature: feature, SubscriptionExpiresAt: &asOf, CapacityPlanRefs: capacityPlanRefs})
	return enabled, mapError(ctx, err)
}

func (s *session) CreateBillingGrant(ctx context.Context, row billingstore.Grant) error {
	if err := s.checkOrganization(row.OrganizationID); err != nil {
		return err
	}
	if err := s.queries.CreateBillingGrant(ctx, pgquery.CreateBillingGrantParams{OrganizationID: s.organization, ID: row.ID, PlanID: row.Plan.ID, PlanVersion: row.Plan.Version, StartsAt: row.StartsAt, ExpiresAt: row.ExpiresAt, RevokedAt: row.RevokedAt}); err != nil {
		return mapError(ctx, err)
	}
	for _, scope := range row.Scope {
		if err := s.queries.CreateBillingGrantScope(ctx, pgquery.CreateBillingGrantScopeParams{OrganizationID: s.organization, GrantID: row.ID, Scope: scope}); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (s *session) RevokeBillingGrant(ctx context.Context, id string, at time.Time) error {
	n, err := s.queries.RevokeBillingGrant(ctx, pgquery.RevokeBillingGrantParams{OrganizationID: s.organization, ID: id, RevokedAt: &at})
	return affected(ctx, n, err)
}

func (s *session) MissingConfiguredBillingPlan(ctx context.Context, configured []billingstore.PlanReference, now time.Time) (billingstore.PlanReference, error) {
	refs := make([]string, 0, len(configured))
	for _, ref := range configured {
		refs = append(refs, fmt.Sprintf("%s:%d", ref.ID, ref.Version))
	}
	row, err := s.queries.MissingConfiguredBillingPlan(ctx, pgquery.MissingConfiguredBillingPlanParams{OrganizationID: s.organization, ConfiguredRefs: refs, AsOf: &now})
	return billingstore.PlanReference{ID: row.ID, Version: row.Version}, mapError(ctx, err)
}
