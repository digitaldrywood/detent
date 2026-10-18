package hubserver

import (
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerUsageCosts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC)
	paid, zero, combined, estimated := int64(20_001), int64(0), int64(40_002), int64(100_000)
	for _, test := range []struct {
		name, mode, placement, coverage string
		paid                            *int64
		estimate                        float64
		known                           *int64
		basis                           string
		unknown, unreported             int
		hubPrices                       bool
		incremental                     bool
	}{
		{name: "local paid API supersedes estimate", mode: "metered", placement: "local", paid: &paid, estimate: 0.1, known: &paid, basis: "runner_reported"},
		{name: "Sprite paid API is independent of infrastructure", mode: "metered", placement: "sprite", paid: &paid, estimate: 0.1, known: &paid, basis: "runner_reported"},
		{name: "incremental reports count nonoverlapping intervals once", mode: "metered", placement: "local", paid: &paid, estimate: 0.1, known: &combined, basis: "runner_reported", incremental: true},
		{name: "Hub estimate is a separate reproducible fact", mode: "metered", placement: "local", known: &estimated, basis: "estimated", unreported: 1, hubPrices: true},
		{name: "metered estimate exposes missing actual cost", mode: "metered", placement: "local", estimate: 0.1, known: &estimated, basis: "estimated", unreported: 1},
		{name: "subscription token activity is not API spend", mode: "subscription", placement: "local", paid: &paid, estimate: 0.1, basis: "unknown"},
		{name: "missing mode does not invent paid API spend", placement: "local", paid: &paid, estimate: 0.1, basis: "unknown", unknown: 1, unreported: 1},
		{name: "explicit unknown mode", mode: "unknown", placement: "local", estimate: 0.1, basis: "unknown", unknown: 1, unreported: 1},
		{name: "paid API without cost remains unknown", mode: "metered", placement: "local", basis: "unknown", unknown: 1, unreported: 1},
		{name: "explicit paid zero survives", mode: "metered", placement: "local", paid: &zero, known: &zero, basis: "runner_reported"},
		{name: "partial reported cost preserves known subtotal", mode: "metered", placement: "local", paid: &paid, estimate: 0.1, known: &paid, basis: "runner_reported", coverage: "partial", unreported: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{now: func() time.Time { return now }}
			if test.hubPrices {
				cfg.Usage = &UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{"test-model": {Input: 1000, Currency: "USD"}}}
			}
			f := newDefaultNativeFixture(t, cfg)
			approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
			issue := f.create(t, "usage")
			worker := f.worker(t, "worker")
			lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
			start := nativeStartedEvent(lease)
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, start), http.StatusOK)
			if test.placement == "sprite" {
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET capabilities_json=json_set(capabilities_json,'$.sprite_name','worker') WHERE id=?`, lease.MachineID); err != nil {
					t.Fatal(err)
				}
			}
			from, to := now.Add(-time.Hour), now
			entry := tracker.NativeUsage{Provider: "codex", Model: "test-model", Input: 100, Output: 10, BillingMode: test.mode, ReportedCostMicros: test.paid, CostEstimate: test.estimate, Currency: "USD", Revision: 1, From: from, To: to, ReportedAt: now, CostCoverage: test.coverage}
			if test.paid != nil {
				entry.CostSource = "runner_report"
			}
			finish := start
			finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 2, "succeeded"
			finish.Data.Usage = []tracker.NativeUsage{entry}
			sourceCount := 1
			if test.incremental {
				first, second := entry, entry
				first.UsageKind, second.UsageKind = "incremental", "incremental"
				first.SourceID, second.SourceID = "turn1", "turn2"
				first.To = from.Add(30 * time.Minute)
				second.From = first.To
				finish.Data.Usage = []tracker.NativeUsage{first, second}
				sourceCount = 2
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, finish), http.StatusOK)
			finish.IdempotencyKey = "finish-replay"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, finish), http.StatusOK)
			var report monthlyCostReport
			response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/usage/monthly?month=2026-03", worker, nil)
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &report)
			if len(report.Sources) != sourceCount {
				t.Fatalf("sources: %+v", report)
			}
			source := report.Sources[0]
			if source.AttemptID != start.Data.AttemptID || source.WorkItemID != string(issue.WorkItemID) || source.ProjectID != string(f.project.ID) || source.Placement != test.placement || source.Basis != test.basis || source.ReportedCostSource != entry.CostSource || !source.ReceivedAt.Equal(now) {
				t.Fatalf("attribution or provenance: %+v", source)
			}
			if (test.estimate > 0 || test.hubPrices) && (source.EstimatedAmountMicros == nil || *source.EstimatedAmountMicros != 100_000) {
				t.Fatalf("estimate lost: %+v", source)
			}
			if test.mode == "subscription" {
				if len(report.Totals) != 0 || source.AllocatedMicros != nil || source.Input != 100 {
					t.Fatalf("subscription became API spend: %+v", report)
				}
			} else {
				if len(report.Totals) != 1 || report.Totals[0].Unknown != test.unknown || report.Totals[0].UnreportedCosts != test.unreported || !reflect.DeepEqual(report.Totals[0].KnownMicros, test.known) {
					t.Fatalf("cost precedence: %+v", report.Totals)
				}
			}
			if report.RunnerReporting != "finish_time" || report.ActiveAttempts != 0 || report.UnreportedAttempts != 0 || !report.ActiveWorkComplete {
				t.Fatalf("finish coverage: %+v", report)
			}
			organization, err := f.service.database.monthlyCosts(t.Context(), string(f.project.OrganizationID), "organization", nil, report.Period, now)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(organization.Totals, report.Totals) {
				t.Fatal("project and organization totals differ")
			}
		})
	}
}

func TestRunnerUsageCorrections(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	issue := f.create(t, "usage corrections")
	worker := f.worker(t, "worker")
	lease := claimNativeAttempt(t, f, worker, "machine", "session", issue.WorkItemID)
	start := nativeStartedEvent(lease)
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, start), http.StatusOK)
	window, _ := costMonth("2026-03", now)
	active, err := f.service.database.monthlyCosts(t.Context(), string(f.project.OrganizationID), "organization", nil, window, now)
	if err != nil {
		t.Fatal(err)
	}
	if active.ActiveAttempts != 1 || active.UnreportedAttempts != 1 || active.ActiveWorkComplete {
		t.Fatalf("active work claimed complete: %+v", active)
	}

	for _, test := range []struct {
		name, status       string
		from, to           time.Time
		active, unreported int64
	}{
		{name: "attempt starts at next month", status: "running", from: window.To, to: window.To},
		{name: "attempt starts after next-month boundary", status: "running", from: window.To.Add(time.Nanosecond), to: window.To.Add(time.Nanosecond)},
		{name: "finish at month start belongs to prior month", status: "succeeded", from: window.From.Add(-time.Hour), to: window.From},
		{name: "finish just after month start is exposed", status: "succeeded", from: window.From.Add(-time.Hour), to: window.From.Add(time.Nanosecond), unreported: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET status=?,started_at=?,updated_at=? WHERE id=?`, test.status, formatHubTime(test.from), formatHubTime(test.to), start.Data.AttemptID); err != nil {
				t.Fatal(err)
			}
			report, err := f.service.database.monthlyCosts(t.Context(), string(f.project.OrganizationID), "organization", nil, window, now)
			if err != nil {
				t.Fatal(err)
			}
			if report.ActiveAttempts != test.active || report.UnreportedAttempts != test.unreported {
				t.Fatalf("month exposure: %+v", report)
			}
		})
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE native_attempts SET status='running',started_at=?,updated_at=? WHERE id=?`, formatHubTime(now), formatHubTime(now), start.Data.AttemptID); err != nil {
		t.Fatal(err)
	}
	from := now.Add(-2 * time.Hour)
	entry := tracker.NativeUsage{Provider: "codex", Model: "test-model", Input: 100, Output: 10, BillingMode: "metered", CostEstimate: 0.1, Currency: "USD", Revision: 1, From: from, To: now, ReportedAt: now}
	finish := start
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 2, "succeeded"
	finish.Data.Usage = []tracker.NativeUsage{entry}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, finish), http.StatusOK)
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET capabilities_json=json_set(capabilities_json,'$.sprite_name','changed') WHERE id=?`, lease.MachineID); err != nil {
		t.Fatal(err)
	}
	paid := int64(20_001)
	correction := finish
	correction.IdempotencyKey, correction.Data.Sequence = "cost-correction", 3
	corrected := entry
	corrected.Revision, corrected.ReportedCostMicros, corrected.CostSource = 2, &paid, "runner_report"
	correction.Data.Usage = []tracker.NativeUsage{corrected}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, correction), http.StatusOK)
	for _, test := range []struct {
		name   string
		edit   func(*tracker.NativeRunEvent)
		status int
	}{
		{name: "correction replay", status: http.StatusOK},
		{name: "changed outcome cannot correct usage", edit: func(e *tracker.NativeRunEvent) { e.Data.Sequence = 4; e.Data.Outcome = "failed" }, status: http.StatusConflict},
		{name: "overlapping incremental is not an extra charge", edit: func(e *tracker.NativeRunEvent) {
			e.Data.Sequence = 4
			e.Data.Usage = []tracker.NativeUsage{corrected}
			e.Data.Usage[0].UsageKind = "incremental"
			e.Data.Usage[0].SourceID = "turn"
		}, status: http.StatusUnprocessableEntity},
		{name: "same revision cannot change amount", edit: func(e *tracker.NativeRunEvent) {
			e.Data.Sequence = 4
			e.Data.Usage = []tracker.NativeUsage{corrected}
			other := int64(3)
			e.Data.Usage[0].ReportedCostMicros = &other
		}, status: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := correction
			e.IdempotencyKey = test.name
			if test.edit != nil {
				test.edit(&e)
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, e), test.status)
		})
	}
	reports := []monthlyCostReport{}
	for _, month := range []string{"2026-02", "2026-03"} {
		period, _ := costMonth(month, now)
		report, err := f.service.database.monthlyCosts(t.Context(), string(f.project.OrganizationID), "organization", nil, period, now)
		if err != nil {
			t.Fatal(err)
		}
		reports = append(reports, report)
	}
	if *reports[0].Totals[0].KnownMicros+*reports[1].Totals[0].KnownMicros != paid || reports[1].Sources[0].Revision != 2 || reports[1].Sources[0].Placement != "local" || reports[1].Sources[0].AllocatedBasis != "estimated" || *reports[1].Sources[0].EstimatedAmountMicros != 100_000 {
		t.Fatalf("monthly correction duplicated estimates: %+v", reports)
	}
	before, _ := json.Marshal(reports)
	if err := f.service.database.Close(); err != nil {
		t.Fatal(err)
	}
	f.service.database, err = openDatabase(t.Context(), f.service.config)
	if err != nil {
		t.Fatal(err)
	}
	replay := correction
	replay.IdempotencyKey = "restart-replay"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, replay), http.StatusOK)
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	for _, test := range []struct {
		name    string
		scope   nativeScope
		wantErr bool
	}{
		{name: "out-of-order original report", scope: scope},
		{name: "original project cannot change", scope: nativeScope{organization: scope.organization, project: "replacement"}, wantErr: true},
		{name: "original organization cannot change", scope: nativeScope{organization: "replacement", project: scope.project}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			err = recordAttemptUsage(t.Context(), tx, test.scope, finish, UsageConfig{}, now)
			if (err != nil) != test.wantErr {
				t.Fatalf("attribution error: %v", err)
			}
			if err == nil {
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	afterReports := []monthlyCostReport{}
	for _, report := range reports {
		after, err := f.service.database.monthlyCosts(t.Context(), string(f.project.OrganizationID), "organization", nil, report.Period, now)
		if err != nil {
			t.Fatal(err)
		}
		afterReports = append(afterReports, after)
	}
	after, _ := json.Marshal(afterReports)
	if string(before) != string(after) {
		t.Fatal("restart or out-of-order finish changed accounting")
	}
}

func TestRunnerCostValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		entry tracker.NativeUsage
	}{
		{name: "NaN estimate", entry: tracker.NativeUsage{CostEstimate: math.NaN()}},
		{name: "infinite estimate", entry: tracker.NativeUsage{CostEstimate: math.Inf(1)}},
		{name: "unknown billing spelling", entry: tracker.NativeUsage{BillingMode: "api"}},
		{name: "increment without identity", entry: tracker.NativeUsage{UsageKind: "incremental"}},
		{name: "untrusted cost provenance", entry: tracker.NativeUsage{CostSource: "private-provider-payload"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateRunnerCostUsage(test.entry); err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
}
