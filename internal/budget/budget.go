package budget

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/digitaldrywood/symphony-go/internal/store"
)

type ReasonCode string

const (
	ReasonPerDayMaxUSD   ReasonCode = "per_day_max_usd"
	ReasonPerIssueMaxUSD ReasonCode = "per_issue_max_usd"
)

var ErrMissingSpendStore = errors.New("budget spend store is required")

type Config struct {
	Enabled         bool
	PerDayMaxUSD    float64
	PerIssueMaxUSD  float64
	RefusalCooldown time.Duration
	PricingPath     string
}

type SpendStore interface {
	DailyTokenSpend(context.Context, time.Time) (store.TokenSpend, error)
	IssueTokenSpend(context.Context, store.IssueIdentity) (store.TokenSpend, error)
}

type Checker struct {
	cfg     Config
	spend   SpendStore
	pricing PricingTable
}

type DispatchRequest struct {
	IssueID    string
	Identifier string
	IssueURL   string
	Model      string
	Now        time.Time
	Estimate   TokenEstimate
}

type TokenEstimate struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	Sessions     int64
}

type Decision struct {
	Allowed bool
	Refusal *Refusal
}

type Refusal struct {
	Code              ReasonCode
	Message           string
	IssueID           string
	Identifier        string
	IssueURL          string
	Model             string
	CurrentSpendUSD   float64
	ProjectedCostUSD  float64
	ProjectedSpendUSD float64
	MaxUSD            *float64
	ResetAt           *time.Time
	RefusedAt         time.Time
	CooldownUntil     time.Time
	Cooldown          time.Duration
}

type TrackedRefusal struct {
	Refusal Refusal
	DueAt   time.Time
}

type RefusalTracker struct {
	mu      sync.Mutex
	entries map[string]TrackedRefusal
}

var defaultTokenEstimate = TokenEstimate{
	InputTokens:  150_000,
	OutputTokens: 20_000,
	TotalTokens:  170_000,
}

func NewChecker(cfg Config, spend SpendStore, pricing PricingTable) *Checker {
	if pricing == nil {
		pricing = DefaultPricingTable()
	}

	return &Checker{
		cfg:     cfg,
		spend:   spend,
		pricing: pricing,
	}
}

func NewCheckerFromConfig(cfg Config, spend SpendStore) (*Checker, error) {
	pricing, err := PricingForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return NewChecker(cfg, spend, pricing), nil
}

func (c *Checker) CheckDispatch(ctx context.Context, req DispatchRequest) (Decision, error) {
	if !c.cfg.Enabled {
		return Decision{Allowed: true}, nil
	}

	model := normalizeModel(req.Model)
	modelPricing, ok := c.pricing.Lookup(model)
	if !ok {
		return Decision{Allowed: true}, nil
	}

	now := req.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}

	projectedCost := tokenCostUSD(req.Estimate.normalized(), modelPricing)
	if capActive(c.cfg.PerDayMaxUSD) {
		if missingSpendStore(c.spend) {
			return Decision{}, ErrMissingSpendStore
		}
		dailySpend, err := c.spend.DailyTokenSpend(ctx, now)
		if err != nil {
			return Decision{}, fmt.Errorf("daily token spend: %w", err)
		}
		currentSpend := SpendUSD(dailySpend, c.pricing)
		if currentSpend+projectedCost > c.cfg.PerDayMaxUSD {
			return Decision{
				Refusal: c.refusal(ReasonPerDayMaxUSD, req, now, currentSpend, projectedCost, c.cfg.PerDayMaxUSD, nextDailyReset(now)),
			}, nil
		}
	}

	if capActive(c.cfg.PerIssueMaxUSD) {
		currentSpend := 0.0
		identity := store.IssueIdentity{
			IssueID:    req.IssueID,
			Identifier: req.Identifier,
			IssueURL:   req.IssueURL,
		}
		if issueIdentityPresent(identity) {
			if missingSpendStore(c.spend) {
				return Decision{}, ErrMissingSpendStore
			}
			issueSpend, err := c.spend.IssueTokenSpend(ctx, identity)
			if err != nil {
				return Decision{}, fmt.Errorf("issue token spend: %w", err)
			}
			currentSpend = SpendUSD(issueSpend, c.pricing)
		}
		if currentSpend+projectedCost > c.cfg.PerIssueMaxUSD {
			return Decision{
				Refusal: c.refusal(ReasonPerIssueMaxUSD, req, now, currentSpend, projectedCost, c.cfg.PerIssueMaxUSD, nil),
			}, nil
		}
	}

	return Decision{Allowed: true}, nil
}

func SpendUSD(spend store.TokenSpend, pricing PricingTable) float64 {
	total := 0.0
	for _, row := range spend.ByModel {
		modelPricing, ok := pricing.Lookup(row.Model)
		if !ok {
			continue
		}
		total += tokenCostUSD(TokenEstimate{
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
			TotalTokens:  row.TotalTokens,
			Sessions:     row.Sessions,
		}, modelPricing)
	}
	return total
}

func FormatUSD(value float64) string {
	return fmt.Sprintf("$%.2f", value)
}

func formatOptionalUSD(value *float64) string {
	if value == nil {
		return "n/a"
	}
	return FormatUSD(*value)
}

func NewRefusalTracker() *RefusalTracker {
	return &RefusalTracker{
		entries: map[string]TrackedRefusal{},
	}
}

func (t *RefusalTracker) Record(refusal Refusal) (TrackedRefusal, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.entries == nil {
		t.entries = map[string]TrackedRefusal{}
	}

	key := refusalKey(refusal.IssueID, refusal.Identifier, refusal.IssueURL)
	now := refusal.RefusedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
		refusal.RefusedAt = now
	}

	if tracked, ok := t.entries[key]; ok && tracked.DueAt.After(now) {
		return tracked, false
	}

	tracked := TrackedRefusal{
		Refusal: refusal,
		DueAt:   refusal.CooldownUntil.UTC(),
	}
	t.entries[key] = tracked
	return tracked, true
}

func (t *RefusalTracker) CooldownActive(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.entries == nil {
		return false
	}

	tracked, ok := t.entries[strings.TrimSpace(key)]
	return ok && tracked.DueAt.After(now.UTC())
}

func (r Refusal) Comment() string {
	max := formatOptionalUSD(r.MaxUSD)
	projectedCost := FormatUSD(r.ProjectedCostUSD)
	currentSpend := FormatUSD(r.CurrentSpendUSD)
	projectedSpend := FormatUSD(r.ProjectedSpendUSD)
	model := r.Model
	if strings.TrimSpace(model) == "" {
		model = "unknown"
	}

	switch r.Code {
	case ReasonPerDayMaxUSD:
		resetAt := "n/a"
		if r.ResetAt != nil {
			resetAt = r.ResetAt.UTC().Format(time.RFC3339)
		}
		return strings.TrimSpace(fmt.Sprintf(`
Symphony refused to dispatch this issue because the projected dispatch would exceed the daily budget.

Current daily spend: %s
Projected dispatch cost: %s
Projected daily spend: %s / %s
Model: %s
Daily budget resets at: %s
This issue will be reconsidered after: %s
`, currentSpend, projectedCost, projectedSpend, max, model, resetAt, r.CooldownUntil.UTC().Format(time.RFC3339)))
	case ReasonPerIssueMaxUSD:
		return strings.TrimSpace(fmt.Sprintf(`
Symphony refused to dispatch this issue because the projected dispatch would exceed the per-issue budget.

Current issue spend: %s
Projected dispatch cost: %s
Projected issue spend: %s / %s
Model: %s
The per-issue budget has no automatic reset.
This issue will be reconsidered after: %s
`, currentSpend, projectedCost, projectedSpend, max, model, r.CooldownUntil.UTC().Format(time.RFC3339)))
	default:
		return strings.TrimSpace(fmt.Sprintf(`
Symphony refused to dispatch this issue because the budget check failed.

Current spend: %s
Projected dispatch cost: %s
Projected spend: %s / %s
Model: %s
This issue will be reconsidered after: %s
`, currentSpend, projectedCost, projectedSpend, max, model, r.CooldownUntil.UTC().Format(time.RFC3339)))
	}
}

func (c *Checker) refusal(code ReasonCode, req DispatchRequest, now time.Time, currentSpend float64, projectedCost float64, cap float64, resetAt *time.Time) *Refusal {
	message := "budget exceeded"
	switch code {
	case ReasonPerDayMaxUSD:
		message = "daily budget exceeded"
	case ReasonPerIssueMaxUSD:
		message = "per-issue budget exceeded"
	}

	maxUSD := cap
	projectedSpend := currentSpend + projectedCost
	return &Refusal{
		Code:              code,
		Message:           message,
		IssueID:           strings.TrimSpace(req.IssueID),
		Identifier:        strings.TrimSpace(req.Identifier),
		IssueURL:          strings.TrimSpace(req.IssueURL),
		Model:             normalizeModel(req.Model),
		CurrentSpendUSD:   currentSpend,
		ProjectedCostUSD:  projectedCost,
		ProjectedSpendUSD: projectedSpend,
		MaxUSD:            &maxUSD,
		ResetAt:           resetAt,
		RefusedAt:         now,
		CooldownUntil:     now.Add(c.cfg.RefusalCooldown),
		Cooldown:          c.cfg.RefusalCooldown,
	}
}

func (e TokenEstimate) normalized() TokenEstimate {
	out := TokenEstimate{
		InputTokens:  nonNegative(e.InputTokens),
		OutputTokens: nonNegative(e.OutputTokens),
		TotalTokens:  nonNegative(e.TotalTokens),
		Sessions:     nonNegative(e.Sessions),
	}
	if out.InputTokens == 0 && out.OutputTokens == 0 && out.TotalTokens == 0 {
		return defaultTokenEstimate
	}
	if out.TotalTokens == 0 {
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	return out
}

func tokenCostUSD(tokens TokenEstimate, pricing ModelPricing) float64 {
	return float64(tokens.InputTokens)*pricing.USDPerInputToken +
		float64(tokens.OutputTokens)*pricing.USDPerOutputToken
}

func capActive(cap float64) bool {
	return cap > 0
}

func issueIdentityPresent(identity store.IssueIdentity) bool {
	return strings.TrimSpace(identity.IssueID) != "" ||
		strings.TrimSpace(identity.Identifier) != "" ||
		strings.TrimSpace(identity.IssueURL) != ""
}

func missingSpendStore(spend SpendStore) bool {
	if spend == nil {
		return true
	}

	value := reflect.ValueOf(spend)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

func nextDailyReset(now time.Time) *time.Time {
	year, month, day := now.UTC().Date()
	reset := time.Date(year, month, day+1, 0, 0, 0, 0, time.UTC)
	return &reset
}

func nonNegative(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func refusalKey(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return "unknown"
}
