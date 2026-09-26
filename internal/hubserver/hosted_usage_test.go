package hubserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// The usage report (decisions section 17.5): what runners report is stored
// per attempt, day, provider and model, and the report folds it into the
// shape the client reads.

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
			window, err := usageRangeWindow(test.value, now)
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

func usageTestPrices() UsageConfig {
	return UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{
		"gpt-6-astra":   {Input: 10, CachedInput: 1, Output: 40},
		"claude-opus-5": {Input: 20, CachedInput: 2, Output: 100},
	}}.normalized()
}

func TestUsageConfigEstimate(t *testing.T) {
	t.Parallel()
	prices := usageTestPrices()
	cases := []struct {
		name                  string
		model                 string
		input, cached, output int64
		wantCost              float64
		wantFound             bool
		wantSaving            float64
	}{
		{
			name: "uncached input, cached input and output", model: "gpt-6-astra",
			input: 1_000_000, cached: 400_000, output: 100_000,
			// 600k uncached at 10, 400k cached at 1, 100k output at 40.
			wantCost: 6 + 0.4 + 4, wantFound: true, wantSaving: 0.4 * 9,
		},
		{name: "case insensitive", model: "GPT-6-Astra", input: 1_000_000, wantCost: 10, wantFound: true},
		{name: "unknown model", model: "gpt-7", input: 1_000_000},
		{
			name: "cached exceeds input", model: "gpt-6-astra", input: 100, cached: 400,
			// Savings cap cached input at the input total, as pricing does.
			wantCost: 100.0 / 1_000_000 * 1, wantFound: true, wantSaving: 100.0 / 1_000_000 * 9,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cost, currency, found := prices.estimate(test.model, test.input, test.cached, test.output)
			if found != test.wantFound {
				t.Fatalf("estimate(%q) found = %t, want %t", test.model, found, test.wantFound)
			}
			if !found {
				return
			}
			if currency != "USD" {
				t.Fatalf("estimate(%q) currency = %q, want USD", test.model, currency)
			}
			if diff := cost - test.wantCost; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("estimate(%q) = %v, want %v", test.model, cost, test.wantCost)
			}
			if saving := prices.cacheSaving(test.model, test.input, test.cached); saving-test.wantSaving > 1e-9 || saving-test.wantSaving < -1e-9 {
				t.Fatalf("cacheSaving(%q, %d) = %v, want %v", test.model, test.cached, saving, test.wantSaving)
			}
		})
	}
	if saving := prices.cacheSaving("gpt-7", 1_000_000, 1_000_000); saving != 0 {
		t.Fatalf("cacheSaving for an unpriced model = %v, want 0", saving)
	}
}

func TestBuildUsageReport(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	window := usageWindow{From: day, To: next.Add(24 * time.Hour)}
	rows := []usageRow{
		{AttemptID: "attempt_1", Period: day, Provider: "codex", Model: "gpt-6-astra", Input: 1_000_000, CachedInput: 400_000, Output: 100_000, Cost: 10, Currency: "USD", RunnerID: "rnr_1", RunnerName: "Studio", BusySeconds: 3600},
		{AttemptID: "attempt_1", Period: day, Provider: "claude", Model: "claude-opus-5", Input: 200_000, Output: 50_000, Cost: 5, Currency: "USD", RunnerID: "rnr_1", RunnerName: "Studio", BusySeconds: 3600},
		{AttemptID: "attempt_2", Period: next, Provider: "codex", Model: "gpt-6-astra", Input: 500_000, CachedInput: 100_000, Output: 20_000, Cost: 5, Currency: "USD", RunnerID: "rnr_2", RunnerName: "Mini", BusySeconds: 1800},
	}
	limits := map[string]usageLimit{"members": {Used: 3, Limit: 10}}
	capacity := map[string]runnerCapacity{"rnr_1": {DisplayName: "Studio", Limit: 2}, "rnr_2": {DisplayName: "Mini", Limit: 1}}
	report := buildUsageReport(window, rows, limits, capacity, usageTestPrices())

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
	if want := usageRound(0.5 * 9); report.Totals.CacheSavings != want {
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
	if want := usageRound(3600.0 / (172800 * 2)); report.Runners[0].CapacityUsed != want {
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
	window := usageWindow{From: time.Now().UTC().Add(-24 * time.Hour), To: time.Now().UTC()}
	report := buildUsageReport(window, nil, nil, nil, UsageConfig{})
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

// A run event stores the attempt's running total. The event carries the
// total, not a delta, so a second event replaces the rows instead of adding
// to them.
func TestNativeRunEventRecordsUsage(t *testing.T) {
	t.Parallel()
	prices := usageTestPrices()
	f := newNativeFixture(t, openTestService(t, Config{DatabasePath: t.TempDir() + "/hub.db", Usage: &prices}), "", "usage")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	start := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)

	checkpoint := start
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "checkpoint", 2
	checkpoint.Data.Handoff = nativeTestCheckpoint()
	checkpoint.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "gpt-6-astra", Input: 1000, CachedInput: 400, Output: 100, CostEstimate: 0.25, Currency: "USD"}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)

	type row struct {
		provider, model, currency string
		input, cached, output     int64
		cost                      float64
	}
	read := func() []row {
		t.Helper()
		result, err := f.service.database.db.QueryContext(t.Context(),
			"SELECT provider, model, sum(input), sum(cached_input), sum(output), sum(cost_estimate), max(currency) FROM attempt_usage WHERE attempt_id = ? GROUP BY provider, model ORDER BY provider, model", start.Data.AttemptID)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = result.Close() }()
		var rows []row
		for result.Next() {
			var got row
			if err := result.Scan(&got.provider, &got.model, &got.input, &got.cached, &got.output, &got.cost, &got.currency); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, got)
		}
		if err := result.Err(); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	stored := read()
	if len(stored) != 1 || stored[0] != (row{provider: "codex", model: "gpt-6-astra", input: 1000, cached: 400, output: 100, cost: 0.25, currency: "USD"}) {
		t.Fatalf("stored usage = %+v", stored)
	}

	// The finish carries the attempt's running total, which replaces the row.
	finish := checkpoint
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 3, "succeeded"
	finish.Data.Handoff = nil
	finish.Data.Usage = []tracker.NativeUsage{
		{Provider: "codex", Model: "gpt-6-astra", Input: 3000, CachedInput: 1200, Output: 300, CostEstimate: 0.75, Currency: "USD"},
		// A model the runner could not price is priced by the hub table:
		// 200 uncached input at 20 and 50 output at 100 per million.
		{Provider: "claude_code", Model: "claude-opus-5", Input: 200, Output: 50},
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
	stored = read()
	if len(stored) != 2 {
		t.Fatalf("stored usage = %+v, want two rows", stored)
	}
	if stored[0].provider != "claude" || stored[0].model != "claude-opus-5" {
		t.Fatalf("hub-priced row = %+v, want the normalized claude provider", stored[0])
	}
	if want := 200.0/1_000_000*20 + 50.0/1_000_000*100; stored[0].cost-want > 1e-9 || want-stored[0].cost > 1e-9 {
		t.Fatalf("hub-priced cost = %v, want %v", stored[0].cost, want)
	}
	if stored[1].input != 3000 || stored[1].cost != 0.75 {
		t.Fatalf("replaced row = %+v, want the running total, not a sum", stored[1])
	}
}

func TestNativeRunEventRejectsInvalidUsage(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "usage-invalid")
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "work")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	start := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
	base := start
	base.Type, base.Data.Sequence = "run.checkpointed", 2
	base.Data.Handoff = nativeTestCheckpoint()

	many := make([]tracker.NativeUsage, 0, maxNativeUsageEntries+1)
	for i := range maxNativeUsageEntries + 1 {
		many = append(many, tracker.NativeUsage{Provider: "codex", Model: "model-" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Input: 1})
	}
	cases := []struct {
		name    string
		usage   []tracker.NativeUsage
		onStart bool
		status  int
	}{
		{name: "no provider", usage: []tracker.NativeUsage{{Model: "gpt-6-astra", Input: 1}}, status: http.StatusUnprocessableEntity},
		{name: "no model", usage: []tracker.NativeUsage{{Provider: "codex", Input: 1}}, status: http.StatusUnprocessableEntity},
		{name: "negative tokens", usage: []tracker.NativeUsage{{Provider: "codex", Model: "gpt-6-astra", Input: -1}}, status: http.StatusUnprocessableEntity},
		{name: "negative cost", usage: []tracker.NativeUsage{{Provider: "codex", Model: "gpt-6-astra", CostEstimate: -1}}, status: http.StatusUnprocessableEntity},
		{name: "repeated model", usage: []tracker.NativeUsage{{Provider: "codex", Model: "gpt-6-astra", Input: 1}, {Provider: "codex", Model: "gpt-6-astra", Input: 2}}, status: http.StatusUnprocessableEntity},
		{name: "too many entries", usage: many, status: http.StatusUnprocessableEntity},
		{name: "usage on a started run", usage: []tracker.NativeUsage{{Provider: "codex", Model: "gpt-6-astra", Input: 1}}, onStart: true, status: http.StatusUnprocessableEntity},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			event := base
			if test.onStart {
				event = start
				event.Data.Handoff = nil
			}
			event.IdempotencyKey = "usage-" + test.name
			event.Data.Usage = test.usage
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, event), test.status)
		})
	}
}

// GET /usage answers every member, refuses staff, validates the range and
// keeps a project the member cannot read opaque.
func TestHostedUsageReportAccess(t *testing.T) {
	t.Parallel()
	f := newUsageHostedFixture(t)
	for _, account := range []string{"owner", "viewer"} {
		t.Run(account, func(t *testing.T) {
			var report usageReport
			usageDecode(t, f.usage(t, account, browserHostedOrganizationBase+"/usage?range=30d", http.StatusOK), &report)
			if report.Providers == nil || report.Daily == nil || report.Runners == nil || report.Limits == nil {
				t.Fatalf("report has a null collection: %+v", report)
			}
			if span := report.Range.To.Sub(report.Range.From); span <= 29*24*time.Hour || span > 30*24*time.Hour {
				t.Fatalf("range = %+v, want 30 days of hours", report.Range)
			}
		})
	}
	f.usage(t, "staff", browserHostedOrganizationBase+"/usage", http.StatusForbidden)
	f.usage(t, "owner", browserHostedOrganizationBase+"/usage?range=1y", http.StatusUnprocessableEntity)
	f.usage(t, "owner", browserHostedOrganizationBase+"/usage?project=proj_missing", http.StatusNotFound)
	f.usage(t, "", browserHostedOrganizationBase+"/usage", http.StatusUnauthorized)
}

// The endpoint aggregates the rows the store holds: the seeded days fold
// into the hero total, the provider rows, the daily series and both
// breakdowns, and the project filter narrows to what a member can read
// (decisions section 17.5).
func TestHostedUsageReportAggregatesStoredAttempts(t *testing.T) {
	t.Parallel()
	f := newUsageHostedFixture(t)
	var report usageReport
	usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/usage?range=90d", http.StatusOK), &report)

	prices := f.service.usagePrices()
	var wantCost float64
	var wantTokens, wantCached, wantUncached, wantOutput int64
	var wantSavings float64
	for _, entry := range browserUsageEntries {
		cost, _, found := prices.estimate(entry.Model, entry.Input, entry.Cached, entry.Output)
		if !found {
			t.Fatalf("price table does not price %s", entry.Model)
		}
		wantCost += cost * browserUsageDays
		wantTokens += (entry.Input + entry.Output) * browserUsageDays
		wantCached += entry.Cached * browserUsageDays
		wantUncached += (entry.Input - entry.Cached) * browserUsageDays
		wantOutput += entry.Output * browserUsageDays
		wantSavings += prices.cacheSaving(entry.Model, entry.Input, entry.Cached) * browserUsageDays
	}
	// Money is compared within a rounding step: the report sums the stored
	// rows one at a time, so the last decimal depends on the order.
	near := func(got, want float64) bool { return got-want < 1e-4 && want-got < 1e-4 }
	if !near(report.Total.Cost, wantCost) || report.Total.Tokens != wantTokens {
		t.Fatalf("total = %+v, want %v across %d tokens", report.Total, usageRound(wantCost), wantTokens)
	}
	// One attempt per day per entry, each counted once.
	if want := int64(browserUsageDays * len(browserUsageEntries)); report.Total.Sessions != want {
		t.Fatalf("total.sessions = %d, want %d", report.Total.Sessions, want)
	}
	if report.Totals.Processed != wantTokens || report.Totals.CachedInput != wantCached ||
		report.Totals.UncachedInput != wantUncached || report.Totals.Output != wantOutput {
		t.Fatalf("totals = %+v, want %d/%d/%d/%d", report.Totals, wantTokens, wantCached, wantUncached, wantOutput)
	}
	if !near(report.Totals.CacheSavings, wantSavings) {
		t.Fatalf("totals.cache_savings = %v, want %v", report.Totals.CacheSavings, usageRound(wantSavings))
	}
	if len(report.Providers) != len(browserUsageEntries) || len(report.Breakdown.ByModel) != len(browserUsageEntries) {
		t.Fatalf("providers = %+v, models = %+v", report.Providers, report.Breakdown.ByModel)
	}
	var share float64
	for _, provider := range report.Providers {
		share += provider.Share
	}
	if share < 0.999 || share > 1.001 {
		t.Fatalf("provider shares add up to %v, want 1", share)
	}
	// The enrolled runner ran every seeded attempt and was busy for each.
	if len(report.Runners) != 1 {
		t.Fatalf("runners = %+v, want the one enrolled runner", report.Runners)
	}
	runner := report.Runners[0]
	if runner.DisplayName != "Preview runner" || runner.Sessions != int64(browserUsageDays*len(browserUsageEntries)) {
		t.Fatalf("runner = %+v, want Preview runner with %d sessions", runner, browserUsageDays*len(browserUsageEntries))
	}
	if want := int64(browserUsageBusy.Seconds()) * runner.Sessions; runner.BusySeconds != want {
		t.Fatalf("runner.busy_seconds = %d, want %d", runner.BusySeconds, want)
	}
	if runner.CapacityUsed <= 0 || runner.CapacityUsed > 1 {
		t.Fatalf("runner.capacity_used = %v, want a share of the window", runner.CapacityUsed)
	}
	if !near(runner.Cost, wantCost) || runner.Tokens != wantTokens {
		t.Fatalf("runner totals = %+v, want %v across %d tokens", runner, usageRound(wantCost), wantTokens)
	}
	if len(report.Daily) != browserUsageDays || len(report.Breakdown.ByDay) != browserUsageDays {
		t.Fatalf("daily = %+v, breakdown.by_day = %+v, want %d days", report.Daily, report.Breakdown.ByDay, browserUsageDays)
	}
	for i := 1; i < len(report.Daily); i++ {
		if !report.Daily[i-1].Day.Before(report.Daily[i].Day.Time) {
			t.Fatalf("daily is not ascending: %+v", report.Daily)
		}
	}
	if report.Currency != "USD" {
		t.Fatalf("currency = %q, want USD", report.Currency)
	}
	// The seeded rows belong to the shared project; the owner's private
	// project spent nothing, and a member without a grant sees neither.
	var narrowed usageReport
	usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/usage?range=90d&project="+f.privateProject, http.StatusOK), &narrowed)
	if narrowed.Total.Cost != 0 || narrowed.Total.Tokens != 0 || len(narrowed.Providers) != 0 {
		t.Fatalf("private project usage = %+v, want an empty report", narrowed.Total)
	}
	f.usage(t, "viewer", browserHostedOrganizationBase+"/usage?range=90d&project="+f.privateProject, http.StatusNotFound)
}

// usageFixtureValue is the Go value one shared usage fixture is compared
// against. The contract is the field set, not the example data, so the value
// is filled to the fixture's own cardinality: every list gets as many fully
// populated entries as the fixture shows and Limits gets the same allowance
// names. A field the hub adds, drops or renames still fails here; a fixture
// the client reshapes with different example data does not.
func usageFixtureValue(t *testing.T, raw []byte) usageReport {
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
	report := usageReport{
		Range:     usageWindow{From: from, To: from.Add(24 * time.Hour)},
		Total:     usageTotal{Cost: 2.14, Tokens: 4141159, Sessions: 5},
		Providers: []usageProvider{},
		Daily:     []usageDay{},
		Totals:    usageTotals{Processed: 4141159, CachedInput: 3869016, UncachedInput: 177057, Output: 95086, CacheSavings: 6.56},
		Breakdown: usageBreakdown{ByModel: []usageModel{}, ByDay: []usageDay{}},
		Limits:    map[string]usageLimit{},
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
		report.Limits[name] = usageLimit{Used: 3, Limit: 10}
	}
	for i := range shape.Runners {
		report.Runners = append(report.Runners, usageRunner{
			ID: "rnr_mock", DisplayName: "Mock MacBook Pro", Sessions: int64(i + 1),
			Tokens: 2981634, Cost: 1.54, BusySeconds: 19284, CapacityUsed: 0.45,
		})
	}
	return report
}

// TestUsageReportGoldenShape pins the section 17.5 shape on the Go side, so
// the hub still has a contract when the shared fixtures are absent.
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

// A day bucket is written as the day alone and an hourly bucket keeps its
// clock, which is how the client tells the two series apart.
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

const browserHostedOrganizationBase = "/api/v2/organizations/org_browser_preview"

// browserUsageDays is how many days of usage the fixture seeds, and
// browserUsageEntries what one of those days spent. Both are named so an
// assertion can compute the report it expects rather than restate it.
const browserUsageDays = 3

var browserUsageEntries = []struct {
	Provider, Model       string
	Input, Cached, Output int64
}{
	{Provider: "codex", Model: "gpt-6-astra", Input: 820_000, Cached: 610_000, Output: 41_000},
	{Provider: "claude", Model: "claude-opus-5", Input: 310_000, Cached: 180_000, Output: 22_500},
}

// browserUsageBusy is how long each seeded attempt held its runner.
const browserUsageBusy = 25 * time.Minute

type usageHostedFixture struct {
	*browserHostedFixture
}

// newUsageHostedFixture is the allocated browser fixture with the hub price
// table installed and three days of usage seeded against its shared project.
func newUsageHostedFixture(t *testing.T) usageHostedFixture {
	t.Helper()
	f := usageHostedFixture{newBrowserHostedFixture(t, true)}
	prices := UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{
		"gpt-6-astra":   {Input: 1.25, CachedInput: 0.125, Output: 10},
		"claude-opus-5": {Input: 5, CachedInput: 0.5, Output: 25},
	}}.normalized()
	f.service.config.Usage = &prices
	f.seedUsage(t)
	return f
}

func (f usageHostedFixture) usage(t *testing.T, account, path string, status int) *httptest.ResponseRecorder {
	t.Helper()
	response := f.page(t, account, path)
	browserHostedStatus(t, response, status)
	return response
}

func usageDecode(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
}

// seedUsage fills attempt_usage so the report has rows to fold: three days
// across two providers and two models, priced by the hub's own table. The
// rows are written straight to the store rather than through a run event,
// because the report only reads what a runner already reported. One runner
// is enrolled and every seeded attempt holds a released lease on its
// machine, so the runner rows have a host, sessions and busy time.
func (f usageHostedFixture) seedUsage(t *testing.T) {
	t.Helper()
	now := f.service.config.now().UTC()
	prices := f.service.usagePrices()
	machine := f.enrollUsageRunner(t)
	var issueRow int64
	var nativeID string
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT id, native_id FROM issues WHERE organization_id = ? AND project_id = ? ORDER BY id LIMIT 1",
		"org_browser_preview", f.project).Scan(&issueRow, &nativeID); err != nil {
		t.Fatalf("the browser fixture has no issue to seed usage against: %v", err)
	}
	for day := range browserUsageDays {
		stamp := now.AddDate(0, 0, -day)
		for index, entry := range browserUsageEntries {
			cost, _, found := prices.estimate(entry.Model, entry.Input, entry.Cached, entry.Output)
			if !found {
				t.Fatalf("the fixture price table does not price %s", entry.Model)
			}
			attemptID := fmt.Sprintf("attempt_browser_usage_%d_%d", day, index)
			leaseID := fmt.Sprintf("lease_browser_usage_%d_%d", day, index)
			started := stamp.Add(-browserUsageBusy)
			lease, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases
 (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				leaseID, issueRow, machine, "session_"+leaseID, formatHubTime(stamp), formatHubTime(started),
				formatHubTime(stamp), formatHubTime(stamp), formatHubTime(started), formatHubTime(stamp))
			if err != nil {
				t.Fatalf("seed usage lease: %v", err)
			}
			fencing, err := lease.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO lease_runners (lease_id, runner_id) VALUES (?, ?)", leaseID, "runner_browser_preview"); err != nil {
				t.Fatalf("seed usage lease runner: %v", err)
			}
			data, err := json.Marshal(tracker.NativeRunData{RunID: "run_" + attemptID, AttemptID: attemptID, FencingToken: tracker.FencingToken(fencing), LeaseID: tracker.LeaseID(leaseID), MachineID: tracker.MachineID(machine), Outcome: "succeeded"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts
 (id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, started_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, 1, 'succeeded', ?, ?, ?)`,
				attemptID, "org_browser_preview", f.project, nativeID, leaseID, fencing, "run_"+attemptID,
				string(data), formatHubTime(started), formatHubTime(stamp)); err != nil {
				t.Fatalf("seed usage attempt: %v", err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attempt_usage
 (attempt_id, organization_id, project_id, period, provider, model, input, cached_input, output, cost_estimate, currency, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'USD', ?)`,
				attemptID, "org_browser_preview", f.project,
				usagePeriod(stamp), entry.Provider, entry.Model,
				entry.Input, entry.Cached, entry.Output, cost, formatHubTime(stamp)); err != nil {
				t.Fatalf("seed usage: %v", err)
			}
		}
	}
}

// enrollUsageRunner registers one runner in the fixture's organization and
// returns its machine id, which is what a lease names. The rows are written
// directly: enrollment is an instance-admin operation the hosted boundary
// does not expose, and no runner process joins this fixture.
func (f usageHostedFixture) enrollUsageRunner(t *testing.T) string {
	t.Helper()
	now := formatHubTime(f.service.config.now().UTC())
	later := formatHubTime(f.service.config.now().UTC().Add(time.Hour))
	const (
		machine    = "machine_browser_preview_runner"
		token      = "tok_browser_preview_runner"
		enrollment = "enr_browser_preview_runner"
		identity   = "runner_browser_preview"
	)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO api_tokens (id, name, token_hash, token_fingerprint, scope, created_at, updated_at, expires_at, native_only) VALUES (?, ?, ?, ?, 'worker', ?, ?, ?, 1)`,
			[]any{token, "Preview runner", strings.Repeat("a", 64), "preview", now, now, later}},
		{`INSERT INTO machines (id, hostname, display_name, capacity, version, last_heartbeat_at, registered_at, updated_at, organization_id, token_id) VALUES (?, ?, ?, 2, 'preview', ?, ?, ?, ?, ?)`,
			[]any{machine, "preview-host", "Preview runner", now, now, now, "org_browser_preview", token}},
		{`INSERT INTO runner_enrollments (id, organization_id, runner_id, machine_id, token_hash, operations_json, created_at, expires_at, created_by, redeemed_at) VALUES (?, ?, ?, ?, ?, '["claim"]', ?, ?, ?, ?)`,
			[]any{enrollment, "org_browser_preview", identity, machine, strings.Repeat("b", 64), now, later, token, now}},
		{`INSERT INTO runner_identities (id, organization_id, machine_id, token_id, enrollment_id, operations_json, created_at, display_name, capacity_limit, reported_capacity, os, architecture, last_heartbeat_at) VALUES (?, ?, ?, ?, ?, '["claim"]', ?, ?, 2, 2, 'linux', 'arm64', ?)`,
			[]any{identity, "org_browser_preview", machine, token, enrollment, now, "Preview runner", now}},
		// A second runner on the same machine ran none of the seeded
		// attempts: the report must not repeat their usage for it.
		{`INSERT INTO api_tokens (id, name, token_hash, token_fingerprint, scope, created_at, updated_at, expires_at, native_only) VALUES (?, ?, ?, ?, 'worker', ?, ?, ?, 1)`,
			[]any{token + "_shared", "Shared runner", strings.Repeat("c", 64), "shared", now, now, later}},
		{`INSERT INTO runner_enrollments (id, organization_id, runner_id, machine_id, token_hash, operations_json, created_at, expires_at, created_by, redeemed_at) VALUES (?, ?, ?, ?, ?, '["claim"]', ?, ?, ?, ?)`,
			[]any{enrollment + "_shared", "org_browser_preview", identity + "_shared", machine, strings.Repeat("d", 64), now, later, token, now}},
		{`INSERT INTO runner_identities (id, organization_id, machine_id, token_id, enrollment_id, operations_json, created_at, display_name, capacity_limit, reported_capacity, os, architecture, last_heartbeat_at) VALUES (?, ?, ?, ?, ?, '["claim"]', ?, ?, 2, 2, 'linux', 'arm64', ?)`,
			[]any{identity + "_shared", "org_browser_preview", machine, token + "_shared", enrollment + "_shared", now, "Shared runner", now}},
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatalf("seed the preview runner: %v", err)
		}
	}
	return machine
}

// compareUsageShape asserts that the Go payload carries exactly the golden's
// key set at every level and agrees on whether a value is null. Values are
// not compared: the golden is an example, the shape is the contract.
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

func TestUsageDelta(t *testing.T) {
	t.Parallel()
	prices := usageTestPrices()
	cases := []struct {
		name     string
		total    tracker.NativeUsage
		recorded tracker.NativeUsage
		want     tracker.NativeUsage
	}{
		{
			name:  "first report",
			total: tracker.NativeUsage{Model: "gpt-6-astra", Input: 1000, CachedInput: 400, Output: 100, CostEstimate: 0.25, Currency: "USD"},
			want:  tracker.NativeUsage{Input: 1000, CachedInput: 400, Output: 100, CostEstimate: 0.25},
		},
		{
			name:     "increment over recorded",
			total:    tracker.NativeUsage{Model: "gpt-6-astra", Input: 3000, CachedInput: 1200, Output: 300, CostEstimate: 0.75, Currency: "USD"},
			recorded: tracker.NativeUsage{Input: 1000, CachedInput: 400, Output: 100, CostEstimate: 0.25},
			want:     tracker.NativeUsage{Input: 2000, CachedInput: 800, Output: 200, CostEstimate: 0.5},
		},
		{
			name:     "redelivered total adds nothing",
			total:    tracker.NativeUsage{Model: "gpt-6-astra", Input: 1000, Output: 100, CostEstimate: 0.25, Currency: "USD"},
			recorded: tracker.NativeUsage{Input: 1000, Output: 100, CostEstimate: 0.25},
		},
		{
			name:     "total that went backwards is not debt",
			total:    tracker.NativeUsage{Model: "gpt-6-astra", Input: 500, Output: 50},
			recorded: tracker.NativeUsage{Input: 1000, Output: 100, CostEstimate: 0.25},
		},
		{
			name:     "unpriced increment priced by the hub",
			total:    tracker.NativeUsage{Model: "claude-opus-5", Input: 3_000_000, Output: 1_000_000},
			recorded: tracker.NativeUsage{Input: 2_000_000},
			want:     tracker.NativeUsage{Input: 1_000_000, Output: 1_000_000, CostEstimate: 20 + 100},
		},
		{
			name:  "foreign currency repriced by the hub",
			total: tracker.NativeUsage{Model: "gpt-6-astra", Input: 1_000_000, CostEstimate: 99, Currency: "EUR"},
			want:  tracker.NativeUsage{Input: 1_000_000, CostEstimate: 10},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := usageDelta(test.total, test.recorded, prices)
			if diff := got.CostEstimate - test.want.CostEstimate; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("usageDelta() cost = %v, want %v", got.CostEstimate, test.want.CostEstimate)
			}
			got.CostEstimate = test.want.CostEstimate
			if got != test.want {
				t.Fatalf("usageDelta() = %+v, want %+v", got, test.want)
			}
		})
	}
}

// Spend reported after midnight on an attempt that started the day before is
// filed under the hour it arrived, not the attempt's start.
func TestRecordAttemptUsageAttributesIncrementsToTheirHour(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: t.TempDir() + "/hub.db"})
	prices := usageTestPrices()
	scope := nativeScope{organization: "org_usage", project: "prj_usage"}
	evening := time.Date(2026, 9, 9, 23, 40, 0, 0, time.UTC)
	morning := time.Date(2026, 9, 10, 0, 20, 0, 0, time.UTC)
	reports := []struct {
		at    time.Time
		total tracker.NativeUsage
	}{
		{at: evening, total: tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 1000, Output: 100, CostEstimate: 0.25, Currency: "USD"}},
		{at: morning, total: tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 3000, Output: 300, CostEstimate: 0.75, Currency: "USD"}},
		{at: morning, total: tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 3000, Output: 300, CostEstimate: 0.75, Currency: "USD"}},
	}
	for _, report := range reports {
		tx, err := service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		event := tracker.NativeRunEvent{Type: "run.checkpointed", Data: tracker.NativeRunData{AttemptID: "attempt_midnight", Usage: []tracker.NativeUsage{report.total}}}
		if err := recordAttemptUsage(t.Context(), tx, scope, event, prices, report.at); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]int64{"2026-09-09T23:00:00Z": 1000, "2026-09-10T00:00:00Z": 2000}
	rows, err := service.database.db.QueryContext(t.Context(), "SELECT period, input FROM attempt_usage WHERE attempt_id = ?", "attempt_midnight")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]int64{}
	for rows.Next() {
		var period string
		var input int64
		if err := rows.Scan(&period, &input); err != nil {
			t.Fatal(err)
		}
		got[period] = input
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("periods = %v, want %v", got, want)
	}
	for period, input := range want {
		if got[period] != input {
			t.Fatalf("periods = %v, want %v", got, want)
		}
	}
}

func TestBuildUsageReportBuckets(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 10, 13, 25, 0, 0, time.UTC)
	rows := []usageRow{
		{AttemptID: "a1", Period: time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC), Provider: "codex", Model: "gpt-6-astra", Input: 10, Cost: 1, Currency: "USD"},
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
			window, err := usageRangeWindow(test.rangeName, now)
			if err != nil {
				t.Fatal(err)
			}
			report := buildUsageReport(window, rows, nil, nil, usageTestPrices())
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
	window := usageWindow{From: from, To: from.Add(24 * time.Hour)}
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
			if got := usageBusySeconds(test.started, test.updated, window); got != test.want {
				t.Fatalf("usageBusySeconds() = %d, want %d", got, test.want)
			}
		})
	}
}

// A hub whose currency changes mid-hour starts a new row; spend already
// recorded in the old currency keeps its label and amount.
func TestRecordAttemptUsageKeepsCurrenciesApart(t *testing.T) {
	t.Parallel()
	service := openTestService(t, Config{DatabasePath: t.TempDir() + "/hub.db"})
	scope := nativeScope{organization: "org_usage", project: "prj_usage"}
	at := time.Date(2026, 9, 10, 9, 10, 0, 0, time.UTC)
	usd := usageTestPrices()
	eur := UsageConfig{Currency: "EUR", Prices: map[string]UsagePrice{"gpt-6-astra": {Input: 10, CachedInput: 1, Output: 40}}}.normalized()
	reports := []struct {
		prices UsageConfig
		at     time.Time
		input  int64
	}{
		{prices: usd, at: at, input: 1_000_000},
		{prices: eur, at: at.Add(20 * time.Minute), input: 3_000_000},
	}
	for _, report := range reports {
		tx, err := service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		event := tracker.NativeRunEvent{Type: "run.checkpointed", Data: tracker.NativeRunData{AttemptID: "attempt_currency", Usage: []tracker.NativeUsage{
			{Provider: "codex", Model: "gpt-6-astra", Input: report.input},
		}}}
		if err := recordAttemptUsage(t.Context(), tx, scope, event, report.prices, report.at); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]struct {
		input int64
		cost  float64
	}{"USD": {input: 1_000_000, cost: 10}, "EUR": {input: 2_000_000, cost: 20}}
	rows, err := service.database.db.QueryContext(t.Context(), "SELECT currency, input, cost_estimate FROM attempt_usage WHERE attempt_id = ?", "attempt_currency")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var currency string
		var input int64
		var cost float64
		if err := rows.Scan(&currency, &input, &cost); err != nil {
			t.Fatal(err)
		}
		expected, found := want[currency]
		if !found || input != expected.input || cost-expected.cost > 1e-9 || expected.cost-cost > 1e-9 {
			t.Fatalf("row %s = %d tokens at %v, want %+v", currency, input, cost, expected)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(want) {
		t.Fatalf("rows = %d, want %d", seen, len(want))
	}
}
