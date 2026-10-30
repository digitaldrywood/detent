package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/digitaldrywood/detent/internal/hostedplans"
)

type HostedGrant struct {
	ID        string        `json:"id"`
	Plan      PlanReference `json:"plan"`
	Scope     []string      `json:"scope"`
	StartsAt  time.Time     `json:"starts_at"`
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`
	RevokedAt *time.Time    `json:"revoked_at,omitempty"`
}

type HostedEntitlement struct {
	IncludedAICreditMicros *int64           `json:"included_ai_credit_micros"`
	Name                   string           `json:"name"`
	MonthlyUSDCents        *int64           `json:"monthly_usd_cents"`
	OrganizationID         string           `json:"organization_id"`
	Base                   PlanReference    `json:"base"`
	EffectiveBase          PlanReference    `json:"effective_base"`
	Source                 string           `json:"source"`
	Revision               int64            `json:"revision"`
	Features               []string         `json:"features"`
	Allowances             map[string]int64 `json:"allowances"`
	Grants                 []HostedGrant    `json:"grants"`
	Usage                  map[string]int64 `json:"usage"`
	WindowEndsAt           time.Time        `json:"window_ends_at"`
}

func defaultHostedPlans(cfg *HostedConfig) HostedPlansConfig {
	if cfg.PlanID == "" && cfg.StorageQuotaBytes == 0 && cfg.EventQuota == 0 {
		return HostedPlansConfig{}
	}
	config := pilotHostedPlans()
	if cfg.PlanID != "" {
		config.Plans[0].ID = cfg.PlanID
		config.Base.ID = cfg.PlanID
	}
	if cfg.StorageQuotaBytes > 0 {
		config.Plans[0].Allowances["collaboration_bytes"] = cfg.StorageQuotaBytes
	}
	if cfg.EventQuota > 0 {
		config.Plans[0].Allowances["ingested_events"] = cfg.EventQuota
	}
	return config
}

func (d *database) configureHostedPlans(ctx context.Context, cfg *HostedConfig) error {
	if cfg == nil {
		return nil
	}
	config := defaultHostedPlans(cfg)
	if !cfg.Plans.IsZero() {
		config = *cfg.Plans
	}
	if config.IsZero() {
		return nil
	}
	if err := config.Validate(); err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := d.seedHostedPlans(ctx, tx, config, "configuration", "startup seed", ""); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_plan_assignments(organization_id,base_id,base_version) VALUES(?,?,?) ON CONFLICT DO NOTHING`, d.hostedOrganization, config.Base.ID, config.Base.Version); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.hostedRuntime.Store(&hostedRuntimeConfig{Plans: config})
	return nil
}

func (d *database) seedHostedPlans(ctx context.Context, tx *sql.Tx, config HostedPlansConfig, actor, reason, runtimeID string) error {
	for _, plan := range config.Plans {
		plan = normalizeHostedPlan(plan)
		encoded, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		var existing, staged string
		err = tx.QueryRowContext(ctx, "SELECT record_json,runtime_id FROM hosted_plans WHERE id = ? AND version = ?", plan.ID, plan.Version).Scan(&existing, &staged)
		if err == nil {
			if existing != string(encoded) {
				d.logger.WarnContext(ctx, "hosted plan seed differs from immutable stored version; keeping stored version", "plan", plan.ID, "version", plan.Version, "actor", actor, "reason", reason)
			}
			if !d.runtimeVisible(staged) {
				if _, err := tx.ExecContext(ctx, "UPDATE hosted_plans SET runtime_id=? WHERE id=? AND version=?", runtimeID, plan.ID, plan.Version); err != nil {
					return err
				}
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO hosted_plans(id,version,record_json,runtime_id,collaboration_bytes) VALUES(?,?,?,?,?)", plan.ID, plan.Version, string(encoded), runtimeID, plan.Allowances["collaboration_bytes"]); err != nil {
			return err
		}
		command := hostedPlanCommand{ID: "seed_" + uuid.NewString(), Action: "create_plan", Plan: plan.PlanReference, Definition: &plan, Reason: reason}
		if err := d.auditHostedPlanCommand(ctx, tx, actor, command, d.now()); err != nil {
			return err
		}
	}
	return nil
}

func (d *database) readHostedPlans(ctx context.Context, query nativeQueryer) ([]HostedPlan, error) {
	rows, err := query.QueryContext(ctx, "SELECT id,version,runtime_id FROM hosted_plans ORDER BY id, version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var references []PlanReference
	for rows.Next() {
		var ref PlanReference
		var runtimeID string
		if err := rows.Scan(&ref.ID, &ref.Version, &runtimeID); err != nil {
			return nil, err
		}
		if d.runtimeVisible(runtimeID) {
			references = append(references, ref)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	plans := make([]HostedPlan, 0, len(references))
	for _, ref := range references {
		plan, err := d.readHostedPlan(ctx, query, ref)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

func (d *database) readHostedPlan(ctx context.Context, query nativeQueryer, ref PlanReference) (HostedPlan, error) {
	var raw, runtimeID string
	var plan HostedPlan
	if err := query.QueryRowContext(ctx, "SELECT record_json,runtime_id FROM hosted_plans WHERE id = ? AND version = ?", ref.ID, ref.Version).Scan(&raw, &runtimeID); err != nil {
		return plan, err
	}
	if !d.runtimeVisible(runtimeID) {
		return plan, sql.ErrNoRows
	}
	err := json.Unmarshal([]byte(raw), &plan)
	if err == nil {
		if plan.Allowances == nil {
			plan.Allowances = make(map[string]int64)
		}
		if _, exists := plan.Allowances["unarchived_issues"]; !exists {
			plan.Allowances["unarchived_issues"] = hostedOpenIssueGuard
		}
		if tier, ok := hostedTierFor(ref); ok {
			return tier.Apply(plan), nil
		}
		if ref.Version == 1 && slices.Contains([]string{"pilot_free", "comp_team"}, ref.ID) {
			var capacityRuntime string
			err := query.QueryRowContext(ctx, "SELECT runtime_id FROM hosted_plans WHERE id='starter' AND version=1").Scan(&capacityRuntime)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return plan, err
			}
			if err == nil && d.runtimeVisible(capacityRuntime) {
				plan.Allowances["unarchived_issues"] = max(plan.Allowances["unarchived_issues"], hostedOpenIssueGuard)
				plan.Allowances["history_records"] = max(plan.Allowances["history_records"], 2000000)
				for _, resource := range []string{"members", "projects", "repositories", "registered_runners", "connected_runners", "concurrent_work"} {
					delete(plan.Allowances, resource)
				}
				if plan.Name == "" {
					plan.Name = "Legacy complimentary pilot"
					if ref.ID == "comp_team" {
						plan.Name = "Legacy Team"
					}
				}
			}
		}
	}
	return plan, err
}

func (d *database) hostedEntitlement(ctx context.Context, query nativeQueryer, now time.Time) (HostedEntitlement, error) {
	runtimePlans := d.plans()
	result := HostedEntitlement{OrganizationID: string(d.hostedOrganization), Source: "base", Grants: []HostedGrant{}}
	if runtimePlans == nil {
		return result, errors.New("hosted entitlements are disabled")
	}
	var subscription, expiry sql.NullString
	var version sql.NullInt64
	if err := query.QueryRowContext(ctx, `SELECT base_id,base_version,subscription_id,subscription_version,subscription_expires_at,revision FROM hosted_plan_assignments WHERE organization_id = ?`, d.hostedOrganization).Scan(&result.Base.ID, &result.Base.Version, &subscription, &version, &expiry, &result.Revision); err != nil {
		return result, err
	}
	result.EffectiveBase = result.Base
	if subscription.Valid && expiry.Valid {
		until, err := parseTimeValue(expiry.String)
		if err != nil {
			return result, err
		}
		if until.After(now) {
			result.EffectiveBase = PlanReference{ID: subscription.String, Version: version.Int64}
			result.Source = "subscription"
		}
	}
	base, err := d.readHostedPlan(ctx, query, result.EffectiveBase)
	if err != nil {
		return result, err
	}
	result.Allowances, result.Features = base.Allowances, base.Features
	result.Name, result.MonthlyUSDCents = base.Name, base.MonthlyUSDCents
	result.IncludedAICreditMicros = new(int64)
	if result.Source == "subscription" {
		result.IncludedAICreditMicros = base.IncludedAICreditMicros()
	}
	if result.Name == "" {
		result.Name = fmt.Sprintf("%s · version %d", base.ID, base.Version)
	}
	rows, err := query.QueryContext(ctx, "SELECT record_json FROM hosted_complimentary_grants ORDER BY id")
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		var grant HostedGrant
		if err := rows.Scan(&raw); err != nil {
			return result, errors.Join(err, rows.Close())
		}
		if err := json.Unmarshal([]byte(raw), &grant); err != nil {
			return result, errors.Join(err, rows.Close())
		}
		result.Grants = append(result.Grants, grant)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return result, err
	}
	for _, grant := range result.Grants {
		if grant.RevokedAt != nil || grant.StartsAt.After(now) || grant.ExpiresAt != nil && !grant.ExpiresAt.After(now) {
			continue
		}
		plan, err := d.readHostedPlan(ctx, query, grant.Plan)
		if err != nil {
			return result, err
		}
		scopes := grant.Scope
		if hostedCapacityPlan(grant.Plan) {
			scopes = append(slices.Clone(scopes), "concurrent_work", "unarchived_issues", "native_execution")
		}
		for _, scope := range scopes {
			_, limited := result.Allowances[scope]
			if value, ok := plan.Allowances[scope]; ok && limited {
				result.Allowances[scope] = max(result.Allowances[scope], value)
			}
			if (scope == "model_choice" || slices.Contains(plan.Features, scope)) && !slices.Contains(result.Features, scope) {
				result.Features = append(result.Features, scope)
			}
		}
	}
	delete(result.Allowances, "members")
	slices.Sort(result.Features)
	result.WindowEndsAt = time.Unix((now.Unix()/runtimePlans.WindowSeconds+1)*runtimePlans.WindowSeconds, 0).UTC()
	return result, nil
}

type hostedLimitError struct {
	Resource    string `json:"resource"`
	Allowance   int64  `json:"allowance"`
	Consumption int64  `json:"consumption"`
}

func (e *hostedLimitError) Error() string {
	if e.Resource == "native_execution" {
		return "The current plan does not include native execution. Choose a plan or request complimentary execution access. Model-provider charges remain separate."
	}
	return fmt.Sprintf("Hosted %s allowance reached (%d of %d). Existing data, reading, export and billing remain available.", e.Resource, e.Consumption, e.Allowance)
}

func (d *database) requireHostedFeature(ctx context.Context, query nativeQueryer, feature string, now time.Time) error {
	if d.plans() == nil {
		return nil
	}
	entitlement, err := d.hostedEntitlement(ctx, query, now)
	if err != nil {
		return err
	}
	if !slices.Contains(entitlement.Features, feature) {
		return &hostedLimitError{Resource: feature}
	}
	return nil
}

type PlanReference = hostedplans.PlanReference
type HostedPlan = hostedplans.HostedPlan
type HostedPlansConfig = hostedplans.HostedPlansConfig

const hostedOpenIssueGuard = hostedplans.OpenIssueGuard

func hostedAllowanceNames() []string { return hostedplans.AllowanceNames() }

func hostedFeatureNames() []string { return hostedplans.FeatureNames() }

func pilotHostedPlans() HostedPlansConfig { return hostedplans.Pilot() }

func capacityHostedPlans() HostedPlansConfig { return hostedplans.Capacity() }

func CheckoutHostedPlans(c *HostedPlansConfig) *HostedPlansConfig { return hostedplans.Checkout(c) }

func hostedCapacityPlan(ref PlanReference) bool { return hostedplans.IsCapacity(ref) }

func normalizeHostedPlan(p HostedPlan) HostedPlan { return hostedplans.Normalize(p) }

func validateHostedPlan(p HostedPlan) error { return hostedplans.ValidatePlan(p) }

func hostedTierFor(ref PlanReference) (hostedplans.Tier, bool) { return hostedplans.TierFor(ref) }

func hostedTiers() []hostedplans.Tier { return hostedplans.Tiers() }
