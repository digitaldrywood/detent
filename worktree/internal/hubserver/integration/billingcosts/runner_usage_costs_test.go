package billingcosts_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
			clock := &leaseTestClock{value: now.Add(-time.Hour)}
			cfg := Config{Clock: clock.Now, RunDetails: &memoryRunDetails{}}
			if test.hubPrices {
				cfg.Usage = &UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{"test-model": {Input: 1000, Currency: "USD"}}}
			}
			service := newClockedPostgresNativeService(t, cfg)
			for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
				t.Run(string(organization), func(t *testing.T) {
					clock.Advance(now.Add(-time.Hour).Sub(clock.Now()))
					f := newNativeFixture(t, service, organization, "usage")
					claimed := claimAttemptDiffFixture(t, false, f)
					issue, worker, lease := claimed.issue, claimed.worker, claimed.lease
					start := nativeStartedEvent(lease)
					start.Data.AttemptID, start.Data.RunID = claimed.attempt, claimed.run
					path := pendingEventPath(claimed)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, worker, start), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 86400}), http.StatusOK)
					if test.placement == "sprite" {
						result, err := usagePostgresAdmin(t, service).ExecContext(t.Context(), `UPDATE machines SET sprite_name='worker' WHERE organization_id=$1 AND id=$2`, organization, lease.MachineID)
						if err != nil {
							t.Fatal(err)
						}
						if n, err := result.RowsAffected(); err != nil || n != 1 {
							t.Fatalf("machine placement rows=%d error=%v", n, err)
						}
					}
					clock.Advance(now.Sub(clock.Now()))
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
					spoofed := finish
					spoofed.IdempotencyKey, spoofed.Data.MachineID = "wrong-machine", "machine-contradictory"
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path, worker, spoofed), http.StatusUnprocessableEntity)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, finish), http.StatusOK)
					finish.IdempotencyKey = "finish-replay"
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, finish), http.StatusOK)

					var input, output int64
					var cost float64
					if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						facts, err := r.(reportstore.UsageReader).ReadReportUsage(t.Context(), reportstore.OverviewQuery{Projects: []tracker.ProjectID{f.project.ID}, From: now.Add(-24 * time.Hour), Split: now.Add(-24 * time.Hour), To: now.Add(time.Hour), Limit: 1000})
						if err != nil {
							return err
						}
						for _, fact := range facts {
							if fact.AttemptID == start.Data.AttemptID {
								input += fact.Input
								output += fact.Output
								cost += fact.Cost
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					wantCost := test.estimate
					if test.hubPrices {
						wantCost = 0.1
					}
					if input != int64(100*sourceCount) || output != int64(10*sourceCount) || math.Abs(cost-wantCost*float64(sourceCount)) > 1e-9 {
						t.Fatalf("usage summary: input=%d output=%d cost=%g", input, output, cost)
					}
					var report monthlyCostReport
					response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/usage/monthly?month=2026-03", worker, nil)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &report)
					if len(report.Sources) != sourceCount {
						t.Fatalf("sources: %+v", report)
					}
					source := report.Sources[0]
					if source.AttemptID != start.Data.AttemptID || source.WorkItemID != string(issue.WorkItemID) || source.ProjectID != string(f.project.ID) || source.MachineID != string(lease.MachineID) || source.Placement != test.placement || source.Basis != test.basis || source.ReportedCostSource != entry.CostSource || !source.ReceivedAt.Equal(now) {
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
		})
	}
}

func TestRunnerUsageCorrections(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC)
	clock := &leaseTestClock{value: now.Add(-2 * time.Hour)}
	claimed := claimedAttemptDiffFixture(t, false, Config{Clock: clock.Now})
	f, issue, worker, lease := claimed.nativeFixture, claimed.issue, claimed.worker, claimed.lease
	start := nativeStartedEvent(lease)
	start.Data.AttemptID, start.Data.RunID = claimed.attempt, claimed.run
	path := f.base + "/work-items/" + string(issue.WorkItemID) + "/events"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, start), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/renew", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, TTLSeconds: 86400}), http.StatusOK)
	clock.Advance(now.Sub(clock.Now()))
	admin := usagePostgresAdmin(t, f.service)

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
		{name: "finish just after month start is exposed", status: "succeeded", from: window.From.Add(-time.Hour), to: window.From.Add(time.Microsecond), unreported: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET status=$3,started_at=$4,updated_at=$5 WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, start.Data.AttemptID, test.status, test.from, test.to); err != nil {
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
	if _, err := admin.ExecContext(t.Context(), `UPDATE native_attempts SET status='running',started_at=$3,updated_at=$3 WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, start.Data.AttemptID, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	from := now.Add(-2 * time.Hour)
	entry := tracker.NativeUsage{Provider: "codex", Model: "test-model", Input: 100, Output: 10, BillingMode: "metered", CostEstimate: 0.1, Currency: "USD", Revision: 1, From: from, To: now, ReportedAt: now}
	finish := start
	finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 2, "succeeded"
	finish.Data.Usage = []tracker.NativeUsage{entry}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, finish), http.StatusOK)
	if _, err := admin.ExecContext(t.Context(), `UPDATE machines SET sprite_name='changed' WHERE organization_id=$1 AND id=$2`, f.project.OrganizationID, lease.MachineID); err != nil {
		t.Fatal(err)
	}
	var original billingstore.CostObservation
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		revision, err := r.BillingCostRevision(t.Context(), billingstore.CostSource{Provider: "codex", ProviderAccount: "runner", SourceID: start.Data.AttemptID + ":test-model", From: from, To: now})
		if err != nil {
			return err
		}
		original = billingstore.CostObservation{OrganizationID: f.project.OrganizationID, ProjectID: revision.ProjectID, Reference: revision.Reference, ReceivedAt: revision.ReceivedAt}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := f.service.config.RunDetails.ReadDetail(t.Context(), string(f.project.OrganizationID), original.Reference.ArchiveReference())
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &original.Observation); err != nil {
		t.Fatal(err)
	}
	var legacy bytes.Buffer
	legacyWriter, err := gzip.NewWriterLevel(&legacy, gzip.BestSpeed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacyWriter.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := legacyWriter.Close(); err != nil {
		t.Fatal(err)
	}
	canonicalRef, legacyRef := original.Reference.ArchiveReference(), original.Reference.ArchiveReference()
	legacyRef.SHA256, legacyRef.CompressedSize = artifact.Digest(legacy.Bytes()), int64(legacy.Len())
	if legacyRef.Key, err = attachment.ArchiveKey(string(f.project.OrganizationID), legacyRef.SHA256); err != nil {
		t.Fatal(err)
	}
	if legacyRef.SHA256 == canonicalRef.SHA256 {
		t.Fatal("legacy archive must not match the current encoding")
	}
	details := f.service.config.RunDetails.(*memoryRunDetails)
	details.mu.Lock()
	details.data[legacyRef.Key] = bytes.Clone(raw)
	details.mu.Unlock()
	substitute := func(from, to attachment.ArchiveReference, sources bool) {
		t.Helper()
		statement := `UPDATE cost_observation_revisions SET object_key=$3,sha256=$4,compressed_bytes=$5 WHERE organization_id=$1 AND object_key=$2 AND revision=1`
		if sources {
			statement = `WITH changed AS (` + statement + ` RETURNING source_key) UPDATE cost_sources SET payload_hash=$4 WHERE organization_id=$1 AND revision=1 AND source_key IN(SELECT source_key FROM changed)`
		}
		result, err := admin.ExecContext(t.Context(), statement, f.project.OrganizationID, from.Key, to.Key, to.SHA256, to.CompressedSize)
		if err != nil {
			t.Fatal(err)
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			t.Fatalf("archive substitution rows=%d error=%v", n, err)
		}
	}
	substitute(canonicalRef, legacyRef, true)
	paid := int64(20_001)
	correction := finish
	correction.IdempotencyKey, correction.Data.Sequence = "cost-correction", 3
	corrected := entry
	corrected.Revision, corrected.ReportedCostMicros, corrected.CostSource = 2, &paid, "runner_report"
	correction.Data.Usage = []tracker.NativeUsage{corrected}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, worker, correction), http.StatusOK)
	substitute(legacyRef, canonicalRef, false)
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
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = reopenPostgresNativeService(t, f.service)
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
			record := original
			record.OrganizationID, record.ProjectID = test.scope.organization, test.scope.project
			err := f.service.config.TenantRepository.WriteTenant(t.Context(), test.scope.organization, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.RecordBillingCosts(t.Context(), []billingstore.CostObservation{record})
				return err
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("attribution error: %v", err)
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
