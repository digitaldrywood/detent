package hostedusage

import (
	"encoding/json"
	"fmt"
	"github.com/digitaldrywood/detent/internal/tracker"
	"math"
	"slices"
	"strings"
	"time"
)

const usageDayLayout = "2006-01-02"
const usagePeriodLayout = "2006-01-02T15:04:05Z"

type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	// Hourly reports the series per hour rather than per day: the client
	// draws the 24h range from hourly buckets (contracts/usage.ts, isHourly).
	Hourly bool `json:"-"`
}

type Total struct {
	Cost     float64 `json:"cost"`
	Tokens   int64   `json:"tokens"`
	Sessions int64   `json:"sessions"`
}

type Provider struct {
	ID       string  `json:"id"`
	Label    string  `json:"label"`
	Sessions int64   `json:"sessions"`
	Cost     float64 `json:"cost"`
	Share    float64 `json:"share"`
	Tokens   int64   `json:"tokens"`
}

// BucketTime is when one period of the daily series starts. A day
// bucket is written as the day alone and an hourly bucket keeps its clock:
// that is how the client tells the two apart (contracts/usage.ts, isHourly),
// so a day written with a clock would be drawn as one hour and the daily
// chart would be empty.
type BucketTime struct {
	time.Time
	Hourly bool
}

const usageHourLayout = "2006-01-02T15:04:05.000Z07:00"

func (t BucketTime) MarshalJSON() ([]byte, error) {
	if t.Hourly {
		return json.Marshal(t.UTC().Format(usageHourLayout))
	}
	return json.Marshal(t.UTC().Format(usageDayLayout))
}

func (t *BucketTime) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err
	}
	if parsed, err := time.Parse(usageDayLayout, text); err == nil {
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

type Day struct {
	Day        BucketTime         `json:"day"`
	Cost       float64            `json:"cost"`
	Tokens     int64              `json:"tokens"`
	ByProvider map[string]float64 `json:"by_provider"`
}

type Totals struct {
	Processed     int64   `json:"processed"`
	CachedInput   int64   `json:"cached_input"`
	UncachedInput int64   `json:"uncached_input"`
	Output        int64   `json:"output"`
	CacheSavings  float64 `json:"cache_savings"`
}

type Model struct {
	Model    string  `json:"model"`
	Provider string  `json:"provider"`
	Cost     float64 `json:"cost"`
	Share    float64 `json:"share"`
	Tokens   int64   `json:"tokens"`
}

type Breakdown struct {
	ByModel []Model `json:"by_model"`
	ByDay   []Day   `json:"by_day"`
}

type Limit struct {
	Used  int64 `json:"used"`
	Limit int64 `json:"limit"`
}

type Runner struct {
	ID           string  `json:"id"`
	DisplayName  string  `json:"display_name"`
	Sessions     int64   `json:"sessions"`
	Tokens       int64   `json:"tokens"`
	Cost         float64 `json:"cost"`
	BusySeconds  int64   `json:"busy_seconds"`
	CapacityUsed float64 `json:"capacity_used"`
}

type Report[Monthly any] struct {
	MonthlyCosts *Monthly         `json:"monthly_costs,omitempty"`
	Chat         ChatSummary      `json:"chat"`
	Range        Window           `json:"range"`
	Total        Total            `json:"total"`
	Providers    []Provider       `json:"providers"`
	Daily        []Day            `json:"daily"`
	Totals       Totals           `json:"totals"`
	Breakdown    Breakdown        `json:"breakdown"`
	Limits       map[string]Limit `json:"limits"`
	Runners      []Runner         `json:"runners"`
	Currency     string           `json:"currency"`
}

// Row is one stored attempt_usage row joined with what the report needs
// to attribute it: the runner that ran the attempt and how long it was busy.
type Row struct {
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

type ChatSummary struct {
	Range           Window  `json:"range"`
	Input           int64   `json:"input"`
	CachedInput     int64   `json:"cached_input"`
	Output          int64   `json:"output"`
	ReasoningOutput int64   `json:"reasoning_output"`
	Tokens          int64   `json:"tokens"`
	Turns           int64   `json:"turns"`
	UnpricedTurns   int64   `json:"unpriced_turns"`
	CostUSD         float64 `json:"cost_usd"`
}

type RunnerCapacity struct {
	DisplayName string
	Limit       int
}

// BuildReport folds the stored rows into the usage report shape. It is a
// pure function of what it is given so the aggregation is testable without a
// database.
func BuildReport[Monthly any](window Window, rows []Row, limits map[string]Limit, capacity map[string]RunnerCapacity, prices Config) Report[Monthly] {
	report := Report[Monthly]{
		Range: window, Providers: []Provider{}, Daily: []Day{},
		Breakdown: Breakdown{ByModel: []Model{}, ByDay: []Day{}},
		Limits:    limits, Runners: []Runner{}, Currency: prices.CurrencyName(),
	}
	if report.Limits == nil {
		report.Limits = map[string]Limit{}
	}
	providers := map[string]*Provider{}
	providerAttempts := map[string]map[string]bool{}
	models := map[string]*Model{}
	days := map[string]*Day{}
	runners := map[string]*Runner{}
	runnerAttempts := map[string]map[string]bool{}
	attempts := map[string]bool{}
	busy := map[string]int64{}
	for _, row := range rows {
		tokens := row.Tokens()
		attempts[row.AttemptID] = true
		// A hub reports in one currency. A row stored under another (the
		// table's currency changed since) still counts its tokens, but its
		// cost cannot be added without a conversion the hub does not have.
		if row.Currency != "" && row.Currency != report.Currency {
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
			provider = &Provider{ID: row.Provider, Label: tracker.UsageProviderLabel(row.Provider)}
			servedAttempts = map[string]bool{}
			providers[row.Provider] = provider
			providerAttempts[row.Provider] = servedAttempts
		}
		provider.Cost += row.Cost
		provider.Tokens += tokens
		servedAttempts[row.AttemptID] = true

		modelKey := row.Provider + "\x00" + row.Model
		model, known := models[modelKey]
		if !known {
			model = &Model{Model: row.Model, Provider: row.Provider}
			models[modelKey] = model
		}
		model.Cost += row.Cost
		model.Tokens += tokens

		bucket := BucketTime{Time: row.Period.Truncate(time.Hour), Hourly: true}
		if !window.Hourly {
			bucket = BucketTime{Time: time.Date(row.Period.Year(), row.Period.Month(), row.Period.Day(), 0, 0, 0, 0, time.UTC)}
		}
		bucketKey := bucket.UTC().Format(usagePeriodLayout)
		day, known := days[bucketKey]
		if !known {
			day = &Day{Day: bucket, ByProvider: map[string]float64{}}
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
			runner = &Runner{ID: row.RunnerID, DisplayName: row.RunnerName}
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
	report.Total.Sessions = int64(len(attempts))
	report.Total.Cost = usageRound(report.Total.Cost)
	report.Totals.CacheSavings = usageRound(report.Totals.CacheSavings)
	total := report.Total.Cost

	for _, provider := range providers {
		provider.Sessions = int64(len(providerAttempts[provider.ID]))
		provider.Cost = usageRound(provider.Cost)
		provider.Share = usageShare(provider.Cost, total)
		report.Providers = append(report.Providers, *provider)
	}
	slices.SortFunc(report.Providers, func(a, b Provider) int { return usageRank(a.Cost, b.Cost, a.ID, b.ID) })

	for _, model := range models {
		model.Cost = usageRound(model.Cost)
		model.Share = usageShare(model.Cost, total)
		report.Breakdown.ByModel = append(report.Breakdown.ByModel, *model)
	}
	slices.SortFunc(report.Breakdown.ByModel, func(a, b Model) int { return usageRank(a.Cost, b.Cost, a.Model, b.Model) })

	for _, day := range days {
		day.Cost = usageRound(day.Cost)
		for provider, cost := range day.ByProvider {
			day.ByProvider[provider] = usageRound(cost)
		}
		report.Daily = append(report.Daily, *day)
	}
	slices.SortFunc(report.Daily, func(a, b Day) int { return a.Day.Compare(b.Day.Time) })
	report.Breakdown.ByDay = report.Daily

	span := window.To.Sub(window.From).Seconds()
	for _, runner := range runners {
		runner.Sessions = int64(len(runnerAttempts[runner.ID]))
		runner.Cost = usageRound(runner.Cost)
		runner.BusySeconds = busy[runner.ID]
		if known, found := capacity[runner.ID]; found {
			if strings.TrimSpace(runner.DisplayName) == "" {
				runner.DisplayName = known.DisplayName
			}
			runner.CapacityUsed = usageCapacityUsed(runner.BusySeconds, span, known.Limit)
		}
		report.Runners = append(report.Runners, *runner)
	}
	slices.SortFunc(report.Runners, func(a, b Runner) int { return usageRank(a.Cost, b.Cost, a.ID, b.ID) })
	return report
}

// usageUncached is the input a provider had to read rather than serve from
// its cache.
func usageUncached(input, cached int64) int64 {
	if cached >= input {
		return 0
	}
	return input - cached
}

// usageShare is one row's part of the window's cost. A window that cost
// nothing has no shares rather than a division by zero.
func usageShare(cost, total float64) float64 {
	if total <= 0 {
		return 0
	}
	return math.Round(cost/total*10000) / 10000
}

// usageRound keeps money to four decimals: enough for a fraction of a cent,
// short of floating-point noise.
func usageRound(value float64) float64 { return math.Round(value*10000) / 10000 }

// usageCapacityUsed is the share of the window the runner was busy, against
// the concurrent work it is allowed. A runner with no capacity limit reports
// none rather than a division by zero.
func usageCapacityUsed(busySeconds int64, span float64, limit int) float64 {
	if busySeconds <= 0 || span <= 0 || limit <= 0 {
		return 0
	}
	used := float64(busySeconds) / (span * float64(limit))
	return math.Round(min(used, 1)*10000) / 10000
}

// usageRank orders a report list by cost, descending, with the identifier
// breaking ties so the order is stable.
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
