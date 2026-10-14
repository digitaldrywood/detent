package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func costTestObservation(metric string, from, to time.Time) costObservation {
	quantity, rate := 2.0, int64(10_000)
	unit := "GB-hour"
	if metric == "cpu" {
		unit = "CPU-hour"
	}
	return costObservation{Provider: "fly_sprites", ProviderAccount: "customer", ResourceID: "sprite-immutable-id", ResourceName: "worker", Bucket: spriteCostBucket, Metric: metric, SourceID: "meter-" + metric, Revision: 1, From: from, To: to, Quantity: &quantity, Unit: unit, QuantityBasis: "measured", Currency: "USD", Basis: "estimated", UnitPriceMicros: &rate, RateSource: "https://fly.io/sprites/", RateEffectiveAt: from.AddDate(0, -1, 0), EvidenceSource: "measurement-batch-1", ObservedAt: to, FreshUntil: to.AddDate(1, 0, 0), Coverage: "complete"}
}

func recordTestCost(t *testing.T, db *sql.DB, organization, project string, o costObservation, now time.Time) (bool, error) {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	changed, err := recordCostObservation(t.Context(), tx, organization, project, o, now)
	if err != nil {
		return changed, err
	}
	return changed, tx.Commit()
}

func TestMonthlyCostAggregation(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	now := to.Add(time.Hour)
	tests := []struct {
		name      string
		mutate    func([]attributedCostObservation) []attributedCostObservation
		resources []costResource
		known     int64
		estimated int64
		billed    int64
		unknown   int
		stale     int
		coverage  string
		gaps      int
	}{
		{name: "compute and storage are separate source quantities", known: 80_000, estimated: 80_000, coverage: "complete"},
		{name: "idle compute retains storage costs", mutate: func(rows []attributedCostObservation) []attributedCostObservation {
			for index := range 2 {
				zero := 0.0
				rows[index].Quantity = &zero
				rows[index].AmountMicros = nil
			}
			return rows
		}, known: 40_000, estimated: 40_000, coverage: "complete"},
		{name: "missing storage is unknown", mutate: func(rows []attributedCostObservation) []attributedCostObservation { return rows[:2] }, known: 40_000, estimated: 40_000, coverage: "partial", gaps: 2},
		{name: "unknown observation is not a zero cost", mutate: func(rows []attributedCostObservation) []attributedCostObservation {
			rows[3].Quantity, rows[3].AmountMicros = nil, nil
			rows[3].QuantityBasis, rows[3].Basis, rows[3].Coverage = "unknown", "unknown", "unknown"
			return rows
		}, known: 60_000, estimated: 60_000, unknown: 1, coverage: "partial", gaps: 1},
		{name: "stale retained storage does not vanish", mutate: func(rows []attributedCostObservation) []attributedCostObservation {
			rows[3].FreshUntil = to
			return rows
		}, known: 80_000, estimated: 80_000, stale: 1, coverage: "partial", gaps: 1},
		{name: "partial provider coverage cannot claim completeness", mutate: func(rows []attributedCostObservation) []attributedCostObservation {
			rows[2].Coverage = "partial"
			return rows
		}, known: 80_000, estimated: 80_000, coverage: "partial", gaps: 1},
		{name: "provider correction can credit a billed line", mutate: func(rows []attributedCostObservation) []attributedCostObservation {
			credit := int64(-5_000)
			rows[3].Basis, rows[3].QuantityBasis, rows[3].AmountMicros = "provider_billed", "provider_reported", &credit
			return rows
		}, known: 55_000, estimated: 60_000, billed: -5_000, coverage: "complete"},
		{name: "unmetered deployed Sprite is unknown", mutate: func([]attributedCostObservation) []attributedCostObservation { return nil }, resources: []costResource{{ProjectID: "project", Provider: "fly_sprites", ProviderAccount: "customer", ResourceName: "existing"}}, coverage: "unknown", gaps: 4},
		{name: "another bucket cannot cover Sprite usage", mutate: func(rows []attributedCostObservation) []attributedCostObservation {
			rows[0].Bucket = "runner_api"
			return rows[:1]
		}, resources: []costResource{{ProjectID: "project", Provider: "fly_sprites", ProviderAccount: "customer", ResourceID: "sprite-immutable-id", ResourceName: "worker"}}, known: 20_000, estimated: 20_000, coverage: "partial", gaps: 4},
		{name: "only unknown observation has null total", mutate: func(rows []attributedCostObservation) []attributedCostObservation {
			rows[0].Quantity, rows[0].AmountMicros = nil, nil
			rows[0].QuantityBasis, rows[0].Basis, rows[0].Coverage = "unknown", "unknown", "unknown"
			return rows[:1]
		}, unknown: 1, coverage: "partial", gaps: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rows := []attributedCostObservation{}
			for _, metric := range spriteCostMetrics() {
				rows = append(rows, attributedCostObservation{ProjectID: "project", costObservation: costTestObservation(metric, from, to)})
			}
			if test.mutate != nil {
				rows = test.mutate(rows)
			}
			for index := range rows {
				if err := normalizeCostObservation(&rows[index].costObservation, now); err != nil {
					t.Fatal(err)
				}
			}
			report, err := buildMonthlyCostReport("org", "organization", usageWindow{From: from, To: to}, now, rows, test.resources)
			if err != nil {
				t.Fatal(err)
			}
			if report.Coverage != test.coverage || len(report.Gaps) != test.gaps {
				t.Fatalf("coverage=%s gaps=%+v", report.Coverage, report.Gaps)
			}
			if len(rows) == 0 {
				if len(report.Totals) != 0 {
					t.Fatalf("unobserved resources have invented totals: %+v", report.Totals)
				}
				return
			}
			total := report.Totals[0]
			if test.name == "only unknown observation has null total" && total.KnownMicros != nil {
				t.Fatalf("unknown cost became zero: %+v", total)
			}
			value := func(p *int64) int64 {
				if p == nil {
					return 0
				}
				return *p
			}
			if value(total.KnownMicros) != test.known || value(total.EstimatedMicros) != test.estimated || value(total.BilledMicros) != test.billed || total.Unknown != test.unknown || total.Stale != test.stale {
				t.Fatalf("totals=%+v, known=%d estimated=%d billed=%d", total, value(total.KnownMicros), value(total.EstimatedMicros), value(total.BilledMicros))
			}
			if !reflect.DeepEqual(report.ByProject[0].Totals, report.Totals) || len(report.Sources) != len(rows) {
				t.Fatalf("project totals or source separation disagree: %+v", report)
			}
		})
	}
}

func TestCostMonthAllocation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, month string
		from, to    time.Time
		amount      int64
		want        int64
		quantity    float64
	}{
		{name: "UTC offset crosses January boundary", month: "2026-01", from: time.Date(2026, 1, 31, 18, 0, 0, 0, time.FixedZone("CST", -6*3600)), to: time.Date(2026, 2, 1, 2, 0, 0, 0, time.UTC), amount: 101, want: 0},
		{name: "first month gets deterministic integer allocation", month: "2026-01", from: time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC), to: time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC), amount: 101, want: 50, quantity: 1},
		{name: "second month receives remainder", month: "2026-02", from: time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC), to: time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC), amount: 101, want: 51, quantity: 1},
		{name: "negative adjustment preserves remainder", month: "2026-02", from: time.Date(2026, 1, 31, 23, 0, 0, 0, time.UTC), to: time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC), amount: -101, want: -51, quantity: 1},
		{name: "leap February includes 29 days", month: "2028-02", from: time.Date(2028, 2, 1, 0, 0, 0, 0, time.UTC), to: time.Date(2028, 3, 1, 0, 0, 0, 0, time.UTC), amount: 101, want: 101, quantity: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			window, err := costMonth(test.month, test.to)
			if err != nil {
				t.Fatal(err)
			}
			o := costTestObservation("cpu", test.from, test.to)
			o.Basis, o.QuantityBasis, o.AmountMicros = "provider_billed", "provider_reported", &test.amount
			report, err := buildMonthlyCostReport("org", "project", window, test.to, []attributedCostObservation{{ProjectID: "project", costObservation: o}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.quantity == 0 {
				if len(report.Sources) != 0 {
					t.Fatalf("exclusive month boundary included next month: %+v", report)
				}
				return
			}
			if *report.Totals[0].KnownMicros != test.want || *report.Sources[0].AllocatedQuantity != test.quantity || report.Timezone != "UTC" {
				t.Fatalf("allocation: %+v %+v", report.Totals, report.Sources)
			}
			if report.Sources[0].Allocation == "duration_prorated" && (report.Totals[0].BilledMicros != nil || report.Totals[0].EstimatedMicros == nil || report.Sources[0].AllocatedBasis != "estimated") {
				t.Fatal("prorated billed source claimed authoritative monthly accuracy")
			}
		})
	}
}

func TestMonthlyCostsMCPBounds(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	rows := []attributedCostObservation{}
	for index := range 1000 {
		o := costTestObservation("cpu", from, to)
		o.ResourceID, o.SourceID = fmt.Sprintf("sprite-%d", index), fmt.Sprintf("cpu-%d", index)
		if err := normalizeCostObservation(&o, to); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, attributedCostObservation{ProjectID: "project", costObservation: o})
	}
	full, err := buildMonthlyCostReport("org", "organization", usageWindow{From: from, To: to}, to, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	bounded := boundedMonthlyCostDetails(full)
	result, err := billingResult(bounded)
	if err != nil || len(result.Content) > operatortool.MaxResultBytes || !reflect.DeepEqual(full.Totals, bounded.Totals) || *bounded.Totals[0].KnownMicros != 20_000_000 || bounded.SourceCount != 1000 || bounded.GapCount != 3000 || bounded.DetailsComplete || len(bounded.Sources) != 32 || len(bounded.Gaps) != 32 {
		t.Fatalf("bounded report dropped totals or exceeded transport: %+v error=%v bytes=%d", bounded.Totals, err, len(result.Content))
	}
}

func TestCostLedgerReplay(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to, now := from.Add(time.Hour), from.Add(2*time.Hour)
	s := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return now }})
	o := costTestObservation("storage_cold", from, to)
	for _, test := range []struct {
		name    string
		project string
		mutate  func(*costObservation)
		changed bool
		wantErr bool
	}{
		{name: "initial storage observation", project: "original", changed: true},
		{name: "retry has no extra charge", project: "original"},
		{name: "source cannot move to another project", project: "replacement", wantErr: true},
		{name: "same revision cannot change content", project: "original", mutate: func(o *costObservation) { o.Coverage = "partial" }, wantErr: true},
		{name: "revision correction replaces quantity", project: "original", mutate: func(o *costObservation) { q := 1.0; o.Quantity, o.Revision = &q, 2 }, changed: true},
		{name: "late earlier revision cannot resurrect old cost", project: "original"},
		{name: "new source for same interval is not additive", project: "original", mutate: func(o *costObservation) { o.SourceID = "invoice-alias" }, wantErr: true},
		{name: "overlapping observation is rejected", project: "original", mutate: func(o *costObservation) { o.From = o.From.Add(time.Minute) }, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := o
			if test.mutate != nil {
				test.mutate(&entry)
			}
			changed, err := recordTestCost(t, s.database.db, "org", test.project, entry, now)
			if (err != nil) != test.wantErr || changed != test.changed {
				t.Fatalf("changed=%t error=%v", changed, err)
			}
		})
	}
	before, err := s.database.monthlyCosts(t.Context(), "org", "organization", nil, usageWindow{From: from, To: from.AddDate(0, 1, 0)}, now)
	if err != nil {
		t.Fatal(err)
	}
	if *before.Totals[0].KnownMicros != 10_000 || before.Sources[0].Revision != 2 || before.Sources[0].ProjectID != "original" {
		t.Fatalf("correction was counted twice or misattributed: %+v", before)
	}
	if err := s.database.Close(); err != nil {
		t.Fatal(err)
	}
	s.database, err = openDatabase(t.Context(), s.config)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := recordTestCost(t, s.database.db, "org", "original", o, now); err != nil || changed {
		t.Fatalf("replay after restart changed ledger: %t %v", changed, err)
	}
	corrected := o
	q := 1.0
	corrected.Quantity, corrected.Revision = &q, 2
	if changed, err := recordTestCost(t, s.database.db, "org", "original", corrected, now); err != nil || changed {
		t.Fatalf("correction retry after restart changed ledger: %t %v", changed, err)
	}
	after, err := s.database.monthlyCosts(t.Context(), "org", "organization", nil, before.Period, now)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("restart totals changed: before=%+v after=%+v error=%v", before, after, err)
	}
	voided := corrected
	voided.Voided, voided.Revision = true, 3
	if changed, err := recordTestCost(t, s.database.db, "org", "original", voided, now); err != nil || !changed {
		t.Fatalf("void original period: %t %v", changed, err)
	}
	replacement := corrected
	replacement.To, replacement.ObservedAt, replacement.Revision = to.Add(time.Minute), to.Add(time.Minute), 1
	if changed, err := recordTestCost(t, s.database.db, "org", "original", replacement, now); err != nil || !changed {
		t.Fatalf("replace corrected interval: %t %v", changed, err)
	}
	if changed, err := recordTestCost(t, s.database.db, "org", "original", corrected, now); err != nil || changed {
		t.Fatalf("old period resurrected after void: %t %v", changed, err)
	}
	final, err := s.database.monthlyCosts(t.Context(), "org", "organization", nil, before.Period, now)
	if err != nil || len(final.Sources) != 1 || *final.Totals[0].KnownMicros != 10_000 || final.Sources[0].To != replacement.To {
		t.Fatalf("period correction double counted: %+v %v", final, err)
	}
}

func TestSpriteCostValidation(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	for _, test := range []struct {
		name   string
		mutate func(*costObservation)
	}{
		{name: "nonfinite quantity", mutate: func(o *costObservation) { q := math.Inf(1); o.Quantity = &q }},
		{name: "negative usage", mutate: func(o *costObservation) { q := -1.0; o.Quantity = &q }},
		{name: "future observation", mutate: func(o *costObservation) { o.ObservedAt = o.To.Add(time.Hour) }},
		{name: "missing freshness", mutate: func(o *costObservation) { o.FreshUntil = time.Time{} }},
		{name: "rate effective after usage", mutate: func(o *costObservation) { o.RateEffectiveAt = o.To }},
		{name: "invented storage zero", mutate: func(o *costObservation) { o.Quantity = nil }},
		{name: "billed quantity is a local estimate", mutate: func(o *costObservation) { v := int64(1); o.Basis, o.AmountMicros = "provider_billed", &v }},
		{name: "cost mismatches quantity and rate", mutate: func(o *costObservation) { v := int64(1); o.AmountMicros = &v }},
		{name: "wrong storage unit", mutate: func(o *costObservation) { o.Unit = "GB" }},
		{name: "empty source", mutate: func(o *costObservation) { o.SourceID = "" }},
		{name: "mixed currency", mutate: func(o *costObservation) { o.Currency = "usd" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			o := costTestObservation("storage_cold", from, to)
			test.mutate(&o)
			if err := validateSpriteCostObservation(&o, to); err == nil {
				t.Fatal("invalid observation accepted")
			}
		})
	}
}

func TestSpriteUsageAPI(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to, now := from.Add(time.Hour), from.Add(2*time.Hour)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
	runner.enroll(t)
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_sprite_pools(organization_id,configured_by) SELECT ?,id FROM api_tokens WHERE token_hash=?`, f.project.OrganizationID, apikey.HashToken(testHubAdminToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO organization_sprite_members(organization_id,token_project_id,name,provider_organization,enrollment_id,state,idle_since,created_at) VALUES(?,?,?,?,?,'enrolled',?,?)`, f.project.OrganizationID, f.project.ID, "worker", "customer", runner.enrollment.ID, formatHubTime(from), formatHubTime(from)); err != nil {
		t.Fatal(err)
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other")
	request := spriteUsageRequest{Mutation: tracker.Mutation{IdempotencyKey: "usage-import"}, Observations: []costObservation{costTestObservation("cpu", from, to), costTestObservation("storage_cold", from, to)}}
	path := f.base + "/usage/sprites"
	var unmetered monthlyCostReport
	response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/usage/monthly?month=2026-02", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &unmetered)
	if len(unmetered.Gaps) != 4 || unmetered.Coverage != "unknown" || len(unmetered.Totals) != 0 {
		t.Fatalf("unmetered pool invented costs: %+v", unmetered)
	}
	response = performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusOK)
	replay := performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, request)
	requireNativeStatus(t, replay, http.StatusOK)
	if response.Body.String() != replay.Body.String() {
		t.Fatal("business replay changed receipt")
	}
	request.IdempotencyKey = "new-import-key"
	response = performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, request)
	requireNativeStatus(t, response, http.StatusOK)
	var receipt struct {
		Recorded int `json:"recorded"`
	}
	decodeHubResponse(t, response, &receipt)
	if receipt.Recorded != 0 {
		t.Fatalf("source retry added cost: %s", response.Body.String())
	}
	captured := make(chan context.Context, 1)
	f.service.echo.GET("/api/v2/organizations/:organization/cost-test", func(c echo.Context) error {
		captured <- c.Request().Context()
		return c.NoContent(http.StatusOK)
	}, f.service.operatorAuthority)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/cost-test", testHubAdminToken, nil), http.StatusOK)
	ctx := operatortool.BindConnection(<-captured, "cost-tools", "cost fixture")
	executor := hubProjectExecutor{f.service}
	if err := executor.OpenConnection(ctx); err != nil {
		t.Fatal(err)
	}
	args, err := json.Marshal(operatortool.ProjectRequest[operatortool.SpriteUsageInput]{ProjectID: string(f.project.ID), RequestID: "mcp-source-retry", Input: operatortool.SpriteUsageInput{Observations: request.Observations}})
	if err != nil {
		t.Fatal(err)
	}
	action := projectAction(t, executor, ctx, operatortool.Call{Name: "import_sprite_usage", Arguments: args})
	if action.Status != "succeeded" || action.Result != "{\"recorded\":0}" {
		t.Fatalf("MCP importer did not share API deduplication: %+v", action)
	}
	result, err := executor.Execute(ctx, operatortool.Call{Name: "monthly_usage_costs", Arguments: json.RawMessage(`{"scope":"organization","month":"2026-02"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var toolReport monthlyCostReport
	if err := json.Unmarshal(result.Content, &toolReport); err != nil || *toolReport.Totals[0].KnownMicros != 40_000 {
		t.Fatalf("MCP monthly total: %s %v", result.Content, err)
	}
	for _, test := range []struct {
		name, token, path string
		status            int
		want              int64
	}{
		{name: "own project", token: f.token, path: f.base + "/usage/monthly?month=2026-02", status: http.StatusOK, want: 40_000},
		{name: "runner budget consumer reads project", token: runner.redemption.Credential, path: f.base + "/usage/monthly?month=2026-02", status: http.StatusOK, want: 40_000},
		{name: "other project", token: other.token, path: other.base + "/usage/monthly?month=2026-02", status: http.StatusOK},
		{name: "project grant cannot read sibling", token: other.token, path: f.base + "/usage/monthly?month=2026-02", status: http.StatusNotFound},
		{name: "org total counts project once", token: testHubAdminToken, path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/usage/monthly?month=2026-02", status: http.StatusOK, want: 40_000},
		{name: "operator cannot read org total", token: f.token, path: "/api/v2/organizations/" + string(f.project.OrganizationID) + "/usage/monthly?month=2026-02", status: http.StatusForbidden},
		{name: "bad month", token: f.token, path: f.base + "/usage/monthly?month=2026-13", status: http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := performHubAPIRequest(t, f.service, http.MethodGet, test.path, test.token, nil)
			requireNativeStatus(t, response, test.status)
			if test.status != http.StatusOK {
				return
			}
			var report monthlyCostReport
			decodeHubResponse(t, response, &report)
			if test.want == 0 {
				if len(report.Totals) != 0 || report.Coverage != "unknown" {
					t.Fatalf("empty scope invented usage: %+v", report)
				}
				return
			}
			if *report.Totals[0].KnownMicros != test.want || len(report.ByProject) != 1 || report.Period.From != from {
				t.Fatalf("wrong monthly total: %+v", report)
			}
		})
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, f.token, request), http.StatusForbidden)
	request.IdempotencyKey = "atomic-import"
	next := costTestObservation("ram", from, to)
	conflict := costTestObservation("cpu", from, to)
	conflict.Coverage = "partial"
	request.Observations = []costObservation{next, conflict}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path, testHubAdminToken, request), http.StatusConflict)
	var count int
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM usage_cost_observations WHERE metric='ram'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed batch partially committed: count=%d error=%v", count, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE organization_sprite_members SET state='deleted' WHERE organization_id=?", f.project.OrganizationID); err != nil {
		t.Fatal(err)
	}
	window, _ := costMonth("2026-02", now)
	report, err := f.service.database.monthlyCosts(t.Context(), string(f.project.OrganizationID), "organization", nil, window, now)
	if err != nil || len(report.Sources) != 2 || report.Sources[0].ProjectID != string(f.project.ID) || *report.Totals[0].KnownMicros != 40_000 {
		t.Fatalf("deleted Sprite attribution was lost: %+v %v", report, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM organization_sprite_pools WHERE organization_id=?", f.project.OrganizationID); err != nil {
		t.Fatal(err)
	}
	after, err := f.service.database.monthlyCosts(t.Context(), string(f.project.OrganizationID), "organization", nil, window, now)
	if err != nil || !reflect.DeepEqual(report, after) {
		t.Fatalf("inventory cleanup changed historical costs: %+v %v", after, err)
	}
}

func TestHostedMonthlyCostsAccess(t *testing.T) {
	t.Parallel()
	f := newUsageHostedFixture(t)
	now := f.service.config.now()
	window, err := costMonth("", now)
	if err != nil {
		t.Fatal(err)
	}
	for index, project := range []string{f.project, f.privateProject} {
		o := costTestObservation("storage_cold", window.From, now)
		o.ResourceID += project
		o.SourceID += project
		q := float64(index + 1)
		o.Quantity = &q
		if _, err := recordTestCost(t, f.service.database.db, f.service.config.Hosted.OrganizationID, project, o, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		account string
		want    int64
	}{
		{account: "owner", want: 30_000},
		{account: "viewer", want: 10_000},
	} {
		t.Run(test.account, func(t *testing.T) {
			var report usageReport
			response := f.usage(t, test.account, browserHostedOrganizationBase+"/usage?range=month:"+now.UTC().Format("2006-01"), http.StatusOK)
			if err := json.Unmarshal(response.Body.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if report.MonthlyCosts == nil || *report.MonthlyCosts.Totals[0].KnownMicros != test.want || report.MonthlyCosts.Scope != "readable_projects" || report.MonthlyCosts.Period.From != window.From {
				t.Fatalf("monthly access report: %+v", report.MonthlyCosts)
			}
			if report.Total.Cost == float64(test.want)/1_000_000 {
				t.Fatal("infrastructure cost was folded into AI estimates")
			}
		})
	}
	var organization monthlyCostReport
	response := f.usage(t, "owner", browserHostedOrganizationBase+"/usage/monthly?month="+now.UTC().Format("2006-01"), http.StatusOK)
	decodeHubResponse(t, response, &organization)
	if organization.Scope != "organization" || *organization.Totals[0].KnownMicros != 30_000 {
		t.Fatalf("organization ledger: %+v", organization)
	}
	f.usage(t, "viewer", browserHostedOrganizationBase+"/usage/monthly", http.StatusNotFound)
	if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM hosted_project_grants WHERE project_id=? AND user_id=(SELECT user_id FROM hosted_members WHERE role='owner')", f.privateProject); err != nil {
		t.Fatal(err)
	}
	f.usage(t, "owner", browserHostedOrganizationBase+"/usage/monthly", http.StatusNotFound)
}

func TestCostLedgerScopeIsolation(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	s := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), now: func() time.Time { return to }})
	for index, scope := range []struct {
		organization, project, account, currency, bucket string
	}{
		{"org", "one", "customer", "USD", spriteCostBucket},
		{"org", "two", "other-customer", "USD", spriteCostBucket},
		{"other-org", "one", "customer", "USD", spriteCostBucket},
		{"org", "one", "euro-customer", "EUR", spriteCostBucket},
		{"org", "one", "customer", "USD", "runner_api"},
	} {
		o := costTestObservation("cpu", from, to)
		o.ProviderAccount, o.Currency, o.Bucket = scope.account, scope.currency, scope.bucket
		o.SourceID += scope.bucket
		q := float64(index + 1)
		o.Quantity = &q
		if _, err := recordTestCost(t, s.database.db, scope.organization, scope.project, o, to); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name, organization string
		projects           []string
		buckets            int
		wantUSD            int64
	}{
		{name: "org sums original observations once", organization: "org", buckets: 3, wantUSD: 30_000},
		{name: "project filter excludes sibling", organization: "org", projects: []string{"one"}, buckets: 3, wantUSD: 10_000},
		{name: "same provider source belongs to separate tenant", organization: "other-org", buckets: 1, wantUSD: 30_000},
		{name: "no project grants reveals no costs", organization: "org", projects: []string{}, buckets: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			report, err := s.database.monthlyCosts(t.Context(), test.organization, "test", test.projects, usageWindow{From: from, To: to}, to)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Totals) != test.buckets {
				t.Fatalf("mixed bucket/currency totals: %+v", report.Totals)
			}
			for _, total := range report.Totals {
				if total.Bucket == spriteCostBucket && total.Currency == "USD" && *total.KnownMicros != test.wantUSD {
					t.Fatalf("scope total: %+v", total)
				}
			}
		})
	}
}
