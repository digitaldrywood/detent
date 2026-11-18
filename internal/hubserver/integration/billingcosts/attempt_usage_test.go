package billingcosts_test

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/usagereport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func attemptUsagePrices() UsageConfig {
	return UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{
		"gpt-6-astra":   {Input: 10, CachedInput: 1, Output: 40},
		"claude-opus-5": {Input: 20, CachedInput: 2, Output: 100},
	}}
}

func startUsageAttempt(t *testing.T, f *attemptDiffFixture) {
	t.Helper()
	event := nativeStartedEvent(f.lease)
	event.IdempotencyKey = hubapi.NewID("event")
	event.Data.Identity.Model = "gpt-6-astra"
	event.Data.RunID, event.Data.AttemptID = f.run, f.attempt
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, pendingEventPath(f), f.worker, event), http.StatusOK)
}

// Spend reported after midnight on an attempt that started the day before is
// filed under the hour it arrived, not the attempt's start.
func TestRecordAttemptUsageAttributesIncrementsToTheirHour(t *testing.T) {
	t.Parallel()
	prices := attemptUsagePrices()
	evening := time.Date(2026, 9, 9, 23, 40, 0, 0, time.UTC)
	morning := time.Date(2026, 9, 10, 0, 20, 0, 0, time.UTC)
	clock := &leaseTestClock{value: evening.Add(-time.Minute)}
	service := newClockedPostgresNativeService(t, Config{Usage: &prices, Clock: clock.Now})
	reports := []struct {
		at    time.Time
		total tracker.NativeUsage
	}{
		{at: evening, total: tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 1000, Output: 100, CostEstimate: 0.25, Currency: "USD"}},
		{at: morning, total: tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 3000, Output: 300, CostEstimate: 0.75, Currency: "USD"}},
		{at: morning, total: tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 3000, Output: 300, CostEstimate: 0.75, Currency: "USD"}},
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			clock.Advance(evening.Add(-time.Minute).Sub(clock.Now()))
			f := claimAttemptDiffFixture(t, false, newNativeFixture(t, service, org, "usage-hour"))
			startUsageAttempt(t, f)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 3600}), http.StatusOK)
			for i, report := range reports {
				clock.Advance(report.at.Sub(clock.Now()))
				event := nativeStartedEvent(f.lease)
				event.Type, event.IdempotencyKey, event.OccurredAt = "run.checkpointed", fmt.Sprintf("usage-hour-%d", i), report.at
				event.Data.Identity.Model = "gpt-6-astra"
				event.Data.AttemptID, event.Data.RunID, event.Data.Sequence = f.attempt, f.run, int64(i+2)
				event.Data.Handoff = hubfixture.Checkpoint()
				event.Data.Usage = []tracker.NativeUsage{report.total}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, pendingEventPath(f), f.worker, event), http.StatusOK)
			}
			want := map[string]int64{"2026-09-09T23:00:00Z": 1000, "2026-09-10T00:00:00Z": 2000}
			rows, err := usagePostgresAdmin(t, service).QueryContext(t.Context(), `SELECT period,input FROM attempt_usage WHERE organization_id=$1 AND attempt_id=$2`, org, f.attempt)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			got := map[string]int64{}
			for rows.Next() {
				var period time.Time
				var input int64
				if err := rows.Scan(&period, &input); err != nil {
					t.Fatal(err)
				}
				got[period.UTC().Format(usagereport.PeriodLayout)] = input
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("periods=%v want=%v", got, want)
			}
			for period, input := range want {
				if got[period] != input {
					t.Fatalf("periods=%v want=%v", got, want)
				}
			}
		})
	}
}

func TestRecordAttemptUsageDelta(t *testing.T) {
	t.Parallel()
	prices := attemptUsagePrices()
	recordedAt := time.Date(2026, 9, 10, 9, 50, 0, 0, time.UTC)
	totalAt := time.Date(2026, 9, 10, 10, 10, 0, 0, time.UTC)
	clock := &leaseTestClock{value: recordedAt.Add(-time.Minute)}
	service := newClockedPostgresNativeService(t, Config{Usage: &prices, Clock: clock.Now})
	cases := []struct {
		name     string
		total    tracker.NativeUsage
		recorded tracker.NativeUsage
		dipped   tracker.NativeUsage
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
			name:     "total that rises again past its old high-water mark",
			total:    tracker.NativeUsage{Model: "gpt-6-astra", Input: 1300, Output: 130, CostEstimate: 0.325, Currency: "USD"},
			recorded: tracker.NativeUsage{Input: 1000, Output: 100, CostEstimate: 0.25, Currency: "USD"},
			dipped:   tracker.NativeUsage{Input: 500, Output: 50},
			want:     tracker.NativeUsage{Input: 300, Output: 30, CostEstimate: 0.075},
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
	for i, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			clock.Advance(recordedAt.Add(-time.Minute).Sub(clock.Now()))
			org := []tracker.OrganizationID{"org_first", "org_first_second"}[i%2]
			f := claimAttemptDiffFixture(t, false, newNativeFixture(t, service, org, fmt.Sprintf("usage-delta-%d", i)))
			startUsageAttempt(t, f)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 3600}), http.StatusOK)
			recorded, dipped, total := test.recorded, test.dipped, test.total
			recorded.Provider, recorded.Model, dipped.Provider, dipped.Model, total.Provider = "codex", test.total.Model, "codex", test.total.Model, "codex"
			type report struct {
				at    time.Time
				usage tracker.NativeUsage
			}
			var reports []report
			if test.recorded != (tracker.NativeUsage{}) {
				reports = append(reports, report{at: recordedAt, usage: recorded})
			}
			if test.dipped != (tracker.NativeUsage{}) {
				reports = append(reports, report{at: recordedAt.Add(5 * time.Minute), usage: dipped})
			}
			reports = append(reports, report{at: totalAt, usage: total})
			for j, report := range reports {
				clock.Advance(report.at.Sub(clock.Now()))
				event := nativeStartedEvent(f.lease)
				event.Type, event.IdempotencyKey, event.OccurredAt = "run.checkpointed", fmt.Sprintf("usage-delta-%d-%d", i, j), report.at
				event.Data.Identity.Model = "gpt-6-astra"
				event.Data.AttemptID, event.Data.RunID, event.Data.Sequence = f.attempt, f.run, int64(j+2)
				event.Data.Handoff = hubfixture.Checkpoint()
				event.Data.Usage = []tracker.NativeUsage{report.usage}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, pendingEventPath(f), f.worker, event), http.StatusOK)
			}
			rows, err := usagePostgresAdmin(t, service).QueryContext(t.Context(), `SELECT input,cached_input,output,cost_estimate FROM attempt_usage WHERE organization_id=$1 AND attempt_id=$2 AND period=$3`, org, f.attempt, totalAt.Truncate(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got tracker.NativeUsage
			for rows.Next() {
				var delta tracker.NativeUsage
				if err := rows.Scan(&delta.Input, &delta.CachedInput, &delta.Output, &delta.CostEstimate); err != nil {
					t.Fatal(err)
				}
				got.Input, got.CachedInput, got.Output, got.CostEstimate = got.Input+delta.Input, got.CachedInput+delta.CachedInput, got.Output+delta.Output, got.CostEstimate+delta.CostEstimate
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if diff := got.CostEstimate - test.want.CostEstimate; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("recorded delta cost = %v, want %v", got.CostEstimate, test.want.CostEstimate)
			}
			got.CostEstimate = test.want.CostEstimate
			if got != test.want {
				t.Fatalf("recorded delta = %+v, want %+v", got, test.want)
			}
			var highWater tracker.NativeUsage
			for _, report := range reports {
				highWater.Input, highWater.CachedInput, highWater.Output = max(highWater.Input, report.usage.Input), max(highWater.CachedInput, report.usage.CachedInput), max(highWater.Output, report.usage.Output)
			}
			var billed, counted tracker.NativeUsage
			if err := usagePostgresAdmin(t, service).QueryRowContext(t.Context(), `SELECT coalesce(sum(input),0),coalesce(sum(cached_input),0),coalesce(sum(output),0) FROM attempt_usage WHERE organization_id=$1 AND attempt_id=$2`, org, f.attempt).Scan(&billed.Input, &billed.CachedInput, &billed.Output); err != nil {
				t.Fatal(err)
			}
			if err := usagePostgresAdmin(t, service).QueryRowContext(t.Context(), `SELECT coalesce(sum(t.input_tokens),0),coalesce(sum(t.cached_input_tokens),0),coalesce(sum(t.output_tokens),0) FROM runner_issue_tokens t JOIN native_attempts a ON a.organization_id=t.organization_id AND a.issue_id=t.issue_id WHERE a.organization_id=$1 AND a.id=$2`, org, f.attempt).Scan(&counted.Input, &counted.CachedInput, &counted.Output); err != nil {
				t.Fatal(err)
			}
			if billed != highWater || counted != highWater {
				t.Fatalf("attempt usage billed=%+v runner tokens=%+v, want high-water %+v", billed, counted, highWater)
			}
		})
	}
}
