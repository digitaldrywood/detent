package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// The usage report: what runners report is stored
// per attempt, day, provider and model, and the report folds it into the
// shape the client reads.

func usageTestPrices() UsageConfig {
	return UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{
		"gpt-6-astra":   {Input: 10, CachedInput: 1, Output: 40},
		"claude-opus-5": {Input: 20, CachedInput: 2, Output: 100},
	}}.normalized()
}

// An empty window still answers the whole shape, with arrays rather than
// nulls, so the client never has to guard a missing field.
// A run event stores the attempt's running total. The event carries the
// total, not a delta, so a second event replaces the rows instead of adding
// to them.
func TestNativeRunEventRecordsUsage(t *testing.T) {
	t.Parallel()
	rewriteHistory := func(t *testing.T, service *Service, query string, args ...any) (sql.Result, error) {
		t.Helper()
		tx, err := usagePostgresAdmin(t, service).BeginTx(t.Context(), nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(t.Context(), "SET LOCAL session_replication_role=replica"); err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(t.Context(), query, args...)
		if err != nil {
			return nil, err
		}
		return result, tx.Commit()
	}
	prices := usageTestPrices()
	clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
	claimed := claimedAttemptDiffFixture(t, false, Config{Usage: &prices, Clock: clock.Now})
	f, issue, worker, lease := claimed.nativeFixture, claimed.issue, claimed.worker, claimed.lease
	path := f.base + "/work-items/" + string(issue.WorkItemID)
	start := nativeStartedEvent(lease)
	start.Data.AttemptID, start.Data.RunID = claimed.attempt, claimed.run
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, start), http.StatusOK)

	clock.Advance(time.Second)
	checkpoint := start
	checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "checkpoint", 2
	checkpoint.Data.Handoff = nativeTestCheckpoint()
	checkpoint.Data.Handoff.HeadSHA = strings.Repeat("a", 40)
	checkpoint.Data.Handoff.WorkspaceDigest = strings.Repeat("b", 64)
	checkpoint.Data.Steps = []tracker.NativeStepReceipt{{Step: "agent_turn", PolicyID: start.Data.PolicyID, ReviewState: strings.Repeat("a", 64), Source: *checkpoint.Data.Handoff, CompletedAt: clock.Now(), CompletionBody: "Completed source"}}
	checkpoint.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "gpt-6-astra", Input: 1000, CachedInput: 400, Output: 100, CostEstimate: 0.25, Currency: "USD"}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, checkpoint), http.StatusOK)
	var running tracker.NativeRuntimeEvidence
	runtimeResponse := performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime", worker, nil)
	requireNativeStatus(t, runtimeResponse, http.StatusOK)
	decodeHubResponse(t, runtimeResponse, &running)
	if running.Attempt == nil || running.Attempt.Status != "running" || len(running.Attempt.Usage) != 1 || running.Attempt.Usage[0].CostEstimate <= 0 || len(running.Attempt.Steps) != 1 || running.Attempt.Steps[0].CompletionBody != "Completed source" {
		t.Fatalf("running attempt lost usage or step receipt: %+v response=%s", running.Attempt, runtimeResponse.Body.String())
	}

	type row struct {
		provider, model, currency string
		input, cached, output     int64
		cost                      float64
	}
	read := func() []row {
		t.Helper()
		var facts []reportstore.UsageFact
		scope := analyticsTestScope(t, f)
		window := usageWindow{From: f.service.config.now().Add(-time.Hour), To: f.service.config.now().Add(time.Hour)}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
			var err error
			facts, err = reader.(reportstore.UsageReader).ReadReportUsage(t.Context(), reportstore.OverviewQuery{Projects: []tracker.ProjectID{scope.Project}, From: window.From, Split: window.From, To: window.To, Limit: maxAnalyticsPopulation})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		grouped := map[string]row{}
		for _, fact := range facts {
			if fact.AttemptID != start.Data.AttemptID {
				continue
			}
			key := fact.Provider + "\n" + fact.Model
			value := grouped[key]
			value.provider, value.model, value.currency = fact.Provider, fact.Model, fact.Currency
			value.input += fact.Input
			value.cached += fact.CachedInput
			value.output += fact.Output
			value.cost += fact.Cost
			grouped[key] = value
		}
		keys := make([]string, 0, len(grouped))
		for key := range grouped {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		rows := make([]row, 0, len(keys))
		for _, key := range keys {
			rows = append(rows, grouped[key])
		}
		return rows
	}
	stored := read()
	if len(stored) != 1 || stored[0] != (row{provider: "codex", model: "gpt-6-astra", input: 1000, cached: 400, output: 100, cost: 0.25, currency: "USD"}) {
		t.Fatalf("stored usage = %+v", stored)
	}

	// The finish carries the attempt's running total, which replaces the row.
	clock.Advance(time.Second)
	finish := checkpoint
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 3, "succeeded"
	finish.Data.Handoff = nil
	finish.Data.ProcessUsage = &procgroup.Usage{PeakMemoryBytes: 2097152, UserCPUSeconds: 1.25, SystemCPUSeconds: 0.25, WallSeconds: 3}
	finish.Data.Usage = []tracker.NativeUsage{
		{Provider: "codex", Model: "gpt-6-astra", Input: 3000, CachedInput: 1200, Output: 300, CostEstimate: 0.75, Currency: "USD"},
		// A model the runner could not price is priced by the hub table:
		// 200 uncached input at 20 and 50 output at 100 per million.
		{Provider: "claude_code", Model: "claude-opus-5", Input: 200, Output: 50},
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
	var receipt tracker.NativeRuntimeEvidence
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/runtime", worker, nil), &receipt)
	if receipt.Attempt == nil || receipt.Attempt.ProcessUsage == nil || *receipt.Attempt.ProcessUsage != *finish.Data.ProcessUsage {
		t.Fatalf("attempt resource receipt = %+v, want %+v", receipt.Attempt, finish.Data.ProcessUsage)
	}
	var attempts tracker.Page[tracker.NativeAttempt]
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path+"/attempts", worker, nil), &attempts)
	if len(attempts.Items) != 1 || attempts.Items[0].ProcessUsage == nil || *attempts.Items[0].ProcessUsage != *finish.Data.ProcessUsage {
		t.Fatalf("attempt usage read = %+v", attempts.Items)
	}
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
	window := usageWindow{From: f.service.config.now().Add(-time.Hour), To: f.service.config.now().Add(time.Hour)}
	reportRows, err := f.service.usageRows(t.Context(), window, []string{string(f.project.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if len(reportRows) != 2 {
		t.Fatalf("finished usage not projected: %+v", reportRows)
	}
	for _, want := range stored {
		found := false
		for _, got := range reportRows {
			if got.Provider == want.provider && got.Model == want.model && got.Input == want.input && got.CachedInput == want.cached && got.Output == want.output && got.Cost == want.cost {
				found = true
			}
		}
		if !found {
			t.Fatalf("finished usage not projected: stored=%+v report=%+v", stored, reportRows)
		}
	}

	t.Run("bounded historical catchup preserves receipt prices and source replay", func(t *testing.T) {
		base := time.Now().UTC().Truncate(time.Hour).Add(30*time.Minute + 1234*time.Nanosecond)
		clock := &leaseTestClock{value: base}
		initial := UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{"legacy-model": {Input: 1000}}}
		service := newClockedPostgresNativeService(t, Config{Clock: clock.Now, Usage: &initial})
		for index, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
			t.Run(string(organization), func(t *testing.T) {
				weight := int64(index + 1)
				clock.Advance(base.Sub(clock.Now()))
				service.config.Usage = &initial
				fixture := newNativeFixture(t, service, organization, "usage-catchup")
				claimed := claimAttemptDiffFixture(t, false, fixture)
				start := nativeStartedEvent(claimed.lease)
				start.Data.AttemptID, start.Data.RunID = claimed.attempt, claimed.run
				start.OccurredAt = base.Add(-102 * time.Hour)
				path := pendingEventPath(claimed)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, claimed.worker, start), http.StatusOK)
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, fixture.base+"/leases/"+string(claimed.lease.ID)+"/renew", claimed.worker, tracker.NativeLeaseMutation{FencingToken: claimed.lease.FencingToken, TTLSeconds: 86400}), http.StatusOK)
				admin := usagePostgresAdmin(t, service)
				result, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) SELECT $1,$2,$3,$4::timestamptz-(102-n)*interval '1 hour','codex','legacy-model',10*$5,0,0,0.01*$5,'usd',$4 FROM generate_series(1,101)n`, organization, fixture.project.ID, claimed.attempt, base.Truncate(time.Hour), weight)
				if err != nil {
					t.Fatal(err)
				}
				if count, err := result.RowsAffected(); err != nil || count != 101 {
					t.Fatalf("legacy rows=%d error=%v", count, err)
				}
				legacyRun := start.Data
				legacyRun.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "legacy-model", Input: 1010 * weight, CostEstimate: 1.01 * float64(weight), Currency: "USD"}}
				scope := analyticsTestScope(t, fixture)
				legacyValue := tracker.CollaborationEvent{ID: "legacy-zero-usage", OrganizationID: organization, ProjectID: fixture.project.ID, AggregateType: "issue", AggregateID: claimed.issue.WorkItemID, Type: "run.checkpointed", SchemaVersion: 1, RecordedAt: base.Add(-time.Hour), Actor: scope.Actor(), Data: tracker.CollaborationData{Run: &legacyRun}}
				raw, err := json.Marshal(legacyValue)
				if err != nil {
					t.Fatal(err)
				}
				legacyEvent, err := eventstore.NewContent(service.runDetailStore()).PrepareEvent(t.Context(), organization, raw)
				if err != nil {
					t.Fatal(err)
				}
				if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Background, func(w tenantstore.Writer) error {
					_, err := w.AppendEvent(t.Context(), legacyEvent)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				result, err = rewriteHistory(t, service, `UPDATE collaboration_events SET project_sequence=0 WHERE organization_id=$1 AND project_id=$2 AND id='legacy-zero-usage'`, organization, fixture.project.ID)
				if err != nil {
					t.Fatal(err)
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					t.Fatalf("historical zero frontier rows=%d error=%v", count, err)
				}
				checkpoint := start
				checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "legacy-current", 2
				checkpoint.Data.Handoff = nativeTestCheckpoint()
				checkpoint.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "legacy-model", Input: 2000 * weight}}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, claimed.worker, checkpoint), http.StatusOK)
				result, err = rewriteHistory(t, service, `UPDATE collaboration_events e SET project_sequence=0 FROM report_event_facts f WHERE f.organization_id=e.organization_id AND f.event_id=e.id AND e.organization_id=$1 AND e.project_id=$2 AND e.attempt_id=$3 AND f.run_sequence=2`, organization, fixture.project.ID, claimed.attempt)
				if err != nil {
					t.Fatal(err)
				}
				if count, err := result.RowsAffected(); err != nil || count != 1 {
					t.Fatalf("pending zero project sequence rows=%d error=%v", count, err)
				}
				firstHour := clock.Now().Truncate(time.Hour)
				feature := service.UsageMaintenanceFeature()
				selected := hubmaintenance.Tenant{Organization: string(organization), Generation: 1}
				budget := hubmaintenance.Budget{Items: 1, Bytes: feature.Bytes}
				sawAttemptSeek, projectWalk := false, false
				step := func(feature hubmaintenance.Feature) {
					t.Helper()
					ctx := budget.Context(t.Context(), selected)
					ctx = postgres.WithTransactionTiming(ctx, nil, func(_ time.Time, query string) {
						if strings.Contains(query, "RecordAttemptEvents") {
							sawAttemptSeek = true
						}
						if strings.Contains(query, "RecordProjectEvents") || strings.Contains(query, "-- name: ListProjects") {
							projectWalk = true
						}
					})
					if err := feature.Step(ctx, selected, budget); err != nil {
						t.Fatal(err)
					}
				}
				step(feature)
				var unknown int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM attempt_usage WHERE organization_id=$1 AND attempt_id=$2 AND usage_input_total IS NULL`, organization, claimed.attempt).Scan(&unknown); err != nil || unknown != 1 {
					t.Fatalf("first bounded prefix unknown=%d error=%v", unknown, err)
				}
				imported := reportstore.UsageRecord{OrganizationID: organization, UsageFact: reportstore.UsageFact{ProjectID: fixture.project.ID, WorkItemID: claimed.issue.WorkItemID, AttemptID: claimed.attempt, Period: base.Truncate(time.Hour).Add(-102 * time.Hour), Provider: "codex", Model: "legacy-model", Input: 7 * weight, Cost: 0.007 * float64(weight), Currency: "USD"}, UpdatedAt: base}
				owner := service.config.TenantRepository.(interface {
					ImportReportUsageFact(context.Context, reportstore.UsageRecord) error
					VerifyReportUsageImport(context.Context, tracker.OrganizationID, string, string, int64) error
				})
				if err := owner.ImportReportUsageFact(t.Context(), imported); err != nil {
					t.Fatal(err)
				}
				if err := owner.VerifyReportUsageImport(t.Context(), organization, claimed.attempt, strings.Repeat("c", 64), 102); err != nil {
					t.Fatal(err)
				}
				changed := UsageConfig{Currency: "EUR", Prices: map[string]UsagePrice{"legacy-model": {Input: 2000}}}
				service.config.Usage = &changed
				clock.Advance(time.Hour)
				checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "legacy-current-eur", 3
				checkpoint.Data.Usage[0].Input = 3000 * weight
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, claimed.worker, checkpoint), http.StatusOK)
				secondHour := clock.Now().Truncate(time.Hour)
				latest := UsageConfig{Currency: "JPY", Prices: map[string]UsagePrice{"legacy-model": {Input: 9000}}}
				service.config.Usage = &latest
				if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.(reportstore.AnalyticsUsageReader).ReadAnalyticsUsagePage(t.Context(), reportstore.AnalyticsUsageQuery{ProjectID: fixture.project.ID, From: firstHour, To: secondHour.Add(time.Hour), Limit: 1000})
					if err == nil && !page.Partial {
						t.Fatal("empty current-window facts concealed pending usage")
					}
					if err != nil {
						return err
					}
					hidden, err := r.(reportstore.AnalyticsUsageReader).ReadAnalyticsUsagePage(t.Context(), reportstore.AnalyticsUsageQuery{ProjectID: fixture.project.ID, Principal: "unrelated-principal", From: firstHour, To: secondHour.Add(time.Hour), Limit: 1000})
					if err == nil && (len(hidden.Items) != 0 || hidden.Partial) {
						t.Fatal("pending usage leaked into another principal cohort")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				restarted := service.UsageMaintenanceFeature()
				testenv.WaitUntil(t, t.Context(), func() error {
					step(restarted)
					var pending int64
					if err := admin.QueryRowContext(t.Context(), `SELECT usage_pending_sequence FROM native_attempts WHERE organization_id=$1 AND id=$2`, organization, claimed.attempt).Scan(&pending); err != nil {
						return err
					}
					if pending != 0 {
						return fmt.Errorf("usage sequence %d is still pending", pending)
					}
					return nil
				})
				if !sawAttemptSeek || projectWalk {
					t.Fatalf("usage seek observed=%t project walk=%t", sawAttemptSeek, projectWalk)
				}
				for _, expected := range []struct {
					period   time.Time
					currency string
					input    int64
					cost     float64
				}{{firstHour, "USD", 983 * weight, 0.983 * float64(weight)}, {secondHour, "EUR", 1000 * weight, 2 * float64(weight)}} {
					var input int64
					var cost float64
					if err := admin.QueryRowContext(t.Context(), `SELECT input,cost_estimate FROM attempt_usage WHERE organization_id=$1 AND attempt_id=$2 AND period=$3 AND currency=$4 AND usage_origin='current'`, organization, claimed.attempt, expected.period, expected.currency).Scan(&input, &cost); err != nil {
						t.Fatal(err)
					}
					if input != expected.input || math.Abs(cost-expected.cost) > 1e-9 {
						t.Fatalf("receipt pricing %s=%d/%v want=%d/%v", expected.currency, input, cost, expected.input, expected.cost)
					}
				}
				var latestEvent eventstore.Event
				if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					latestEvent, err = r.LatestRunEvent(t.Context(), fixture.project.ID, claimed.attempt)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				payload, err := service.runDetailStore().ReadDetail(t.Context(), string(organization), eventstore.Reference(latestEvent.PayloadObjectKey, latestEvent.PayloadSHA256, latestEvent.PayloadBytes, latestEvent.PayloadContentBytes))
				if err != nil || !strings.Contains(string(payload), "_hub_usage_v1") {
					t.Fatalf("receipt pricing envelope missing: %v", err)
				}
				history := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/work-items/"+string(claimed.issue.WorkItemID)+"/history", testHubAdminToken, nil)
				requireNativeStatus(t, history, http.StatusOK)
				if !strings.Contains(history.Body.String(), "legacy-model") || strings.Contains(history.Body.String(), "_hub_usage_v1") {
					t.Fatal("typed history exposed private receipt pricing or omitted actual run usage")
				}
				exported := performHubAPIRequest(t, service, http.MethodGet, fixture.base+"/work-items/export", testHubAdminToken, nil)
				requireNativeStatus(t, exported, http.StatusOK)
				if !strings.Contains(exported.Body.String(), string(claimed.issue.WorkItemID)) || strings.Contains(exported.Body.String(), "_hub_usage_v1") {
					t.Fatal("work export exposed private receipt pricing or omitted the actual item")
				}
				for range 2 {
					if err := owner.ImportReportUsageFact(t.Context(), imported); err != nil {
						t.Fatal(err)
					}
					if err := owner.VerifyReportUsageImport(t.Context(), organization, claimed.attempt, strings.Repeat("c", 64), 102); err != nil {
						t.Fatal(err)
					}
				}
				var input int64
				var usd, eur float64
				if err := admin.QueryRowContext(t.Context(), `SELECT sum(input),coalesce(sum(cost_estimate) FILTER(WHERE currency='USD'),0),coalesce(sum(cost_estimate) FILTER(WHERE currency='EUR'),0) FROM attempt_usage WHERE organization_id=$1 AND attempt_id=$2`, organization, claimed.attempt).Scan(&input, &usd, &eur); err != nil {
					t.Fatal(err)
				}
				if input != 3000*weight || math.Abs(usd-2*float64(weight)) > 1e-9 || math.Abs(eur-2*float64(weight)) > 1e-9 {
					t.Fatalf("historical/current totals=%d/%v/%v", input, usd, eur)
				}
				if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
					complete, err := r.(reportstore.UsageCoverageReader).ReadReportUsageCoverage(t.Context(), reportstore.OverviewQuery{Projects: []tracker.ProjectID{fixture.project.ID}, From: base.Add(-200 * time.Hour), To: secondHour.Add(time.Hour), Limit: 1000})
					if err == nil && !complete {
						t.Fatal("complete source import and replay remained unavailable")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	})

	t.Run("pending attempts preserve source progress and repair event projections", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			complete bool
		}{
			{name: "non-run event", complete: true},
			{name: "missing report fact", complete: true},
			{name: "missing receipt prices"},
			{name: "missing run sequence"},
			{name: "conflicting report fact"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				clock := &leaseTestClock{value: time.Now().UTC().Truncate(time.Microsecond)}
				prices := UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{"example-model": {Input: 1000}}}
				service := newClockedPostgresNativeService(t, Config{Clock: clock.Now, Usage: &prices})
				for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
					t.Run(string(organization), func(t *testing.T) {
						fixture := newNativeFixture(t, service, organization, "usage-progress")
						claimed := claimAttemptDiffFixture(t, false, fixture)
						path := pendingEventPath(claimed)
						start := nativeStartedEvent(claimed.lease)
						start.Data.AttemptID, start.Data.RunID = claimed.attempt, claimed.run
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, claimed.worker, start), http.StatusOK)
						admin := usagePostgresAdmin(t, service)
						writeOne := func(query string, args ...any) {
							t.Helper()
							result, err := admin.ExecContext(t.Context(), query, args...)
							if err != nil {
								t.Fatal(err)
							}
							if count, err := result.RowsAffected(); err != nil || count != 1 {
								t.Fatalf("scoped fixture rows=%d error=%v", count, err)
							}
						}
						progress := func() (int64, int64) {
							t.Helper()
							var pending, input int64
							if err := admin.QueryRowContext(t.Context(), `SELECT a.usage_pending_sequence,coalesce((SELECT sum(u.input) FROM attempt_usage u WHERE u.organization_id=a.organization_id AND u.attempt_id=a.id AND u.usage_origin='current'),0) FROM native_attempts a WHERE a.organization_id=$1 AND a.id=$2`, organization, claimed.attempt).Scan(&pending, &input); err != nil {
								t.Fatal(err)
							}
							return pending, input
						}
						writeOne(`INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at) VALUES($1,$2,$3,$4,'codex','example-model',10,0,0,0.01,'USD',$4)`, organization, fixture.project.ID, claimed.attempt, clock.Now().Truncate(time.Hour).Add(-time.Hour))
						clock.Advance(time.Second)
						checkpoint := start
						checkpoint.Type, checkpoint.IdempotencyKey, checkpoint.Data.Sequence = "run.checkpointed", "progress-checkpoint", 2
						checkpoint.Data.Handoff = nativeTestCheckpoint()
						checkpoint.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "example-model", Input: 100}}
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, claimed.worker, checkpoint), http.StatusOK)
						var event eventstore.Event
						if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							event, err = r.LatestRunEvent(t.Context(), fixture.project.ID, claimed.attempt)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						switch tc.name {
						case "non-run event":
							data := checkpoint.Data
							data.Usage, data.Handoff = nil, nil
							value := tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: organization, ProjectID: fixture.project.ID, AggregateType: "issue", AggregateID: claimed.issue.WorkItemID, Type: "attempt.noted", SchemaVersion: 1, Actor: analyticsTestScope(t, fixture).Actor(), RecordedAt: clock.Now(), Data: tracker.CollaborationData{Run: &data}}
							raw, err := json.Marshal(value)
							if err != nil {
								t.Fatal(err)
							}
							note, err := eventstore.NewContent(service.runDetailStore()).PrepareEvent(t.Context(), organization, raw)
							if err != nil {
								t.Fatal(err)
							}
							note.Report = nil
							if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Background, func(w tenantstore.Writer) error {
								_, err := w.AppendEvent(t.Context(), note)
								return err
							}); err != nil {
								t.Fatal(err)
							}
						case "missing report fact":
							writeOne(`DELETE FROM report_event_facts WHERE organization_id=$1 AND event_id=$2`, organization, event.ID)
						case "missing receipt prices":
							value, err := eventstore.NewContent(service.runDetailStore()).Event(t.Context(), event)
							if err != nil {
								t.Fatal(err)
							}
							raw, err := json.Marshal(value)
							if err != nil {
								t.Fatal(err)
							}
							legacy, err := eventstore.NewContent(service.runDetailStore()).PrepareEvent(t.Context(), organization, raw)
							if err != nil {
								t.Fatal(err)
							}
							result, err := rewriteHistory(t, service, `UPDATE collaboration_events SET payload_object_key=$3,payload_sha256=$4,payload_bytes=$5,payload_content_bytes=$6 WHERE organization_id=$1 AND id=$2`, organization, event.ID, legacy.PayloadObjectKey, legacy.PayloadSHA256, legacy.PayloadBytes, legacy.PayloadContentBytes)
							if err != nil {
								t.Fatal(err)
							}
							if count, err := result.RowsAffected(); err != nil || count != 1 {
								t.Fatalf("historical receipt fixture rows=%d error=%v", count, err)
							}
						}
						clock.Advance(time.Second)
						finish := checkpoint
						finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "progress-finish", 3, "succeeded"
						finish.Data.Handoff = nil
						finish.Data.Usage[0].Input = 200
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, claimed.worker, finish), http.StatusOK)
						if tc.name == "missing run sequence" {
							writeOne(`DELETE FROM report_event_facts WHERE organization_id=$1 AND event_id=$2`, organization, event.ID)
							result, err := rewriteHistory(t, service, `DELETE FROM collaboration_events WHERE organization_id=$1 AND id=$2`, organization, event.ID)
							if err != nil {
								t.Fatal(err)
							}
							if count, err := result.RowsAffected(); err != nil || count != 1 {
								t.Fatalf("historical gap fixture rows=%d error=%v", count, err)
							}
						}
						if tc.name == "conflicting report fact" {
							writeOne(`UPDATE report_event_facts SET run_sequence=1 WHERE organization_id=$1 AND event_id=$2`, organization, event.ID)
						}
						writeOne(`DELETE FROM report_usage_current WHERE organization_id=$1 AND attempt_id=$2`, organization, claimed.attempt)
						feature := service.UsageMaintenanceFeature()
						selected := hubmaintenance.Tenant{Organization: string(organization), Generation: 1}
						budget := hubmaintenance.Budget{Items: 1, Bytes: feature.Bytes}
						for turn := range 6 {
							err := feature.Step(budget.Context(t.Context(), selected), selected, budget)
							if tc.name == "conflicting report fact" && turn%2 == 0 {
								if !errors.Is(err, corestore.ErrRevisionConflict) {
									t.Fatalf("turn %d: conflicting immutable projection error=%v", turn, err)
								}
							} else if err != nil {
								t.Fatalf("turn %d: %v", turn, err)
							}
							if tc.name == "non-run event" && turn == 2 {
								if pending, input := progress(); pending != 3 || input != 90 {
									t.Fatalf("non-run event changed run progress or usage=%d/%d", pending, input)
								}
							}
						}
						var sourceInput int64
						var sourceCost float64
						var sourceComplete bool
						if err := admin.QueryRowContext(t.Context(), `SELECT input,cost_estimate,complete FROM report_usage_current WHERE organization_id=$1 AND attempt_id=$2 AND model='example-model'`, organization, claimed.attempt).Scan(&sourceInput, &sourceCost, &sourceComplete); err != nil || sourceInput != 200 || math.Abs(sourceCost-0.2) > 1e-9 || !sourceComplete {
							t.Fatalf("accepted source did not refresh=%d/%v/%t error=%v", sourceInput, sourceCost, sourceComplete, err)
						}
						pending, currentInput := progress()
						if tc.complete && (pending != 0 || currentInput != 190) || !tc.complete && (pending == 0 || currentInput != 0) {
							t.Fatalf("usage progress pending=%d input=%d complete=%t", pending, currentInput, tc.complete)
						}
						if tc.name == "missing report fact" {
							var sequence int64
							if err := admin.QueryRowContext(t.Context(), `SELECT run_sequence FROM report_event_facts WHERE organization_id=$1 AND event_id=$2`, organization, event.ID).Scan(&sequence); err != nil || sequence != 2 {
								t.Fatalf("immutable event projection not repaired=%d error=%v", sequence, err)
							}
						}
						if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							query := reportstore.AnalyticsUsageQuery{ProjectID: fixture.project.ID, From: clock.Now().Add(-2 * time.Hour), To: clock.Now().Add(time.Hour), Limit: 100}
							page, err := r.(reportstore.AnalyticsUsageReader).ReadAnalyticsUsagePage(t.Context(), query)
							if err != nil {
								return err
							}
							if page.Partial == tc.complete {
								t.Fatalf("typed usage partial=%t complete=%t", page.Partial, tc.complete)
							}
							complete, err := r.(reportstore.UsageCoverageReader).ReadReportUsageCoverage(t.Context(), reportstore.OverviewQuery{Projects: []tracker.ProjectID{fixture.project.ID}, From: query.From, To: query.To, Limit: 100})
							if err == nil && complete != tc.complete {
								t.Fatalf("typed usage coverage=%t want=%t", complete, tc.complete)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					})
				}
			})
		}
	})

}

func TestNativeRunEventRejectsInvalidUsage(t *testing.T) {
	t.Parallel()
	f := newDefaultNativeFixture(t, Config{})
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
	t.Run("reports", func(t *testing.T) {
		for _, account := range []string{"owner", "viewer"} {
			for _, window := range []string{"24h", "48h", "7d", "30d"} {
				var report hostedReportsReport
				usageDecode(t, f.usage(t, account, browserHostedOrganizationBase+"/reports?project="+f.project+"&range="+window, http.StatusOK), &report)
				if report.Analytics.ProjectID != f.project || len(report.Throughput) == 0 || len(report.Coverage) == 0 {
					t.Fatalf("reports scope or coverage = %+v", report)
				}
			}
		}
		for _, request := range []struct {
			account, query string
			status         int
		}{
			{"", "?project=" + f.project + "&range=24h", http.StatusUnauthorized},
			{"staff", "?project=" + f.project + "&range=24h", http.StatusForbidden},
			{"owner", "?project=other&range=24h", http.StatusNotFound},
			{"owner", "?project=" + f.project + "&range=1y", http.StatusUnprocessableEntity},
		} {
			f.usage(t, request.account, browserHostedOrganizationBase+"/reports"+request.query, request.status)
		}
	})
	t.Run("diagnostics", func(t *testing.T) {
		for _, account := range []string{"owner", "viewer"} {
			var report diagnosticsReport
			usageDecode(t, f.usage(t, account, browserHostedOrganizationBase+"/diagnostics?range=30d", http.StatusOK), &report)
			if report.Findings != nil || report.Ready != nil || report.DetectorTick != nil || report.Refused != nil {
				t.Fatalf("unrecorded diagnostics became measurements: %+v", report)
			}
			var slots float64
			for _, bucket := range report.Capacity {
				slots += bucket.Slots
			}
			if slots <= 0 || len(report.Coverage) != 9 {
				t.Fatalf("missing recorded lease capacity or coverage: %+v", report)
			}
		}
		f.usage(t, "", browserHostedOrganizationBase+"/diagnostics", http.StatusUnauthorized)
		f.usage(t, "staff", browserHostedOrganizationBase+"/diagnostics", http.StatusForbidden)
		f.usage(t, "owner", browserHostedOrganizationBase+"/diagnostics?range=1y", http.StatusUnprocessableEntity)
		now := f.service.config.now().UTC().Add(-time.Minute)
		admin := hostedPostgresAdmin(t, f.service)
		var item tracker.NativeWorkItemID
		if err := admin.QueryRowContext(t.Context(), "SELECT native_id FROM issues WHERE organization_id=$1 AND project_id=$2 ORDER BY id LIMIT 1", "org_browser_preview", f.project).Scan(&item); err != nil {
			t.Fatal(err)
		}
		scope := hubscope.NativeScope{Organization: "org_browser_preview", Project: tracker.ProjectID(f.project)}
		for _, outcome := range []string{"ready", "skipped"} {
			appendAnalyticsTestEvent(t, f.service, scope, item, "scheduler.decision", tracker.CollaborationData{Decision: &tracker.NativeSchedulerDecision{Source: "native_claim", Outcome: outcome, Reason: "host_capacity", At: now}}, now)
		}
		f.observeUsageRefusal(t, "attempt_browser_usage_0_0", now)
		var report diagnosticsReport
		usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/diagnostics?range=24h", http.StatusOK), &report)
		if report.Ready == nil || *report.Ready != 1 || report.Skipped == nil || *report.Skipped != 1 || len(report.SkipReasons) != 1 || report.SkipReasons[0].Reason != "host_capacity" {
			t.Fatalf("recorded scheduler decisions missing: %+v", report)
		}
		if len(report.Refused) != 1 || report.Refused[0].Reason != "conflict" || report.Refused[0].Count != 1 || report.Coverage[7].Observed == nil || *report.Coverage[7].Observed != 1 {
			t.Fatalf("recorded landing refusal missing: %+v", report)
		}
		t.Run("repeated refusal remains counted after retry storm classification", func(t *testing.T) {
			for day := range browserUsageDays {
				for index := range browserUsageEntries {
					if day != 0 || index != 0 {
						f.observeUsageRefusal(t, fmt.Sprintf("attempt_browser_usage_%d_%d", day, index), now)
					}
				}
			}

			usageDecode(t, f.usage(t, "owner", browserHostedOrganizationBase+"/diagnostics?range=24h", http.StatusOK), &report)
			if len(report.Refused) != 1 || report.Refused[0].Count != browserUsageDays*len(browserUsageEntries) {
				t.Fatalf("repeated refusal missing: %+v", report.Refused)
			}
		})
	})
}

// The endpoint aggregates the rows the store holds: the seeded days fold
// into the hero total, the provider rows, the daily series and both
// breakdowns, and the project filter narrows to what a member can read.
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
// TestUsageReportGoldenShape pins the usage report shape on the Go side, so
// the hub still has a contract when the shared fixtures are absent.
// A day bucket is written as the day alone and an hourly bucket keeps its
// clock, which is how the client tells the two series apart.
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
func newUsageHostedFixture(t *testing.T, configure ...func(*Config)) usageHostedFixture {
	t.Helper()
	configure = append([]func(*Config){func(cfg *Config) { cfg.Content = (&changeObjectFixture{}).call }}, configure...)
	f := usageHostedFixture{newBrowserHostedOrganizationFixture(t, true, "org_browser_preview", configure...)}
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
	admin := hostedPostgresAdmin(t, f.service)
	org, project := tracker.OrganizationID("org_browser_preview"), tracker.ProjectID(f.project)
	var issueRow int64
	var nativeID tracker.NativeWorkItemID
	if err := admin.QueryRowContext(t.Context(), "SELECT id,native_id FROM issues WHERE organization_id=$1 AND project_id=$2 ORDER BY id LIMIT 1", org, project).Scan(&issueRow, &nativeID); err != nil {
		t.Fatalf("the browser fixture has no issue to seed usage against: %v", err)
	}
	content := eventstore.NewContent(f.service.runDetailStore())
	for day := range browserUsageDays {
		stamp := now.AddDate(0, 0, -day)
		for index, entry := range browserUsageEntries {
			cost, _, found := prices.estimate(entry.Model, entry.Input, entry.Cached, entry.Output)
			if !found {
				t.Fatalf("fixture price table does not price %s", entry.Model)
			}
			attemptID := fmt.Sprintf("attempt_browser_usage_%d_%d", day, index)
			leaseID := "lease_" + attemptID
			started := stamp.Add(-browserUsageBusy)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempt_runs(organization_id,run_id,issue_id)VALUES($1,$2,$3)`, org, "run_"+attemptID, issueRow); err != nil {
				t.Fatal(err)
			}
			var fencing int64
			if err := admin.QueryRowContext(t.Context(), `INSERT INTO leases(organization_id,lease_id,issue_id,machine_id,session_id,work_item_revision,expires_at,acquired_at,renewed_at,released_at)VALUES($1,$2,$3,$4,$5,1,$6::timestamptz+interval '1 hour',$7,$6,NULL)RETURNING fencing_token`, org, leaseID, issueRow, machine, "session_"+leaseID, stamp, started).Scan(&fencing); err != nil {
				t.Fatalf("seed usage lease: %v", err)
			}
			data := tracker.NativeRunData{RunID: "run_" + attemptID, AttemptID: attemptID, FencingToken: tracker.FencingToken(fencing), LeaseID: tracker.LeaseID(leaseID), MachineID: tracker.MachineID(machine), RunnerID: "runner_browser_preview", Sequence: 1, Outcome: "succeeded"}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := content.PreparePayload(t.Context(), org, raw)
			if err != nil {
				t.Fatal(err)
			}
			event, err := content.PrepareEvent(t.Context(), org, historyTestJSON(t, tracker.CollaborationEvent{ID: "event_" + attemptID, OrganizationID: org, ProjectID: project, AggregateID: nativeID, Type: "run.finished", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "runner_browser_preview"}, RecordedAt: stamp, Data: tracker.CollaborationData{Run: &data}}))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_attempts(organization_id,project_id,id,issue_id,lease_id,fencing_token,run_id,sequence,status,work_item_revision,dispatch_generation,payload_object_key,started_at,started_at_submicro_ns,updated_at)VALUES($1,$2,$3,$4,$5,$6,$7,1,'succeeded',1,1,$8,$9,$10,$11)`, org, project, attemptID, issueRow, leaseID, fencing, data.RunID, ref.Key, started.Truncate(time.Microsecond), started.Nanosecond()%1000, stamp); err != nil {
				t.Fatalf("seed usage attempt: %v", err)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: stamp}); err != nil {
					return err
				}
				if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: org, LeaseID: leaseID, RunnerID: "runner_browser_preview", AssignedAt: started}); err != nil {
					return err
				}
				_, err := w.AppendEvent(t.Context(), event)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET released_at=$3 WHERE organization_id=$1 AND lease_id=$2`, org, leaseID, stamp); err != nil {
				t.Fatalf("seed usage release: %v", err)
			}
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at,usage_origin,usage_input_total,usage_cached_input_total,usage_output_total,usage_cost_total,usage_sequence)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'USD',$11,'current',$7,$8,$9,$10,1)`, org, project, attemptID, stamp.UTC().Truncate(time.Hour), entry.Provider, entry.Model, entry.Input, entry.Cached, entry.Output, cost, stamp); err != nil {
				t.Fatalf("seed usage: %v", err)
			}
		}
	}
}

func (f usageHostedFixture) enrollUsageRunner(t *testing.T) string {
	t.Helper()
	now := f.service.config.now().UTC()
	later := now.Add(time.Hour)
	org := tracker.OrganizationID("org_browser_preview")
	machine := tracker.MachineID("machine_browser_preview_runner")
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: org, ID: string(machine), Hostname: "example-runner", DisplayName: "Preview runner", Capacity: 2, Version: "test"}); err != nil {
			return err
		}
		for _, entity := range []struct{ id, name string }{{"runner_browser_preview", "Preview runner"}, {"runner_browser_preview_shared", "Shared runner"}} {
			token := "token_" + entity.id
			enrollment := "enrollment_" + entity.id
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: token, Name: entity.name, Hash: apikey.HashToken(string(org) + token), Scope: "worker", NativeOnly: true, Kind: "service", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now, ExpiresAt: &later}); err != nil {
				return err
			}
			if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: org, ID: enrollment, RunnerID: entity.id, MachineID: machine, TokenHash: apikey.HashToken(string(org) + enrollment), Scope: "organization", SharedMachine: true, Operations: []string{"claim"}, CreatedAt: now, ExpiresAt: later, CreatedBy: "test_hub_admin"}); err != nil {
				return err
			}
			if err := w.RedeemRunnerEnrollment(t.Context(), enrollment, entity.id, machine, now); err != nil {
				return err
			}
			if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: org, RunnerID: entity.id, MachineID: machine, TokenID: token, EnrollmentID: enrollment, Scope: "organization", Operations: []string{"claim"}, DisplayName: entity.name, CapacityLimit: 2, ReportedCapacity: 2, OS: "linux", Architecture: "arm64", CreatedAt: now, ExpiresAt: later, LastHeartbeatAt: now}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed the preview runner: %v", err)
	}
	return string(machine)
}

func (f usageHostedFixture) observeUsageRefusal(t *testing.T, attempt string, at time.Time) {
	t.Helper()
	org := tracker.OrganizationID("org_browser_preview")
	project := tracker.ProjectID(f.project)
	admin := hostedPostgresAdmin(t, f.service)
	var item tracker.NativeWorkItemID
	var run, lease, machine string
	var sequence, fence int64
	if err := admin.QueryRowContext(t.Context(), `SELECT i.native_id,a.run_id,a.lease_id,l.machine_id,a.sequence,a.fencing_token FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id WHERE a.organization_id=$1 AND a.project_id=$2 AND a.id=$3`, org, project, attempt).Scan(&item, &run, &lease, &machine, &sequence, &fence); err != nil {
		t.Fatal(err)
	}
	data := tracker.NativeRunData{AttemptID: attempt, RunID: run, LeaseID: tracker.LeaseID(lease), MachineID: tracker.MachineID(machine), FencingToken: tracker.FencingToken(fence), Sequence: sequence + 1, Outcome: "succeeded", Runtime: &tracker.NativeRuntimeObservation{Landing: &tracker.NativeLandingReceipt{RefusalKind: "conflict", ObservedAt: at}}}
	content := eventstore.NewContent(f.service.runDetailStore())
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := content.PreparePayload(t.Context(), org, raw)
	if err != nil {
		t.Fatal(err)
	}
	event, err := content.PrepareEvent(t.Context(), org, historyTestJSON(t, tracker.CollaborationEvent{ID: fmt.Sprintf("refusal_%s_%d", attempt, sequence+1), OrganizationID: org, ProjectID: project, AggregateID: item, Type: "run.observed", SchemaVersion: 1, Actor: tracker.Actor{Kind: "runner", PrincipalID: "runner_browser_preview"}, RecordedAt: at, Data: tracker.CollaborationData{Run: &data}}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET sequence=$4,payload_object_key=$5,updated_at=$6 WHERE organization_id=$1 AND project_id=$2 AND id=$3`, org, project, attempt, sequence+1, ref.Key, at); err != nil {
		t.Fatal(err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: at}); err != nil {
			return err
		}
		_, err := w.AppendEvent(t.Context(), event)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// Spend reported after midnight on an attempt that started the day before is
// filed under the hour it arrived, not the attempt's start.
func TestRecordAttemptUsageAttributesIncrementsToTheirHour(t *testing.T) {
	t.Parallel()
	prices := usageTestPrices()
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
			f.start(t)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 3600}), http.StatusOK)
			for i, report := range reports {
				clock.Advance(report.at.Sub(clock.Now()))
				event := nativeStartedEvent(f.lease)
				event.Type, event.IdempotencyKey, event.OccurredAt = "run.checkpointed", fmt.Sprintf("usage-hour-%d", i), report.at
				event.Data.Identity.Model = "gpt-6-astra"
				event.Data.AttemptID, event.Data.RunID, event.Data.Sequence = f.attempt, f.run, int64(i+2)
				event.Data.Handoff = nativeTestCheckpoint()
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
				got[period.UTC().Format(usagePeriodLayout)] = input
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

// A hub whose currency changes mid-hour starts a new row; spend already
// recorded in the old currency keeps its label and amount.
func TestRecordAttemptUsageKeepsCurrenciesApart(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 10, 9, 10, 0, 0, time.UTC)
	usd := usageTestPrices()
	eur := UsageConfig{Currency: "EUR", Prices: map[string]UsagePrice{"gpt-6-astra": {Input: 10, CachedInput: 1, Output: 40}}}.normalized()
	clock := &leaseTestClock{value: at.Add(-time.Minute)}
	service := newClockedPostgresNativeService(t, Config{Usage: &usd, Clock: clock.Now})
	reports := []struct {
		prices UsageConfig
		at     time.Time
		input  int64
	}{
		{prices: usd, at: at, input: 1_000_000},
		{prices: eur, at: at.Add(20 * time.Minute), input: 3_000_000},
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			clock.Advance(at.Add(-time.Minute).Sub(clock.Now()))
			service.config.Usage = &usd
			f := claimAttemptDiffFixture(t, false, newNativeFixture(t, service, org, "usage-currency"))
			f.start(t)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 3600}), http.StatusOK)
			for i, report := range reports {
				clock.Advance(report.at.Sub(clock.Now()))
				service.config.Usage = &report.prices
				event := nativeStartedEvent(f.lease)
				event.Type, event.IdempotencyKey, event.OccurredAt = "run.checkpointed", fmt.Sprintf("usage-currency-%d", i), report.at
				event.Data.Identity.Model = "gpt-6-astra"
				event.Data.AttemptID, event.Data.RunID, event.Data.Sequence = f.attempt, f.run, int64(i+2)
				event.Data.Handoff = nativeTestCheckpoint()
				event.Data.Usage = []tracker.NativeUsage{{Provider: "codex", Model: "gpt-6-astra", Input: report.input}}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, pendingEventPath(f), f.worker, event), http.StatusOK)
			}
			want := map[string]struct {
				input int64
				cost  float64
			}{"USD": {input: 1_000_000, cost: 10}, "EUR": {input: 2_000_000, cost: 20}}
			rows, err := usagePostgresAdmin(t, service).QueryContext(t.Context(), `SELECT currency,input,cost_estimate FROM attempt_usage WHERE organization_id=$1 AND attempt_id=$2`, org, f.attempt)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
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
					t.Fatalf("row %s=%d tokens at %v want=%+v", currency, input, cost, expected)
				}
				seen++
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if seen != len(want) {
				t.Fatalf("rows=%d want=%d", seen, len(want))
			}
		})
	}
}

func TestRecordAttemptUsageDelta(t *testing.T) {
	t.Parallel()
	prices := usageTestPrices()
	recordedAt := time.Date(2026, 9, 10, 9, 50, 0, 0, time.UTC)
	totalAt := time.Date(2026, 9, 10, 10, 10, 0, 0, time.UTC)
	clock := &leaseTestClock{value: recordedAt.Add(-time.Minute)}
	service := newClockedPostgresNativeService(t, Config{Usage: &prices, Clock: clock.Now})
	cases := []struct {
		name     string
		total    tracker.NativeUsage
		recorded tracker.NativeUsage
		want     tracker.NativeUsage
		status   int
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
			status:   http.StatusConflict,
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
			f.start(t)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(f.lease.ID)+"/renew", f.worker, tracker.NativeLeaseMutation{FencingToken: f.lease.FencingToken, TTLSeconds: 3600}), http.StatusOK)
			recorded, total := test.recorded, test.total
			recorded.Provider, recorded.Model, total.Provider = "codex", test.total.Model, "codex"
			reports := []struct {
				at    time.Time
				usage tracker.NativeUsage
			}{{at: totalAt, usage: total}}
			if test.recorded != (tracker.NativeUsage{}) {
				reports = append([]struct {
					at    time.Time
					usage tracker.NativeUsage
				}{{at: recordedAt, usage: recorded}}, reports...)
			}
			for j, report := range reports {
				clock.Advance(report.at.Sub(clock.Now()))
				event := nativeStartedEvent(f.lease)
				event.Type, event.IdempotencyKey, event.OccurredAt = "run.checkpointed", fmt.Sprintf("usage-delta-%d-%d", i, j), report.at
				event.Data.Identity.Model = "gpt-6-astra"
				event.Data.AttemptID, event.Data.RunID, event.Data.Sequence = f.attempt, f.run, int64(j+2)
				event.Data.Handoff = nativeTestCheckpoint()
				event.Data.Usage = []tracker.NativeUsage{report.usage}
				status := http.StatusOK
				if j == len(reports)-1 && test.status != 0 {
					status = test.status
				}
				requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, pendingEventPath(f), f.worker, event), status)
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
		})
	}
}
