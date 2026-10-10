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
	Plans             []HostedPlan  `yaml:"plans"`
	Base              PlanReference `yaml:"base"`
	WindowSeconds     int64         `yaml:"window_seconds"`
	RetentionWindows  int64         `yaml:"retention_windows"`
	ConnectedSeconds  int64         `yaml:"connected_seconds"`
	InvitationSeconds int64         `yaml:"invitation_seconds"`

	// written records that the YAML section named at least one key, so an
	// explicitly empty plan list is an error rather than the defaults.
	written bool
}

// UnmarshalYAML decodes the catalog and remembers whether the section had
// any keys: `entitlements: {}` means the default catalog, while a section
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

func capacityHostedPlans() HostedPlansConfig {
	config := pilotHostedPlans()
	legacy := config.Plans
	config.Base = PlanReference{ID: "free", Version: 1}
	config.Plans = nil
	for _, tier := range []struct {
		id, name                string
		cents, projects, issues int64
	}{
		{"free", "Free", 0, 1, 200},
		{"starter", "Starter", 4900, 5, 2000},
		{"growth", "Growth", 14900, 25, 10000},
		{"scale", "Scale", 39900, 100, 50000},
	} {
		allowances := maps.Clone(legacy[1].Allowances)
		allowances["projects"], allowances["unarchived_issues"] = tier.projects, tier.issues
		allowances["collaboration_bytes"], allowances["history_records"] = 1<<30, 2000000
		for _, name := range []string{"members", "repositories", "registered_runners", "connected_runners", "concurrent_work"} {
			delete(allowances, name)
		}
		features := []string{"collaboration", "github_integration"}
		if tier.cents > 0 {
			features = append(features, "native_execution", "hosted_artifacts")
		}
		cents := tier.cents
		config.Plans = append(config.Plans, HostedPlan{PlanReference: PlanReference{ID: tier.id, Version: 1}, Name: tier.name, MonthlyUSDCents: &cents, Features: features, Allowances: allowances})
	}
	config.Plans = append(config.Plans, legacy...)
	return config
}

func defaultHostedPlans(cfg *HostedConfig) HostedPlansConfig {
	config := capacityHostedPlans()
	if cfg.PlanID == "" && cfg.StorageQuotaBytes == 0 && cfg.EventQuota == 0 {
		return config
	}
	config = pilotHostedPlans()
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
	return ref.Version == 1 && slices.Contains([]string{"free", "starter", "growth", "scale"}, ref.ID)
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
	if err := config.validate(); err != nil {
		return err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, plan := range config.Plans {
		plan.Allowances = maps.Clone(plan.Allowances)
		if plan.Allowances == nil {
			plan.Allowances = make(map[string]int64)
		}
		for _, name := range hostedAllowanceNames() {
			if name == "unarchived_issues" || hostedCapacityPlan(plan.PlanReference) && slices.Contains([]string{"members", "repositories", "registered_runners", "connected_runners", "concurrent_work"}, name) {
				continue
			}
			if _, exists := plan.Allowances[name]; !exists {
				plan.Allowances[name] = 0
			}
		}
		plan.Features = slices.Clone(plan.Features)
		slices.Sort(plan.Features)
		plan.Features = slices.Compact(plan.Features)
		encoded, err := json.Marshal(plan)
		if err != nil {
			return err
		}
		var existing string
		err = tx.QueryRowContext(ctx, "SELECT record_json FROM hosted_plans WHERE id = ? AND version = ?", plan.ID, plan.Version).Scan(&existing)
		if err == nil && existing != string(encoded) {
			return errors.New("hosted plan versions are immutable; configure a new version")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO hosted_plans(id,version,record_json) VALUES(?,?,?) ON CONFLICT DO NOTHING", plan.ID, plan.Version, string(encoded)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hosted_plan_assignments(organization_id,base_id,base_version) VALUES(?,?,?) ON CONFLICT DO NOTHING`, d.hostedOrganization, config.Base.ID, config.Base.Version); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.hostedPlans = &config
	return nil
}

func readHostedPlan(ctx context.Context, query nativeQueryer, ref PlanReference) (HostedPlan, error) {
	var raw string
	var plan HostedPlan
	if err := query.QueryRowContext(ctx, "SELECT record_json FROM hosted_plans WHERE id = ? AND version = ?", ref.ID, ref.Version).Scan(&raw); err != nil {
		return plan, err
	}
	err := json.Unmarshal([]byte(raw), &plan)
	if err == nil {
		if plan.Allowances == nil {
			plan.Allowances = make(map[string]int64)
		}
		if _, exists := plan.Allowances["unarchived_issues"]; !exists {
			plan.Allowances["unarchived_issues"] = 200
		}
		if ref.Version == 1 && slices.Contains([]string{"pilot_free", "comp_team"}, ref.ID) {
			var capacityCatalog bool
			if err := query.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM hosted_plans WHERE id='starter' AND version=1)").Scan(&capacityCatalog); err != nil {
				return plan, err
			}
			if capacityCatalog {
				plan.Allowances["projects"] = max(plan.Allowances["projects"], 5)
				plan.Allowances["unarchived_issues"] = max(plan.Allowances["unarchived_issues"], 2000)
				plan.Allowances["collaboration_bytes"] = max(plan.Allowances["collaboration_bytes"], 1<<30)
				plan.Allowances["history_records"] = max(plan.Allowances["history_records"], 2000000)
				for _, resource := range []string{"members", "repositories", "registered_runners", "connected_runners", "concurrent_work"} {
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
	result := HostedEntitlement{OrganizationID: string(d.hostedOrganization), Source: "base", Grants: []HostedGrant{}}
	if d.hostedPlans == nil {
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
	base, err := readHostedPlan(ctx, query, result.EffectiveBase)
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
		plan, err := readHostedPlan(ctx, query, grant.Plan)
		if err != nil {
			return result, err
		}
		for _, scope := range grant.Scope {
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
	delete(result.Allowances, "concurrent_work")
	slices.Sort(result.Features)
	result.WindowEndsAt = time.Unix((now.Unix()/d.hostedPlans.WindowSeconds+1)*d.hostedPlans.WindowSeconds, 0).UTC()
	return result, nil
}

type hostedLimitError struct {
	Resource    string `json:"resource"`
	Allowance   int64  `json:"allowance"`
	Consumption int64  `json:"consumption"`
}

func (e *hostedLimitError) Error() string {
	if e.Resource == "native_execution" {
		return "Free includes project and issue exploration, with no AI turns. Upgrade to a paid plan or request complimentary execution access. Model-provider charges remain separate."
	}
	return fmt.Sprintf("Hosted %s allowance reached (%d of %d). Existing data, reading, export and billing remain available.", e.Resource, e.Consumption, e.Allowance)
}

func (d *database) requireHostedFeature(ctx context.Context, query nativeQueryer, feature string, now time.Time) error {
	if d.hostedPlans == nil {
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
