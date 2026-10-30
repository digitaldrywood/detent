package hostedusage

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

func TestBuildUsageReport(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	window := Window{From: day, To: next.Add(24 * time.Hour)}
	rows := []Row{
		{AttemptID: "attempt_1", Period: day, Provider: "codex", Model: "gpt-6-astra", Input: 1_000_000, CachedInput: 400_000, Output: 100_000, Cost: 10, Currency: "USD", RunnerID: "rnr_1", RunnerName: "Studio", BusySeconds: 3600},
		{AttemptID: "attempt_1", Period: day, Provider: "claude", Model: "claude-opus-5", Input: 200_000, Output: 50_000, Cost: 5, Currency: "USD", RunnerID: "rnr_1", RunnerName: "Studio", BusySeconds: 3600},
		{AttemptID: "attempt_2", Period: next, Provider: "codex", Model: "gpt-6-astra", Input: 500_000, CachedInput: 100_000, Output: 20_000, Cost: 5, Currency: "USD", RunnerID: "rnr_2", RunnerName: "Mini", BusySeconds: 1800},
	}
	limits := map[string]Limit{"members": {Used: 3, Limit: 10}}
	capacity := map[string]RunnerCapacity{"rnr_1": {DisplayName: "Studio", Limit: 2}, "rnr_2": {DisplayName: "Mini", Limit: 1}}
	report := BuildReport[struct{}](window, rows, limits, capacity, usageTestPrices())

	if report.Total.Cost != 20 || report.Total.Sessions != 2 {
		t.Fatalf("total = %+v, want 20 across 2 sessions", report.Total)
	}
	if want := int64(1_000_000 + 100_000 + 200_000 + 50_000 + 500_000 + 20_000); report.Total.Tokens != want {
		t.Fatalf("total tokens = %d, want %d", report.Total.Tokens, want)
	}
	if report.Totals.Processed != report.Total.Tokens {
		t.Fatalf("totals.processed = %d, want %d", report.Totals.Processed, report.Total.Tokens)
	}
	if report.Totals.CachedInput != 500_000 {
		t.Fatalf("totals.cached_input = %d, want 500000", report.Totals.CachedInput)
	}
	if report.Totals.UncachedInput != 600_000+200_000+400_000 {
		t.Fatalf("totals.uncached_input = %d, want 1200000", report.Totals.UncachedInput)
	}
	if report.Totals.Output != 170_000 {
		t.Fatalf("totals.output = %d, want 170000", report.Totals.Output)
	}
	// 400k plus 100k cached tokens of gpt-6-astra, saving 9 per million.
	if want := reportRound(0.5 * 9); report.Totals.CacheSavings != want {
		t.Fatalf("totals.cache_savings = %v, want %v", report.Totals.CacheSavings, want)
	}

	if len(report.Providers) != 2 {
		t.Fatalf("providers = %+v, want two", report.Providers)
	}
	if report.Providers[0].ID != "codex" || report.Providers[0].Label != "Codex" || report.Providers[0].Cost != 15 || report.Providers[0].Sessions != 2 {
		t.Fatalf("first provider = %+v, want Codex at 15 across 2 sessions", report.Providers[0])
	}
	if report.Providers[0].Share != 0.75 || report.Providers[1].Share != 0.25 {
		t.Fatalf("provider shares = %v, %v, want 0.75 and 0.25", report.Providers[0].Share, report.Providers[1].Share)
	}
	if report.Providers[1].ID != "claude" || report.Providers[1].Label != "Claude Code" {
		t.Fatalf("second provider = %+v, want Claude Code", report.Providers[1])
	}

	if len(report.Daily) != 2 || !report.Daily[0].Day.Equal(day) || report.Daily[0].Cost != 15 {
		t.Fatalf("daily = %+v, want two ascending days starting at 15", report.Daily)
	}
	if report.Daily[0].ByProvider["codex"] != 10 || report.Daily[0].ByProvider["claude"] != 5 {
		t.Fatalf("first day by provider = %+v", report.Daily[0].ByProvider)
	}
	if len(report.Breakdown.ByDay) != len(report.Daily) {
		t.Fatalf("breakdown.by_day = %+v, want the same days as daily", report.Breakdown.ByDay)
	}
	if len(report.Breakdown.ByModel) != 2 || report.Breakdown.ByModel[0].Model != "gpt-6-astra" || report.Breakdown.ByModel[0].Cost != 15 {
		t.Fatalf("breakdown.by_model = %+v", report.Breakdown.ByModel)
	}

	if len(report.Runners) != 2 || report.Runners[0].ID != "rnr_1" {
		t.Fatalf("runners = %+v", report.Runners)
	}
	// One attempt counted once even though it used two models.
	if report.Runners[0].Sessions != 1 || report.Runners[0].BusySeconds != 3600 || report.Runners[0].Cost != 15 {
		t.Fatalf("first runner = %+v, want one session of 3600 seconds at 15", report.Runners[0])
	}
	// 3600 busy seconds of a 172800-second window with a capacity of two.
	if want := reportRound(3600.0 / (172800 * 2)); report.Runners[0].CapacityUsed != want {
		t.Fatalf("capacity_used = %v, want %v", report.Runners[0].CapacityUsed, want)
	}
	if report.Limits["members"].Limit != 10 {
		t.Fatalf("limits = %+v", report.Limits)
	}
	if report.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", report.Currency)
	}
}

// An empty window still answers the whole shape, with arrays rather than
// nulls, so the client never has to guard a missing field.
func TestBuildUsageReportEmpty(t *testing.T) {
	t.Parallel()
	window := Window{From: time.Now().UTC().Add(-24 * time.Hour), To: time.Now().UTC()}
	report := BuildReport[struct{}](window, nil, nil, nil, Config{})
	if report.Providers == nil || report.Daily == nil || report.Runners == nil ||
		report.Breakdown.ByModel == nil || report.Breakdown.ByDay == nil || report.Limits == nil {
		t.Fatalf("empty report has a null collection: %+v", report)
	}
	if report.Total != (Total{}) || report.Totals != (Totals{}) {
		t.Fatalf("empty report has totals: %+v %+v", report.Total, report.Totals)
	}
	if report.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", report.Currency)
	}
}

func TestUsageBucketTimeJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		bucket BucketTime
		want   string
	}{
		{name: "day", bucket: BucketTime{Time: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)}, want: `"2026-09-09"`},
		{name: "day drops a stray clock", bucket: BucketTime{Time: time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC)}, want: `"2026-09-09"`},
		{name: "hour", bucket: BucketTime{Time: time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC), Hourly: true}, want: `"2026-09-09T13:00:00.000Z"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(test.bucket)
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != test.want {
				t.Fatalf("encoded = %s, want %s", encoded, test.want)
			}
			var decoded BucketTime
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Hourly != test.bucket.Hourly {
				t.Fatalf("decoded hourly = %v, want %v", decoded.Hourly, test.bucket.Hourly)
			}
			if !decoded.Hourly && !decoded.Equal(test.bucket.Truncate(24*time.Hour)) {
				t.Fatalf("decoded day = %v, want %v", decoded.Time, test.bucket.Truncate(24*time.Hour))
			}
			if decoded.Hourly && !decoded.Equal(test.bucket.Time) {
				t.Fatalf("decoded hour = %v, want %v", decoded.Time, test.bucket.Time)
			}
		})
	}
	var bad BucketTime
	if err := json.Unmarshal([]byte(`"yesterday"`), &bad); err == nil {
		t.Fatal("expected an error for an unreadable period")
	}
}

func usageTestPrices() Config {
	return Config{Currency: "USD", Prices: map[string]Price{"gpt-6-astra": {Input: 10, CachedInput: 1, Output: 40}, "claude-opus-5": {Input: 20, CachedInput: 2, Output: 100}}}.Normalized()
}
func reportRound(value float64) float64 { return math.Round(value*10000) / 10000 }
