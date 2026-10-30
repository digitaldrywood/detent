package hostedplans

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

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

func AllowanceNames() []string {
	return []string{"unarchived_issues", "members", "projects", "repositories", "registered_runners", "connected_runners", "concurrent_work", "api_mutations", "ingested_events", "collaboration_bytes", "history_records", "artifact_retained_bytes", "artifact_reserved_bytes", "artifact_bytes", "artifact_upload_bytes", "artifact_retention_seconds", "relay_bytes"}
}

func FeatureNames() []string {
	return []string{"collaboration", "native_execution", "github_integration", "hosted_artifacts", "model_choice"}
}

func Pilot() HostedPlansConfig {
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

type Tier struct {
	PlanReference
	name       string
	cents      *int64
	concurrent int64
	storage    int64
	Offered    bool
}

const OpenIssueGuard = 10000

func Tiers() []Tier {
	price := func(cents int64) *int64 { return &cents }
	return []Tier{
		{PlanReference{ID: "free", Version: 1}, "Free", price(0), 2, 1 << 30, true},
		{PlanReference{ID: "starter", Version: 1}, "Starter", price(4900), 8, 25 << 30, true},
		{PlanReference{ID: "team", Version: 2}, "Team", price(9900), 20, 100 << 30, true},
		{PlanReference{ID: "growth", Version: 2}, "Growth", price(24900), 50, 250 << 30, true},
		{PlanReference{ID: "scale", Version: 2}, "Scale", nil, 100, 1 << 30, true},
		{PlanReference{ID: "growth", Version: 1}, "Growth", price(14900), 50, 1 << 30, false},
		{PlanReference{ID: "scale", Version: 1}, "Scale", price(39900), 100, 1 << 30, false},
	}
}

func TierFor(ref PlanReference) (Tier, bool) {
	for _, tier := range Tiers() {
		if tier.PlanReference == ref {
			return tier, true
		}
	}
	return Tier{}, false
}

func (tier Tier) Apply(plan HostedPlan) HostedPlan {
	if plan.Allowances == nil {
		plan.Allowances = make(map[string]int64)
	}
	plan.Allowances["unarchived_issues"], plan.Allowances["concurrent_work"] = OpenIssueGuard, tier.concurrent
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

func Capacity() HostedPlansConfig {
	config := Pilot()
	legacy := config.Plans
	config.Base = Tiers()[0].PlanReference
	config.Plans = nil
	for _, tier := range Tiers() {
		allowances := maps.Clone(legacy[1].Allowances)
		allowances["collaboration_bytes"], allowances["history_records"] = tier.storage, 2000000
		plan := tier.Apply(HostedPlan{PlanReference: tier.PlanReference, Name: tier.name, MonthlyUSDCents: tier.cents, Features: []string{"collaboration", "github_integration"}, Allowances: allowances})
		config.Plans = append(config.Plans, plan)
	}
	config.Plans = append(config.Plans, legacy...)
	return config
}

func Checkout(plans *HostedPlansConfig) *HostedPlansConfig {
	config := Capacity()
	if !plans.IsZero() {
		config = *plans
	}
	config.Plans = slices.Clone(config.Plans)
	config.Base = PlanReference{ID: "payment_pending", Version: 1}
	allowances := make(map[string]int64)
	for _, name := range AllowanceNames() {
		allowances[name] = 0
	}
	free := Capacity().Plans[0]
	for _, name := range []string{"api_mutations", "history_records", "collaboration_bytes"} {
		allowances[name] = free.Allowances[name]
	}
	config.Plans = append(config.Plans, HostedPlan{PlanReference: config.Base, Name: "Payment pending", Features: []string{}, Allowances: allowances})
	return &config
}

func IsCapacity(ref PlanReference) bool {
	_, ok := TierFor(ref)
	return ok
}

func (c *HostedPlansConfig) IsZero() bool {
	return c == nil || !c.written && len(c.Plans) == 0 && c.Base == PlanReference{} && c.WindowSeconds == 0 && c.RetentionWindows == 0 && c.ConnectedSeconds == 0 && c.InvitationSeconds == 0
}

func (c HostedPlansConfig) Validate() error {
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
			if !slices.Contains(FeatureNames(), feature) {
				return errors.New("hosted plan feature is unknown")
			}
		}
		for name, limit := range p.Allowances {
			if !slices.Contains(AllowanceNames(), name) || limit < 0 || limit > 1<<50 {
				return errors.New("hosted plan allowance is invalid")
			}
		}
	}
	if !seen[c.Base] {
		return errors.New("hosted base plan must reference a configured version")
	}
	return nil
}

func Normalize(plan HostedPlan) HostedPlan {
	plan.Allowances = maps.Clone(plan.Allowances)
	if plan.Allowances == nil {
		plan.Allowances = make(map[string]int64)
	}
	for _, name := range AllowanceNames() {
		if name == "unarchived_issues" || name == "concurrent_work" || IsCapacity(plan.PlanReference) && slices.Contains([]string{"members", "projects", "repositories", "registered_runners", "connected_runners"}, name) {
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

func ValidatePlan(plan HostedPlan) error {
	config := Pilot()
	config.Plans, config.Base = []HostedPlan{plan}, plan.PlanReference
	return config.Validate()
}

func hostedSafeID(value string) bool {
	return value != "" && len(value) <= 128 && strings.IndexFunc(value, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-'
	}) == -1
}

func (plan HostedPlan) IncludedAICreditMicros() *int64 {
	amount := int64(0)
	tier, ok := TierFor(plan.PlanReference)
	if !ok || !tier.Offered || plan.ID == "free" {
		return &amount
	}
	if plan.MonthlyUSDCents == nil {
		return nil
	}
	amount = max(0, *plan.MonthlyUSDCents) * 1000
	return &amount
}
