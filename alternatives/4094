package usagereport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/usagecosts"
)

func TestUsageRangeWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 13, 25, 0, 0, time.UTC)
	cases := []struct {
		name    string
		value   string
		span    time.Duration
		wantErr bool
	}{
		{name: "24h", value: "24h", span: 24 * time.Hour},
		{name: "7d", value: "7d", span: 7 * 24 * time.Hour},
		{name: "30d", value: "30d", span: 30 * 24 * time.Hour},
		{name: "90d", value: "90d", span: 90 * 24 * time.Hour},
		{name: "default", value: "", span: 7 * 24 * time.Hour},
		{name: "unknown", value: "1y", wantErr: true},
		{name: "not a range", value: "yesterday", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			window, err := RangeWindow(test.value, now)
			if test.wantErr {
				if err == nil {
					t.Fatalf("usageRangeWindow(%q) = %+v, want an error", test.value, window)
				}
				return
			}
			if err != nil {
				t.Fatalf("usageRangeWindow(%q) = %v, want nil", test.value, err)
			}
			wantFrom := now.Truncate(time.Hour).Add(time.Hour - test.span)
			if !window.To.Equal(now) || !window.From.Equal(wantFrom) {
				t.Fatalf("usageRangeWindow(%q) = %+v, want %v to %v", test.value, window, wantFrom, now)
			}
			if window.Hourly != (test.span <= 24*time.Hour) {
				t.Fatalf("usageRangeWindow(%q) hourly = %t, want %t", test.value, window.Hourly, test.span <= 24*time.Hour)
			}
		})
	}
}

func TestBuildUsageReport(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	window := usagecosts.Window{From: day, To: next.Add(24 * time.Hour)}
	rows := []Row{
		{AttemptID: "attempt_1", Period: day, Provider: "codex", Model: "gpt-6-astra", Input: 1_000_000, CachedInput: 400_000, Output: 100_000, Cost: 10, Currency: "USD", RunnerID: "rnr_1", RunnerName: "Studio", BusySeconds: 3600},
		{AttemptID: "attempt_1", Period: day, Provider: "claude", Model: "claude-opus-5", Input: 200_000, Output: 50_000, Cost: 5, Currency: "USD", RunnerID: "rnr_1", RunnerName: "Studio", BusySeconds: 3600},
		{AttemptID: "attempt_2", Period: next, Provider: "codex", Model: "gpt-6-astra", Input: 500_000, CachedInput: 100_000, Output: 20_000, Cost: 5, Currency: "USD", RunnerID: "rnr_2", RunnerName: "Mini", BusySeconds: 1800},
	}
	limits := map[string]Limit{"members": {Used: 3, Limit: 10}}
	capacity := map[string]RunnerCapacity{"rnr_1": {DisplayName: "Studio", Limit: 2}, "rnr_2": {DisplayName: "Mini", Limit: 1}}
	report := Build(window, rows, limits, capacity, usageTestPrices())

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
	if want := Round(0.5 * 9); report.Totals.CacheSavings != want {
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
	if want := Round(3600.0 / (172800 * 2)); report.Runners[0].CapacityUsed != want {
		t.Fatalf("capacity_used = %v, want %v", report.Runners[0].CapacityUsed, want)
	}
	if report.Limits["members"].Limit != 10 {
		t.Fatalf("limits = %+v", report.Limits)
	}
	if report.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", report.Currency)
	}
}

func TestBuildUsageReportEmpty(t *testing.T) {
	t.Parallel()
	window := usagecosts.Window{From: time.Now().UTC().Add(-24 * time.Hour), To: time.Now().UTC()}
	report := Build(window, nil, nil, nil, usagecosts.Prices{})
	if report.Providers == nil || report.Daily == nil || report.Runners == nil ||
		report.Breakdown.ByModel == nil || report.Breakdown.ByDay == nil || report.Limits == nil {
		t.Fatalf("empty report has a null collection: %+v", report)
	}
	if report.Total != (usageTotal{}) || report.Totals != (usageTotals{}) {
		t.Fatalf("empty report has totals: %+v %+v", report.Total, report.Totals)
	}
	if report.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", report.Currency)
	}
}

func TestUsageReportGoldenShape(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "usage-report.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	var want any
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("decode golden: %v", err)
	}
	encoded, err := json.Marshal(usageFixtureValue(t, raw))
	if err != nil {
		t.Fatalf("encode report: %v", err)
	}
	var got any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	compareUsageShape(t, "", want, got)
}

func TestUsageBucketTimeJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		bucket usageBucketTime
		want   string
	}{
		{name: "day", bucket: usageBucketTime{Time: time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)}, want: `"2026-09-09"`},
		{name: "day drops a stray clock", bucket: usageBucketTime{Time: time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC)}, want: `"2026-09-09"`},
		{name: "hour", bucket: usageBucketTime{Time: time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC), Hourly: true}, want: `"2026-09-09T13:00:00.000Z"`},
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
			var decoded usageBucketTime
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
	var bad usageBucketTime
	if err := json.Unmarshal([]byte(`"yesterday"`), &bad); err == nil {
		t.Fatal("expected an error for an unreadable period")
	}
}

func TestBuildUsageReportBuckets(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 13, 25, 0, 0, time.UTC)
	rows := []Row{
		{AttemptID: "a1", Period: time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC), Provider: "codex", Model: "gpt-6-astra", Input: 10, Cost: 1, Currency: "usd"},
		{AttemptID: "a1", Period: time.Date(2026, 9, 10, 2, 0, 0, 0, time.UTC), Provider: "codex", Model: "gpt-6-astra", Input: 10, Cost: 2, Currency: "USD"},
		{AttemptID: "a2", Period: time.Date(2026, 9, 10, 2, 0, 0, 0, time.UTC), Provider: "codex", Model: "gpt-6-astra", Input: 10, Cost: 5, Currency: "EUR"},
	}
	cases := []struct {
		name       string
		rangeName  string
		wantDays   []string
		wantHourly bool
	}{
		{name: "24h is hourly", rangeName: "24h", wantDays: []string{`"2026-09-10T01:00:00.000Z"`, `"2026-09-10T02:00:00.000Z"`}, wantHourly: true},
		{name: "7d is daily", rangeName: "7d", wantDays: []string{`"2026-09-10"`}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			window, err := RangeWindow(test.rangeName, now)
			if err != nil {
				t.Fatal(err)
			}
			report := Build(window, rows, nil, nil, usageTestPrices())
			if len(report.Daily) != len(test.wantDays) {
				t.Fatalf("daily = %+v, want %d buckets", report.Daily, len(test.wantDays))
			}
			for i, want := range test.wantDays {
				encoded, err := json.Marshal(report.Daily[i].Day)
				if err != nil {
					t.Fatal(err)
				}
				if string(encoded) != want || report.Daily[i].Day.Hourly != test.wantHourly {
					t.Fatalf("bucket %d = %s, want %s", i, encoded, want)
				}
			}
			// The EUR row keeps its tokens but its cost is not added to a USD
			// report.
			if report.Currency != "USD" || report.Total.Cost != 3 || report.Total.Tokens != 30 {
				t.Fatalf("total = %+v %s, want 3 USD across 30 tokens", report.Total, report.Currency)
			}
		})
	}
}

func TestUsageBusySecondsClampsToWindow(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	window := usagecosts.Window{From: from, To: from.Add(24 * time.Hour)}
	stamp := func(offset time.Duration) string { return formatHubTime(from.Add(offset)) }
	cases := []struct {
		name             string
		started, updated string
		want             int64
	}{
		{name: "inside", started: stamp(time.Hour), updated: stamp(2 * time.Hour), want: 3600},
		{name: "started before the window", started: stamp(-48 * time.Hour), updated: stamp(time.Hour), want: 3600},
		{name: "ran past the window", started: stamp(23 * time.Hour), updated: stamp(30 * time.Hour), want: 3600},
		{name: "spans the whole window", started: stamp(-time.Hour), updated: stamp(48 * time.Hour), want: 24 * 3600},
		{name: "wholly before the window", started: stamp(-3 * time.Hour), updated: stamp(-2 * time.Hour), want: 0},
		{name: "unreadable", started: "yesterday", updated: stamp(time.Hour), want: 0},
		{name: "missing", started: "", updated: stamp(time.Hour), want: 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := BusySeconds(test.started, test.updated, window); got != test.want {
				t.Fatalf("usageBusySeconds() = %d, want %d", got, test.want)
			}
		})
	}
}

func usageTestPrices() usagecosts.Prices {
	return usagecosts.Prices{Currency: "USD", Prices: map[string]usagecosts.Price{
		"gpt-6-astra":   {Input: 10, CachedInput: 1, Output: 40},
		"claude-opus-5": {Input: 20, CachedInput: 2, Output: 100},
	}}.Normalized()
}

func usageFixtureValue(t *testing.T, raw []byte) Report {
	t.Helper()
	var shape struct {
		Providers []json.RawMessage `json:"providers"`
		Daily     []json.RawMessage `json:"daily"`
		Breakdown struct {
			ByModel []json.RawMessage `json:"by_model"`
			ByDay   []json.RawMessage `json:"by_day"`
		} `json:"breakdown"`
		Limits  map[string]json.RawMessage `json:"limits"`
		Runners []json.RawMessage          `json:"runners"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatalf("decode usage fixture: %v", err)
	}
	from := time.Date(2026, 9, 9, 13, 0, 0, 0, time.UTC)
	report := Report{
		Chat:      ChatSummary{Range: usagecosts.Window{From: from, To: from.Add(24 * time.Hour)}},
		Range:     usagecosts.Window{From: from, To: from.Add(24 * time.Hour)},
		Total:     usageTotal{Cost: 2.14, Tokens: 4141159, Sessions: 5},
		Providers: []usageProvider{},
		Daily:     []usageDay{},
		Totals:    usageTotals{Processed: 4141159, CachedInput: 3869016, UncachedInput: 177057, Output: 95086, CacheSavings: 6.56},
		Breakdown: usageBreakdown{ByModel: []usageModel{}, ByDay: []usageDay{}},
		Limits:    map[string]Limit{},
		Runners:   []usageRunner{},
		Currency:  "USD",
	}
	for i := range shape.Providers {
		report.Providers = append(report.Providers, usageProvider{
			ID: "codex", Label: "Codex", Sessions: int64(i + 1), Cost: 0.83, Share: 0.3871, Tokens: 2581421,
		})
	}
	day := func(i int) usageDay {
		return usageDay{Day: usageBucketTime{Time: from.Add(time.Duration(i) * time.Hour), Hourly: true}, Cost: 0.07, Tokens: 137061, ByProvider: map[string]float64{"codex": 0.03, "claude": 0.04}}
	}
	for i := range shape.Daily {
		report.Daily = append(report.Daily, day(i))
	}
	for i := range shape.Breakdown.ByDay {
		report.Breakdown.ByDay = append(report.Breakdown.ByDay, day(i))
	}
	for range shape.Breakdown.ByModel {
		report.Breakdown.ByModel = append(report.Breakdown.ByModel, usageModel{
			Model: "gpt-6-astra", Provider: "codex", Cost: 0.7, Share: 0.3269, Tokens: 1746029,
		})
	}
	for name := range shape.Limits {
		report.Limits[name] = Limit{Used: 3, Limit: 10}
	}
	for i := range shape.Runners {
		report.Runners = append(report.Runners, usageRunner{
			ID: "rnr_mock", DisplayName: "Mock MacBook Pro", Sessions: int64(i + 1),
			Tokens: 2981634, Cost: 1.54, BusySeconds: 19284, CapacityUsed: 0.45,
		})
	}
	return report
}

func compareUsageShape(t *testing.T, path string, want, got any) {
	t.Helper()
	label := path
	if label == "" {
		label = "(root)"
	}
	if (want == nil) != (got == nil) {
		t.Errorf("%s: golden null = %t, Go null = %t", label, want == nil, got == nil)
		return
	}
	switch wanted := want.(type) {
	case nil:
	case map[string]any:
		gotten, ok := got.(map[string]any)
		if !ok {
			t.Errorf("%s: golden is an object, Go value is %T", label, got)
			return
		}
		for key, value := range wanted {
			other, present := gotten[key]
			if !present {
				t.Errorf("%s: Go value is missing key %q", label, key)
				continue
			}
			compareUsageShape(t, path+"."+key, value, other)
		}
		for key := range gotten {
			if _, present := wanted[key]; !present {
				t.Errorf("%s: Go value has extra key %q", label, key)
			}
		}
	case []any:
		gotten, ok := got.([]any)
		if !ok {
			t.Errorf("%s: golden is an array, Go value is %T", label, got)
			return
		}
		if len(wanted) != len(gotten) {
			t.Errorf("%s: golden has %d elements, Go value has %d", label, len(wanted), len(gotten))
			return
		}
		for i := range wanted {
			compareUsageShape(t, path+"[]", wanted[i], gotten[i])
		}
	default:
		if fmt.Sprintf("%T", want) != fmt.Sprintf("%T", got) {
			t.Errorf("%s: golden is %T, Go value is %T", label, want, got)
		}
	}
}

func formatHubTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}
