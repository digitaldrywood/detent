package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/google/uuid"
)

type PlanReference struct {
	ID      string `json:"id" yaml:"id"`
	Version int64  `json:"version" yaml:"version"`
}

type HostedPlan struct {
	PlanReference   `yaml:",inline"`
	Name            string           `json:"name,omitempty" yaml:"name,omitempty"`
	MonthlyUSDCents *int64           `json:"monthly_usd_cents,omitempty" yaml:"monthly_usd_cents,omitempty"`
	Features        []string         `json:"features" yaml:"features"`
	Allowances      map[string]int64 `json:"allowances" yaml:"allowances"`
}

type HostedPlansConfig struct {
	Plans             []HostedPlan  `yaml:"plans" json:"plans"`
	Base              PlanReference `yaml:"base" json:"base"`
	WindowSeconds     int64         `yaml:"window_seconds" json:"window_seconds"`
	RetentionWindows  int64         `yaml:"retention_windows" json:"retention_windows"`
	ConnectedSeconds  int64         `yaml:"connected_seconds" json:"connected_seconds"`
	InvitationSeconds int64         `yaml:"invitation_seconds" json:"invitation_seconds"`

	// written records that the YAML section named at least one key, so an
	// explicitly empty plan list is an error rather than an absent catalog.
	written bool
}

// UnmarshalYAML decodes the catalog and remembers whether the section had
// any keys: `entitlements: {}` means no catalog, while a section
// that names keys is the operator's own catalog and must validate.
func (c *HostedPlansConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain HostedPlansConfig
	var decoded plain
	// node.Decode does not inherit the caller's KnownFields, so the section is
	// decoded again strictly: a misspelled key must fail, not zero a value.
	resolved, err := resolvedYAMLNode(node)
	if err != nil {
		return err
	}
	raw, err := yaml.Marshal(resolved)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decoded); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	*c = HostedPlansConfig(decoded)
	c.written = node.Kind == yaml.MappingNode && len(node.Content) > 0
	return nil
}

// resolvedYAMLNode copies node with every alias replaced by the value it
// names, so the node can be encoded on its own even when an anchor it uses is
// defined elsewhere in the document. An alias that leads back into a value
// still being resolved is a cycle and an error, never unbounded recursion.
func resolvedYAMLNode(node *yaml.Node) (*yaml.Node, error) {
	return resolveYAMLNode(node, make(map[*yaml.Node]bool))
}

func resolveYAMLNode(node *yaml.Node, resolving map[*yaml.Node]bool) (*yaml.Node, error) {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return resolveYAMLNode(node.Alias, resolving)
	}
	if resolving[node] {
		return nil, fmt.Errorf("hosted plan configuration line %d: an alias refers to a value that contains it", node.Line)
	}
	resolving[node] = true
	defer delete(resolving, node)
	copied := *node
	copied.Anchor = ""
	copied.Content = make([]*yaml.Node, len(node.Content))
	for i, child := range node.Content {
		resolved, err := resolveYAMLNode(child, resolving)
		if err != nil {
			return nil, err
		}
		copied.Content[i] = resolved
	}
	return &copied, nil
}

type HostedGrant struct {
	ID        string        `json:"id"`
	Plan      PlanReference `json:"plan"`
	Scope     []string      `json:"scope"`
	StartsAt  time.Time     `json:"starts_at"`
	ExpiresAt *time.Time    `json:"expires_at,omitempty"`
	RevokedAt *time.Time    `json:"revoked_at,omitempty"`
}

type HostedEntitlement struct {
	Name            string           `json:"name"`
	MonthlyUSDCents *int64           `json:"monthly_usd_cents"`
	OrganizationID  string           `json:"organization_id"`
	Base            PlanReference    `json:"base"`
	EffectiveBase   PlanReference    `json:"effective_base"`
	Source          string           `json:"source"`
	Revision        int64            `json:"revision"`
	Features        []string         `json:"features"`
	Allowances      map[string]int64 `json:"allowances"`
	Grants          []HostedGrant    `json:"grants"`
	Usage           map[string]int64 `json:"usage"`
	WindowEndsAt    time.Time        `json:"window_ends_at"`
}

func hostedAllowanceNames() []string {
	return []string{"unarchived_issues", "members", "projects", "repositories", "registered_runners", "connected_runners", "concurrent_work", "api_mutations", "ingested_events", "collaboration_bytes", "history_records", "artifact_retained_bytes", "artifact_reserved_bytes", "artifact_bytes", "artifact_upload_bytes", "artifact_retention_seconds", "relay_bytes"}
}

func hostedFeatureNames() []string {
	return []string{"collaboration", "native_execution", "github_integration", "hosted_artifacts", "model_choice"}
}

func pilotHostedPlans() HostedPlansConfig {
	return HostedPlansConfig{
		Base: PlanReference{ID: "pilot_free", Version: 1}, WindowSeconds: 3600, RetentionWindows: 24, ConnectedSeconds: 90, InvitationSeconds: 86400,
		Plans: []HostedPlan{{PlanReference: PlanReference{ID: "pilot_free", Version: 1}, Features: []string{"collaboration", "native_execution", "github_integration"}, Allowances: map[string]int64{
			"members": 10, "projects": 10, "repositories": 10, "registered_runners": 10, "connected_runners": 10, "concurrent_work": 5,
			"api_mutations": 10000, "ingested_events": 10000, "collaboration_bytes": 64 << 20, "history_records": 10000,
		}}, {PlanReference: PlanReference{ID: "comp_team", Version: 1}, Features: []string{"collaboration", "native_execution", "github_integration", "hosted_artifacts"}, Allowances: map[string]int64{
			"members": 20, "projects": 20, "repositories": 20, "registered_runners": 20, "connected_runners": 20, "concurrent_work": 10,
			"api_mutations": 20000, "ingested_events": 20000, "collaboration_bytes": 128 << 20, "history_records": 20000,
		}}},
	}
}

type hostedTier struct {
	PlanReference
	name       string
	cents      *int64
	concurrent int64
	offered    bool
}

const hostedOpenIssueGuard = 10000

func hostedTiers() []hostedTier {
	price := func(cents int64) *int64 { return &cents }
	return []hostedTier{
		{PlanReference{ID: "free", Version: 1}, "Free", price(0), 2, true},
		{PlanReference{ID: "starter", Version: 1}, "Starter", price(4900), 8, true},
		{PlanReference{ID: "team", Version: 2}, "Team", price(9900), 20, true},
		{PlanReference{ID: "growth", Version: 2}, "Growth", price(24900), 50, true},
		{PlanReference{ID: "scale", Version: 2}, "Scale", nil, 100, true},
		{PlanReference{ID: "growth", Version: 1}, "Growth", price(14900), 50, false},
		{PlanReference{ID: "scale", Version: 1}, "Scale", price(39900), 100, false},
	}
}

func hostedTierFor(ref PlanReference) (hostedTier, bool) {
	for _, tier := range hostedTiers() {
		if tier.PlanReference == ref {
			return tier, true
		}
	}
	return hostedTier{}, false
}

func (tier hostedTier) apply(plan HostedPlan) HostedPlan {
	if plan.Allowances == nil {
		plan.Allowances = make(map[string]int64)
	}
	plan.Allowances["unarchived_issues"], plan.Allowances["concurrent_work"] = hostedOpenIssueGuard, tier.concurrent
	for _, name := range []string{"members", "projects", "repositories", "registered_runners", "connected_runners"} {
		delete(plan.Allowances, name)
	}
	features := []string{"native_execution"}
	if tier.ID != "free" {
		features = append(features, "hosted_artifacts")
	}
	for _, feature := range features {
		if !slices.Contains(plan.Features, feature) {
			plan.Features = append(plan.Features, feature)
		}
	}
	slices.Sort(plan.Features)
	return plan
}

func capacityHostedPlans() HostedPlansConfig {
	config := pilotHostedPlans()
	legacy := config.Plans
	config.Base = hostedTiers()[0].PlanReference
	config.Plans = nil
	for _, tier := range hostedTiers() {
		allowances := maps.Clone(legacy[1].Allowances)
		allowances["collaboration_bytes"], allowances["history_records"] = 1<<30, 2000000
		plan := tier.apply(HostedPlan{PlanReference: tier.PlanReference, Name: tier.name, MonthlyUSDCents: tier.cents, Features: []string{"collaboration", "github_integration"}, Allowances: allowances})
		config.Plans = append(config.Plans, plan)
	}
	config.Plans = append(config.Plans, legacy...)
	return config
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

func CheckoutHostedPlans(plans *HostedPlansConfig) *HostedPlansConfig {
	config := capacityHostedPlans()
	if !plans.IsZero() {
		config = *plans
	}
	config.Plans = slices.Clone(config.Plans)
	config.Base = PlanReference{ID: "payment_pending", Version: 1}
	allowances := make(map[string]int64)
	for _, name := range hostedAllowanceNames() {
		allowances[name] = 0
	}
	free := capacityHostedPlans().Plans[0]
	for _, name := range []string{"api_mutations", "history_records", "collaboration_bytes"} {
		allowances[name] = free.Allowances[name]
	}
	config.Plans = append(config.Plans, HostedPlan{PlanReference: config.Base, Name: "Payment pending", Features: []string{}, Allowances: allowances})
	return &config
}

func hostedCapacityPlan(ref PlanReference) bool {
	_, ok := hostedTierFor(ref)
	return ok
}

func (c *HostedPlansConfig) IsZero() bool {
	return c == nil || !c.written && len(c.Plans) == 0 && c.Base == PlanReference{} && c.WindowSeconds == 0 && c.RetentionWindows == 0 && c.ConnectedSeconds == 0 && c.InvitationSeconds == 0
}

func (c HostedPlansConfig) validate() error {
	if c.InvitationSeconds < 60 || c.InvitationSeconds > 7*86400 || len(c.Plans) == 0 || len(c.Plans) > 100 || c.WindowSeconds < 60 || c.WindowSeconds > 86400 || c.WindowSeconds%60 != 0 || c.RetentionWindows*c.WindowSeconds > 30*86400 || c.RetentionWindows < 1 || c.RetentionWindows > 720 || c.ConnectedSeconds < 30 || c.ConnectedSeconds > 3600 {
		return errors.New("hosted pilot plan configuration is invalid")
	}
	seen := make(map[PlanReference]bool)
	for _, p := range c.Plans {
		if !hostedSafeID(p.ID) || p.Version < 1 || seen[p.PlanReference] {
			return errors.New("hosted plan identity is invalid or duplicated")
		}
		seen[p.PlanReference] = true
		if len(p.Name) > 80 || p.MonthlyUSDCents != nil && (*p.MonthlyUSDCents < 0 || *p.MonthlyUSDCents > 100000000) {
			return errors.New("hosted plan display name or monthly price is invalid")
		}
		for _, feature := range p.Features {
			if !slices.Contains(hostedFeatureNames(), feature) {
				return errors.New("hosted plan feature is unknown")
			}
		}
		for name, limit := range p.Allowances {
			if !slices.Contains(hostedAllowanceNames(), name) || limit < 0 || limit > 1<<50 {
				return errors.New("hosted plan allowance is invalid")
			}
		}
	}
	if !seen[c.Base] {
		return errors.New("hosted base plan must reference a configured version")
	}
	return nil
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
	if err := config.validate(); err != nil {
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

func normalizeHostedPlan(plan HostedPlan) HostedPlan {
	plan.Allowances = maps.Clone(plan.Allowances)
	if plan.Allowances == nil {
		plan.Allowances = make(map[string]int64)
	}
	for _, name := range hostedAllowanceNames() {
		if name == "unarchived_issues" || name == "concurrent_work" || hostedCapacityPlan(plan.PlanReference) && slices.Contains([]string{"members", "projects", "repositories", "registered_runners", "connected_runners"}, name) {
			continue
		}
		if _, exists := plan.Allowances[name]; !exists {
			plan.Allowances[name] = 0
		}
	}
	plan.Features = slices.Clone(plan.Features)
	slices.Sort(plan.Features)
	plan.Features = slices.Compact(plan.Features)
	return plan
}

func validateHostedPlan(plan HostedPlan) error {
	config := pilotHostedPlans()
	config.Plans, config.Base = []HostedPlan{plan}, plan.PlanReference
	return config.validate()
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
		if _, err := tx.ExecContext(ctx, "INSERT INTO hosted_plans(id,version,record_json,runtime_id) VALUES(?,?,?,?)", plan.ID, plan.Version, string(encoded), runtimeID); err != nil {
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
			return tier.apply(plan), nil
		}
		if ref.Version == 1 && slices.Contains([]string{"pilot_free", "comp_team"}, ref.ID) {
			var capacityRuntime string
			err := query.QueryRowContext(ctx, "SELECT runtime_id FROM hosted_plans WHERE id='starter' AND version=1").Scan(&capacityRuntime)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return plan, err
			}
			if err == nil && d.runtimeVisible(capacityRuntime) {
				plan.Allowances["unarchived_issues"] = max(plan.Allowances["unarchived_issues"], hostedOpenIssueGuard)
				plan.Allowances["collaboration_bytes"] = max(plan.Allowances["collaboration_bytes"], 1<<30)
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
