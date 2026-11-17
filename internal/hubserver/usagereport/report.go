package usagereport

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/usagecosts"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var usageRanges = map[string]time.Duration{
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
	"90d": 90 * 24 * time.Hour,
}

const defaultUsageRange = "7d"

type usageTotal struct {
	Cost     float64 `json:"cost"`
	Tokens   int64   `json:"tokens"`
	Sessions int64   `json:"sessions"`
}

type usageProvider struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Sessions int64   `json:"sessions"`
	Cost     float64 `json:"cost"`
	Share    float64 `json:"share"`
	Tokens   int64   `json:"tokens"`
}

type usageBucketTime struct {
	time.Time
	Hourly bool
}

const usageHourLayout = "2006-01-02T15:04:05.000Z07:00"

func (t usageBucketTime) MarshalJSON() ([]byte, error) {
	if t.Hourly {
		return json.Marshal(t.UTC().Format(usageHourLayout))
	}
	return json.Marshal(t.UTC().Format(DayLayout))
}

func (t *usageBucketTime) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if parsed, err := time.Parse(DayLayout, text); err == nil {
		t.Time, t.Hourly = parsed.UTC(), false
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return fmt.Errorf("decode usage period %q: %w", text, err)
	}
	t.Time, t.Hourly = parsed.UTC(), true
	return nil
}

type usageDay struct {
	Day        usageBucketTime    `json:"day"`
	Cost       float64            `json:"cost"`
	Tokens     int64              `json:"tokens"`
	ByProvider map[string]float64 `json:"by_provider"`
}

type usageTotals struct {
	Processed     int64   `json:"processed"`
	CachedInput   int64   `json:"cached_input"`
	UncachedInput int64   `json:"uncached_input"`
	Output        int64   `json:"output"`
	CacheSavings  float64 `json:"cache_savings"`
}

type usageModel struct {
	Model    string  `json:"model"`
	Provider string  `json:"provider"`
	Cost     float64 `json:"cost"`
	Share    float64 `json:"share"`
	Tokens   int64   `json:"tokens"`
}

type usageBreakdown struct {
	ByModel []usageModel `json:"by_model"`
	ByDay   []usageDay   `json:"by_day"`
}

type Limit struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

type usageRunner struct {
	ID           string  `json:"id"`
	DisplayName  string  `json:"display_name"`
	Sessions     int64   `json:"sessions"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
	BusySeconds  int64   `json:"busy_seconds"`
	CapacityUsed float64 `json:"capacity_used"`
}

type Report struct {
	MonthlyCosts *usagecosts.Report `json:"monthly_costs,omitempty"`
	Chat         ChatSummary        `json:"chat"`
	Range        usagecosts.Window  `json:"range"`
	Total        usageTotal         `json:"total"`
	Providers    []usageProvider    `json:"providers"`
	Daily        []usageDay         `json:"daily"`
	Totals       usageTotals        `json:"totals"`
	Breakdown    usageBreakdown     `json:"breakdown"`
	Limits       map[string]Limit   `json:"limits"`
	Runners      []usageRunner      `json:"runners"`
	Currency     string             `json:"currency"`
}

type Row struct {
	ChatSessions int64
	Chat         bool
	CacheSavings float64
	AttemptID    string
	Period       time.Time
	Provider     string
	Model        string
	Input        int64
	CachedInput  int64
	Output       int64
	Cost         float64
	Currency     string
	RunnerID     string
	RunnerName   string
	BusySeconds  int64
}

func (r Row) Tokens() int64 { return r.Input + r.Output }

func RangeWindow(value string, now time.Time) (usagecosts.Window, error) {
	name := strings.TrimSpace(value)
	if strings.HasPrefix(name, "month:") {
		if name == "month:" {
			return usagecosts.Window{}, hubapi.PublicInvalid("Month must be YYYY-MM in UTC")
		}
		return usagecosts.Month(strings.TrimPrefix(name, "month:"), now)
	}
	if name == "" {
		name = defaultUsageRange
	}
	span, known := usageRanges[name]
	if !known {
		return usagecosts.Window{}, hubapi.PublicInvalid("Range must be 24h, 7d, 30d, 90d or month:YYYY-MM in UTC")
	}
	// Usage is stored per UTC hour, so the window starts on an hour boundary:
	// the current partial hour plus the full hours before it, span in all.
	// Every stored period at or after From lies wholly inside the window.
	to := now.UTC()
	from := to.Truncate(time.Hour).Add(time.Hour - span)
	return usagecosts.Window{From: from, To: to, Hourly: span <= 24*time.Hour}, nil
}

func BusySeconds(started, updated string, window usagecosts.Window) int64 {
	if strings.TrimSpace(started) == "" || strings.TrimSpace(updated) == "" {
		return 0
	}
	from, err := parseTimeValue(started)
	if err != nil {
		return 0
	}
	to, err := parseTimeValue(updated)
	if err != nil {
		return 0
	}
	if !window.From.IsZero() && from.Before(window.From) {
		from = window.From
	}
	if !window.To.IsZero() && to.After(window.To) {
		to = window.To
	}
	seconds := int64(to.Sub(from).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}

type RunnerCapacity struct {
	DisplayName string
	Limit       int
}

func Build(window usagecosts.Window, rows []Row, limits map[string]Limit, capacity map[string]RunnerCapacity, prices usagecosts.Prices) Report {
	report := Report{
		Range: window, Providers: []usageProvider{}, Daily: []usageDay{},
		Breakdown: usageBreakdown{ByModel: []usageModel{}, ByDay: []usageDay{}},
		Limits:    limits, Runners: []usageRunner{}, Currency: prices.CurrencyValue(),
	}
	if report.Limits == nil {
		report.Limits = map[string]Limit{}
	}
	providers := map[string]*usageProvider{}
	providerAttempts := map[string]map[string]bool{}
	models := map[string]*usageModel{}
	days := map[string]*usageDay{}
	runners := map[string]*usageRunner{}
	runnerAttempts := map[string]map[string]bool{}
	attempts := map[string]bool{}
	busy := map[string]int64{}
	var chatSessions int64
	providerChatSessions := map[string]int64{}
	for _, row := range rows {
		tokens := row.Tokens()
		if row.Chat && row.ChatSessions > 0 {
			chatSessions += row.ChatSessions
			providerChatSessions[row.Provider] += row.ChatSessions
		} else {
			attempts[row.AttemptID] = true
		}
		// A hub reports in one currency. A row stored under another (the
		// table's currency changed since) still counts its tokens, but its
		// cost cannot be added without a conversion the hub does not have.
		if row.Currency != "" && !strings.EqualFold(row.Currency, report.Currency) {
			row.Cost = 0
			row.CacheSavings = 0
		}
		report.Total.Cost += row.Cost
		report.Total.Tokens += tokens
		report.Totals.Processed += tokens
		report.Totals.CachedInput += row.CachedInput
		report.Totals.UncachedInput += usageUncached(row.Input, row.CachedInput)
		report.Totals.Output += row.Output
		if row.Chat {
			report.Totals.CacheSavings += row.CacheSavings
		} else {
			report.Totals.CacheSavings += prices.CacheSaving(row.Model, row.Input, row.CachedInput)
		}

		provider, known := providers[row.Provider]
		servedAttempts := providerAttempts[row.Provider]
		if !known || servedAttempts == nil {
			provider = &usageProvider{ID: row.Provider, Label: tracker.UsageProviderLabel(row.Provider)}
			servedAttempts = map[string]bool{}
			providers[row.Provider] = provider
			providerAttempts[row.Provider] = servedAttempts
		}
		provider.Cost += row.Cost
		provider.Tokens += tokens
		if !row.Chat || row.ChatSessions == 0 {
			servedAttempts[row.AttemptID] = true
		}

		modelKey := row.Provider + "\x00" + row.Model
		model, known := models[modelKey]
		if !known {
			model = &usageModel{Model: row.Model, Provider: row.Provider}
			models[modelKey] = model
		}
		model.Cost += row.Cost
		model.Tokens += tokens

		bucket := usageBucketTime{Time: row.Period.Truncate(time.Hour), Hourly: true}
		if !window.Hourly {
			bucket = usageBucketTime{Time: time.Date(row.Period.Year(), row.Period.Month(), row.Period.Day(), 0, 0, 0, 0, time.UTC)}
		}
		bucketKey := bucket.UTC().Format(PeriodLayout)
		day, known := days[bucketKey]
		if !known {
			day = &usageDay{Day: bucket, ByProvider: map[string]float64{}}
			days[bucketKey] = day
		}
		day.Cost += row.Cost
		day.Tokens += tokens
		day.ByProvider[row.Provider] += row.Cost

		if row.RunnerID == "" {
			continue
		}
		runner, known := runners[row.RunnerID]
		ranAttempts := runnerAttempts[row.RunnerID]
		if !known || ranAttempts == nil {
			runner = &usageRunner{ID: row.RunnerID, DisplayName: row.RunnerName}
			ranAttempts = map[string]bool{}
			runners[row.RunnerID] = runner
			runnerAttempts[row.RunnerID] = ranAttempts
		}
		runner.Cost += row.Cost
		runner.Tokens += tokens
		// An attempt is counted once however many models it used.
		if !ranAttempts[row.AttemptID] {
			ranAttempts[row.AttemptID] = true
			busy[row.RunnerID] += row.BusySeconds
		}
	}
	report.Total.Sessions = int64(len(attempts)) + chatSessions
	report.Total.Cost = Round(report.Total.Cost)
	report.Totals.CacheSavings = Round(report.Totals.CacheSavings)
	total := report.Total.Cost

	for _, provider := range providers {
		provider.Sessions = int64(len(providerAttempts[provider.ID])) + providerChatSessions[provider.ID]
		provider.Cost = Round(provider.Cost)
		provider.Share = usageShare(provider.Cost, total)
		report.Providers = append(report.Providers, *provider)
	}
	slices.SortFunc(report.Providers, func(a, b usageProvider) int { return usageRank(a.Cost, b.Cost, a.ID, b.ID) })

	for _, model := range models {
		model.Cost = Round(model.Cost)
		model.Share = usageShare(model.Cost, total)
		report.Breakdown.ByModel = append(report.Breakdown.ByModel, *model)
	}
	slices.SortFunc(report.Breakdown.ByModel, func(a, b usageModel) int { return usageRank(a.Cost, b.Cost, a.Model, b.Model) })

	for _, day := range days {
		day.Cost = Round(day.Cost)
		for provider, cost := range day.ByProvider {
			day.ByProvider[provider] = Round(cost)
		}
		report.Daily = append(report.Daily, *day)
	}
	slices.SortFunc(report.Daily, func(a, b usageDay) int { return a.Day.Compare(b.Day.Time) })
	report.Breakdown.ByDay = report.Daily

	span := window.To.Sub(window.From).Seconds()
	for _, runner := range runners {
		runner.Sessions = int64(len(runnerAttempts[runner.ID]))
		runner.Cost = Round(runner.Cost)
		runner.BusySeconds = busy[runner.ID]
		if known, found := capacity[runner.ID]; found {
			if strings.TrimSpace(runner.DisplayName) == "" {
				runner.DisplayName = known.DisplayName
			}
			runner.CapacityUsed = usageCapacityUsed(runner.BusySeconds, span, known.Limit)
		}
		report.Runners = append(report.Runners, *runner)
	}
	slices.SortFunc(report.Runners, func(a, b usageRunner) int { return usageRank(a.Cost, b.Cost, a.ID, b.ID) })
	return report
}

func usageUncached(input, cached int64) int64 {
	if cached >= input {
		return 0
	}
	return input - cached
}

func usageShare(cost, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(cost/total*10000) / 10000
}

func Round(value float64) float64 { return math.Round(value*10000) / 10000 }

func usageCapacityUsed(busySeconds int64, span float64, limit int) float64 {
	if busySeconds <= 0 || span <= 0 || limit <= 0 {
		return 0
	}
	used := float64(busySeconds) / (span * float64(limit))
	return math.Round(min(used, 1)*10000) / 10000
}

func usageRank(leftCost, rightCost float64, leftID, rightID string) int {
	switch {
	case leftCost > rightCost:
		return -1
	case leftCost < rightCost:
		return 1
	default:
		return strings.Compare(leftID, rightID)
	}
}

type ChatSummary struct {
	Range           usagecosts.Window `json:"range"`
	Input           int64             `json:"input"`
	CachedInput     int64             `json:"cached_input"`
	Output          int64             `json:"output"`
	ReasoningOutput int64             `json:"reasoning_output"`
	Tokens          int64             `json:"tokens"`
	Turns           int64             `json:"turns"`
	UnpricedTurns   int64             `json:"unpriced_turns"`
	CostUSD         float64           `json:"cost_usd"`
}

const (
	PeriodLayout = "2006-01-02T15:04:05Z"
	DayLayout    = "2006-01-02"
)

func parseTimeValue(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
}
