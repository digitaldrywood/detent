package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
			"api_mutations": 1000000, "ingested_events": 1000000, "collaboration_bytes": 64 << 20, "history_records": 10000,
		}}, {PlanReference: PlanReference{ID: "comp_team", Version: 1}, Features: []string{"collaboration", "native_execution", "github_integration", "hosted_artifacts"}, Allowances: map[string]int64{
			"members": 20, "projects": 20, "repositories": 20, "registered_runners": 20, "connected_runners": 20, "concurrent_work": 10,
			"api_mutations": 1000000, "ingested_events": 1000000, "collaboration_bytes": 128 << 20, "history_records": 20000,
		}}},
	}
}

type hostedTier struct {
	PlanReference
	name       string
	cents      *int64
	concurrent int64
	storage    int64
	offered    bool
}

const hostedOpenIssueGuard = 10000

func hostedTiers() []hostedTier {
	price := func(cents int64) *int64 { return &cents }
	return []hostedTier{
		{PlanReference{ID: "free", Version: 1}, "Free", price(0), 2, 1 << 30, true},
		{PlanReference{ID: "starter", Version: 1}, "Starter", price(4900), 8, 25 << 30, true},
		{PlanReference{ID: "team", Version: 2}, "Team", price(9900), 20, 100 << 30, true},
		{PlanReference{ID: "growth", Version: 2}, "Growth", price(24900), 50, 250 << 30, true},
		{PlanReference{ID: "scale", Version: 2}, "Scale", nil, 100, 1 << 30, true},
		{PlanReference{ID: "growth", Version: 1}, "Growth", price(14900), 50, 1 << 30, false},
		{PlanReference{ID: "scale", Version: 1}, "Scale", price(39900), 100, 1 << 30, false},
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
		allowances["collaboration_bytes"], allowances["history_records"] = tier.storage, 2000000
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
	for _, name := range []string{"api_mutations", "ingested_events", "history_records", "collaboration_bytes"} {
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
	if err := d.refreshRuntime(ctx); err != nil {
		return err
	}
	config := defaultHostedPlans(cfg)
	if !cfg.Plans.IsZero() {
		config = *cfg.Plans
	}
	if config.IsZero() {
		if d.billingRepository == nil {
			return nil
		}
		return d.readBillingTenant(ctx, func(r tenantstore.Reader) error {
			return d.validateConfiguredPlanReferences(ctx, r, config)
		})
	}
	if err := config.validate(); err != nil {
		return err
	}

	var prices []HostedBillingPrice
	if cfg.Billing != nil {
		prices = cfg.Billing.Prices
	}
	if err := d.applyRuntimeRecords(ctx, config, prices, "configuration", "startup configuration", "", false); err != nil {
		return err
	}
	err := d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		assignment := billingstore.Assignment{OrganizationID: d.hostedOrganization, Base: billingstore.PlanReference{ID: config.Base.ID, Version: config.Base.Version}}
		if _, err := writer.BillingAssignment(ctx); errors.Is(err, corestore.ErrNotFound) {
			return writer.SetBillingAssignment(ctx, assignment, 0)
		} else {
			return err
		}
	})
	if err != nil {
		return err
	}
	return d.refreshRuntime(ctx)
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
	if len(plan.Features) == 0 {
		plan.Features = nil
	}
	return plan
}

func validateHostedPlan(plan HostedPlan) error {
	config := pilotHostedPlans()
	config.Plans, config.Base = []HostedPlan{plan}, plan.PlanReference
	return config.validate()
}

func (d *database) readHostedPlans(ctx context.Context, query nativeQueryer) ([]HostedPlan, error) {

	var plans []HostedPlan
	err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		after := billingstore.PlanReference{}
		for {
			records, err := reader.BillingPlans(ctx, after, corestore.MaxPageSize)
			if err != nil {
				return err
			}
			for _, record := range records {
				plan, err := d.readHostedPlanRecord(ctx, reader, PlanReference{ID: record.ID, Version: record.Version})
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return err
				}
				plans = append(plans, plan)
			}
			if len(records) < corestore.MaxPageSize {
				return nil
			}
			last := records[len(records)-1]
			after = billingstore.PlanReference{ID: last.ID, Version: last.Version}
		}
	})
	return plans, err
}

func (d *database) readHostedPlan(ctx context.Context, query nativeQueryer, ref PlanReference) (HostedPlan, error) {

	var plan HostedPlan
	err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		plan, err = d.readHostedPlanRecord(ctx, reader, ref)
		return err
	})
	return plan, err
}

func (d *database) loadHostedPlan(ctx context.Context, ref PlanReference, read func(context.Context, PlanReference) (HostedPlan, string, error)) (HostedPlan, error) {
	plan, _, err := read(ctx, ref)
	if err != nil {
		return plan, err
	}
	if plan.Allowances == nil {
		plan.Allowances = make(map[string]int64)
	}
	if _, exists := plan.Allowances["unarchived_issues"]; !exists {
		plan.Allowances["unarchived_issues"] = hostedOpenIssueGuard
	}
	return plan, err
}

func (d *database) hostedEntitlement(ctx context.Context, query nativeQueryer, now time.Time) (HostedEntitlement, error) {

	var entitlement HostedEntitlement
	err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		entitlement, err = d.hostedEntitlementRecord(ctx, reader, now)
		return err
	})
	return entitlement, err
}

func (d *database) resolveHostedEntitlement(ctx context.Context, now time.Time, result HostedEntitlement, readPlan func(context.Context, PlanReference) (HostedPlan, error)) (HostedEntitlement, error) {
	runtimePlans := d.plans()
	base, err := readPlan(ctx, result.EffectiveBase)
	if err != nil {
		return result, err
	}
	result.Allowances, result.Features = base.Allowances, base.Features
	result.Name, result.MonthlyUSDCents = base.Name, base.MonthlyUSDCents
	result.IncludedAICreditMicros = new(int64)
	if result.Source == "subscription" {
		result.IncludedAICreditMicros = base.includedAICreditMicros()
	}
	if result.Name == "" {
		result.Name = fmt.Sprintf("%s · version %d", base.ID, base.Version)
	}
	for _, grant := range result.Grants {
		if grant.RevokedAt != nil || grant.StartsAt.After(now) || grant.ExpiresAt != nil && !grant.ExpiresAt.After(now) {
			continue
		}
		plan, err := readPlan(ctx, grant.Plan)
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
	return requireHostedFeatureInEntitlement(entitlement, feature)
}

func (d *database) requireHostedFeatureRecord(ctx context.Context, reader billingstore.Reader, feature string, now time.Time) error {
	if d.plans() == nil {
		return nil
	}
	capacityPlanRefs := make([]string, 0, len(hostedTiers()))
	for _, tier := range hostedTiers() {
		if hostedCapacityPlan(tier.PlanReference) {
			capacityPlanRefs = append(capacityPlanRefs, fmt.Sprintf("%s:%d", tier.ID, tier.Version))
		}
	}
	enabled, err := reader.BillingFeatureEnabled(ctx, feature, now, capacityPlanRefs)
	if err != nil {
		return err
	}
	if !enabled {
		return &hostedLimitError{Resource: feature}
	}
	return nil
}

func requireHostedFeatureInEntitlement(entitlement HostedEntitlement, feature string) error {
	if !slices.Contains(entitlement.Features, feature) {
		return &hostedLimitError{Resource: feature}
	}
	return nil
}
