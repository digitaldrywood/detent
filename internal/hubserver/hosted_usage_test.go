package hubserver

import (
	"encoding/json"
	"net/http"
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
	now := time.Date(2026, 9, 10, 13, 0, 0, 0, time.UTC)
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
			if !window.To.Equal(now) || window.To.Sub(window.From) != test.span {
				t.Fatalf("usageRangeWindow(%q) = %+v, want a %v window ending at %v", test.value, window, test.span, now)
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
			wantCost: 100.0 / 1_000_000 * 1, wantFound: true, wantSaving: 400.0 / 1_000_000 * 9,
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
			if saving := prices.cacheSaving(test.model, test.cached); saving-test.wantSaving > 1e-9 || saving-test.wantSaving < -1e-9 {
				t.Fatalf("cacheSaving(%q, %d) = %v, want %v", test.model, test.cached, saving, test.wantSaving)
			}
		})
	}
	if saving := prices.cacheSaving("gpt-7", 1_000_000); saving != 0 {
		t.Fatalf("cacheSaving for an unpriced model = %v, want 0", saving)
	}
}

func TestBuildUsageReport(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	window := usageWindow{From: day, To: next.Add(24 * time.Hour)}
	rows := []usageRow{
		{AttemptID: "attempt_1", Day: day, Provider: "codex", Model: "gpt-6-astra", Input: 1_000_000, CachedInput: 400_000, Output: 100_000, Cost: 10, Currency: "USD", RunnerID: "rnr_1", RunnerName: "Studio", BusySeconds: 3600},
		{AttemptID: "attempt_1", Day: day, Provider: "claude", Model: "claude-opus-5", Input: 200_000, Output: 50_000, Cost: 5, Currency: "USD", RunnerID: "rnr_1", RunnerName: "Studio", BusySeconds: 3600},
		{AttemptID: "attempt_2", Day: next, Provider: "codex", Model: "gpt-6-astra", Input: 500_000, CachedInput: 100_000, Output: 20_000, Cost: 5, Currency: "USD", RunnerID: "rnr_2", RunnerName: "Mini", BusySeconds: 1800},
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
			"SELECT provider, model, input, cached_input, output, cost_estimate, currency FROM attempt_usage WHERE attempt_id = ? ORDER BY provider, model", start.Data.AttemptID)
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
	f := newBrowserHostedFixture(t, true)
	for _, account := range []string{"owner", "viewer"} {
		t.Run(account, func(t *testing.T) {
			var report usageReport
			browserHostedDecode(t, f.api(t, account, http.MethodGet, browserHostedOrganizationBase+"/usage?range=30d", nil, http.StatusOK), &report)
			if report.Providers == nil || report.Daily == nil || report.Runners == nil || report.Limits == nil {
				t.Fatalf("report has a null collection: %+v", report)
			}
			if report.Range.To.Sub(report.Range.From) != 30*24*time.Hour {
				t.Fatalf("range = %+v, want 30 days", report.Range)
			}
		})
	}
	f.api(t, "staff", http.MethodGet, browserHostedOrganizationBase+"/usage", nil, http.StatusForbidden)
	f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/usage?range=1y", nil, http.StatusUnprocessableEntity)
	f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/usage?project=proj_missing", nil, http.StatusNotFound)
	browserHostedStatus(t, f.page(t, "", browserHostedOrganizationBase+"/usage"), http.StatusUnauthorized)
}

// The endpoint aggregates the rows the store holds: the seeded days fold
// into the hero total, the provider rows, the daily series and both
// breakdowns, and the project filter narrows to what a member can read
// (decisions section 17.5).
func TestHostedUsageReportAggregatesStoredAttempts(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	var report usageReport
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/usage?range=90d", nil, http.StatusOK), &report)

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
		wantSavings += prices.cacheSaving(entry.Model, entry.Cached) * browserUsageDays
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
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/usage?range=90d&project="+f.privateProject, nil, http.StatusOK), &narrowed)
	if narrowed.Total.Cost != 0 || narrowed.Total.Tokens != 0 || len(narrowed.Providers) != 0 {
		t.Fatalf("private project usage = %+v, want an empty report", narrowed.Total)
	}
	f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/usage?range=90d&project="+f.privateProject, nil, http.StatusNotFound)
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

// usageFixtureCases binds the shared usage fixtures the client has written.
// The client owns that directory, so binding what is present keeps the Go
// half honest without failing a checkout where the fixtures have not landed.
// internal/hubserver/testdata/usage-report.json is the Go-side golden that
// always exists and is checked by TestUsageReportGoldenShape.
func usageFixtureCases(t *testing.T, directory string, present map[string]bool) map[string]conversationFixtureCase {
	t.Helper()
	cases := map[string]conversationFixtureCase{}
	for name := range present {
		if !strings.HasPrefix(name, "usage-") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatalf("read usage fixture %s: %v", name, err)
		}
		cases[name] = conversationFixtureCase{value: usageFixtureValue(t, raw)}
	}
	return cases
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
	compareConversationShape(t, "", want, got, map[string]bool{}, map[string]bool{})
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
