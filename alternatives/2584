package retro

import (
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"
)

const (
	DefaultSchedule                 = "0 3 * * *"
	DefaultTargetState              = "Backlog"
	DefaultProductRepository        = "digitaldrywood/detent"
	DefaultDailyIssueCap            = 3
	DefaultLookbackDays             = 7
	DefaultMinOccurrences           = 2
	DefaultFallbackThreshold        = 3
	DefaultReceiptBaselineMultiple  = 4
	DefaultSingleOccurrenceSeverity = SeverityCritical
)

type Config struct {
	Enabled                  bool     `yaml:"enabled"`
	Schedule                 string   `yaml:"schedule,omitempty"`
	TargetState              string   `yaml:"target_state,omitempty"`
	Labels                   []string `yaml:"labels,omitempty"`
	ProductRepository        string   `yaml:"product_repository,omitempty"`
	DailyIssueCap            int      `yaml:"daily_issue_cap,omitempty"`
	LookbackDays             int      `yaml:"lookback_days,omitempty"`
	MinOccurrences           int      `yaml:"min_occurrences,omitempty"`
	SingleOccurrenceSeverity string   `yaml:"single_occurrence_severity,omitempty"`
	FallbackThreshold        int      `yaml:"fallback_threshold,omitempty"`
	ReceiptBaselineMultiple  float64  `yaml:"receipt_baseline_multiple,omitempty"`

	AllowPublicCrossProjectDetails bool `yaml:"allow_public_cross_project_details,omitempty"`
}

func (c *Config) Normalize() {
	if c == nil {
		return
	}
	c.Schedule = strings.TrimSpace(c.Schedule)
	c.TargetState = strings.TrimSpace(c.TargetState)
	c.ProductRepository = strings.TrimSpace(c.ProductRepository)
	c.SingleOccurrenceSeverity = strings.ToLower(strings.TrimSpace(c.SingleOccurrenceSeverity))
	c.Labels = normalizeLabels(c.Labels)
	if !c.Enabled {
		return
	}
	if c.Schedule == "" {
		c.Schedule = DefaultSchedule
	}
	if c.TargetState == "" {
		c.TargetState = DefaultTargetState
	}
	if c.ProductRepository == "" {
		c.ProductRepository = DefaultProductRepository
	}
	if c.DailyIssueCap == 0 {
		c.DailyIssueCap = DefaultDailyIssueCap
	}
	if c.LookbackDays == 0 {
		c.LookbackDays = DefaultLookbackDays
	}
	if c.MinOccurrences == 0 {
		c.MinOccurrences = DefaultMinOccurrences
	}
	if c.SingleOccurrenceSeverity == "" {
		c.SingleOccurrenceSeverity = DefaultSingleOccurrenceSeverity
	}
	if c.FallbackThreshold == 0 {
		c.FallbackThreshold = DefaultFallbackThreshold
	}
	if c.ReceiptBaselineMultiple == 0 {
		c.ReceiptBaselineMultiple = DefaultReceiptBaselineMultiple
	}
	if !containsFold(c.Labels, "retro") {
		c.Labels = append(c.Labels, "retro")
	}
}

func (c Config) Validate(prefix string, states []string) []string {
	c.Normalize()
	if prefix == "" {
		prefix = "retro"
	}
	if !c.Enabled {
		return nil
	}
	var problems []string
	if _, err := cron.ParseStandard(c.Schedule); err != nil {
		problems = append(problems, prefix+".schedule must be a valid five-field cron expression")
	}
	if c.TargetState == "" {
		problems = append(problems, prefix+".target_state is required when "+prefix+".enabled is true")
	} else if len(states) > 0 && !containsFold(states, c.TargetState) {
		problems = append(problems, prefix+".target_state must name a configured tracker state")
	}
	if !validRepository(c.ProductRepository) {
		problems = append(problems, prefix+".product_repository must be owner/name")
	}
	if c.DailyIssueCap <= 0 {
		problems = append(problems, prefix+".daily_issue_cap must be greater than 0")
	}
	if c.LookbackDays <= 0 {
		problems = append(problems, prefix+".lookback_days must be greater than 0")
	}
	if c.MinOccurrences < 2 {
		problems = append(problems, prefix+".min_occurrences must be at least 2")
	}
	if severityRank(c.SingleOccurrenceSeverity) == 0 {
		problems = append(problems, prefix+".single_occurrence_severity must be one of info, warning, high, critical")
	}
	if c.FallbackThreshold < 2 {
		problems = append(problems, prefix+".fallback_threshold must be at least 2")
	}
	if c.ReceiptBaselineMultiple <= 1 {
		problems = append(problems, prefix+".receipt_baseline_multiple must be greater than 1")
	}
	for index, label := range c.Labels {
		if strings.ContainsAny(label, "\r\n") {
			problems = append(problems, fmt.Sprintf("%s.labels[%d] must be a single line", prefix, index))
		}
	}
	return problems
}

func normalizeLabels(labels []string) []string {
	out := make([]string, 0, len(labels))
	seen := map[string]struct{}{}
	for _, label := range labels {
		label = strings.TrimSpace(label)
		key := strings.ToLower(label)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, label)
	}
	return out
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func validRepository(value string) bool {
	owner, name, ok := strings.Cut(strings.TrimSpace(value), "/")
	return ok && strings.TrimSpace(owner) != "" && strings.TrimSpace(name) != "" && !strings.Contains(name, "/")
}
