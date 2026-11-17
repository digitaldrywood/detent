package usagecosts

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func costTestObservation(metric string, from, to time.Time) Observation {
	quantity, rate := 2.0, int64(10_000)
	unit := "GB-hour"
	if metric == "cpu" {
		unit = "CPU-hour"
	}
	return Observation{Provider: "fly_sprites", ProviderAccount: "customer", ResourceID: "sprite-immutable-id", ResourceName: "worker", Bucket: SpriteBucket, Metric: metric, SourceID: "meter-" + metric, Revision: 1, From: from, To: to, Quantity: &quantity, Unit: unit, QuantityBasis: "measured", Currency: "USD", Basis: "estimated", UnitPriceMicros: &rate, RateSource: "https://fly.io/sprites/", RateEffectiveAt: from.AddDate(0, -1, 0), EvidenceSource: "measurement-batch-1", ObservedAt: to, FreshUntil: to.AddDate(1, 0, 0), Coverage: "complete"}
}

func TestMonthlyCostAggregation(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	now := to.Add(time.Hour)
	tests := []struct {
		name      string
		mutate    func([]AttributedObservation) []AttributedObservation
		resources []Resource
		known     int64
		estimated int64
		billed    int64
		unknown   int
		stale     int
		coverage  string
		gaps      int
	}{
		{name: "compute and storage are separate source quantities", known: 80_000, estimated: 80_000, coverage: "complete"},
		{name: "idle compute retains storage costs", mutate: func(rows []AttributedObservation) []AttributedObservation {
			for index := range 2 {
				zero := 0.0
				rows[index].Quantity = &zero
				rows[index].AmountMicros = nil
			}
			return rows
		}, known: 40_000, estimated: 40_000, coverage: "complete"},
		{name: "missing storage is unknown", mutate: func(rows []AttributedObservation) []AttributedObservation { return rows[:2] }, known: 40_000, estimated: 40_000, coverage: "partial", gaps: 2},
		{name: "unknown observation is not a zero cost", mutate: func(rows []AttributedObservation) []AttributedObservation {
			rows[3].Quantity, rows[3].AmountMicros = nil, nil
			rows[3].QuantityBasis, rows[3].Basis, rows[3].Coverage = "unknown", "unknown", "unknown"
			return rows
		}, known: 60_000, estimated: 60_000, unknown: 1, coverage: "partial", gaps: 1},
		{name: "stale retained storage does not vanish", mutate: func(rows []AttributedObservation) []AttributedObservation {
			rows[3].FreshUntil = to
			return rows
		}, known: 80_000, estimated: 80_000, stale: 1, coverage: "partial", gaps: 1},
		{name: "partial provider coverage cannot claim completeness", mutate: func(rows []AttributedObservation) []AttributedObservation {
			rows[2].Coverage = "partial"
			return rows
		}, known: 80_000, estimated: 80_000, coverage: "partial", gaps: 1},
		{name: "provider correction can credit a billed line", mutate: func(rows []AttributedObservation) []AttributedObservation {
			credit := int64(-5_000)
			rows[3].Basis, rows[3].QuantityBasis, rows[3].AmountMicros = "provider_billed", "provider_reported", &credit
			return rows
		}, known: 55_000, estimated: 60_000, billed: -5_000, coverage: "complete"},
		{name: "unmetered deployed Sprite is unknown", mutate: func([]AttributedObservation) []AttributedObservation { return nil }, resources: []Resource{{ProjectID: "project", Provider: "fly_sprites", ProviderAccount: "customer", ResourceName: "existing"}}, coverage: "unknown", gaps: 4},
		{name: "another bucket cannot cover Sprite usage", mutate: func(rows []AttributedObservation) []AttributedObservation {
			rows[0].Bucket = "runner_api"
			return rows[:1]
		}, resources: []Resource{{ProjectID: "project", Provider: "fly_sprites", ProviderAccount: "customer", ResourceID: "sprite-immutable-id", ResourceName: "worker"}}, known: 20_000, estimated: 20_000, coverage: "partial", gaps: 4},
		{name: "only unknown observation has null total", mutate: func(rows []AttributedObservation) []AttributedObservation {
			rows[0].Quantity, rows[0].AmountMicros = nil, nil
			rows[0].QuantityBasis, rows[0].Basis, rows[0].Coverage = "unknown", "unknown", "unknown"
			return rows[:1]
		}, unknown: 1, coverage: "partial", gaps: 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			rows := []AttributedObservation{}
			for _, metric := range SpriteMetrics() {
				rows = append(rows, AttributedObservation{ProjectID: "project", Observation: costTestObservation(metric, from, to)})
			}
			if test.mutate != nil {
				rows = test.mutate(rows)
			}
			for index := range rows {
				if err := Normalize(&rows[index].Observation, now); err != nil {
					t.Fatal(err)
				}
			}
			report, err := BuildReport("org", "organization", Window{From: from, To: to}, now, rows, test.resources)
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
			window, err := Month(test.month, test.to)
			if err != nil {
				t.Fatal(err)
			}
			o := costTestObservation("cpu", test.from, test.to)
			o.Basis, o.QuantityBasis, o.AmountMicros = "provider_billed", "provider_reported", &test.amount
			report, err := BuildReport("org", "project", window, test.to, []AttributedObservation{{ProjectID: "project", Observation: o}}, nil)
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
	rows := []AttributedObservation{}
	for index := range 1000 {
		o := costTestObservation("cpu", from, to)
		o.ResourceID, o.SourceID = fmt.Sprintf("sprite-%d", index), fmt.Sprintf("cpu-%d", index)
		if err := Normalize(&o, to); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, AttributedObservation{ProjectID: "project", Observation: o})
	}
	full, err := BuildReport("org", "organization", Window{From: from, To: to}, to, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	bounded := BoundedDetails(full)
	result, err := operatortool.EncodeResult(bounded)
	if err != nil || len(result.Content) > operatortool.MaxResultBytes || !reflect.DeepEqual(full.Totals, bounded.Totals) || *bounded.Totals[0].KnownMicros != 20_000_000 || bounded.SourceCount != 1000 || bounded.GapCount != 3000 || bounded.DetailsComplete || len(bounded.Sources) != 32 || len(bounded.Gaps) != 32 {
		t.Fatalf("bounded report dropped totals or exceeded transport: %+v error=%v bytes=%d", bounded.Totals, err, len(result.Content))
	}
}

func TestCostLedgerReplay(t *testing.T) {
	owner := t
	t.Parallel()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to, now := from.Add(time.Hour), from.Add(2*time.Hour)
	s := newCostLedgerTestService(t, now)
	for _, organization := range []string{"org", "other-org"} {
		t.Run(organization, func(t *testing.T) {
			o := costTestObservation("storage_cold", from, to)
			for _, test := range []struct {
				name    string
				project string
				mutate  func(*Observation)
				changed bool
				wantErr bool
			}{
				{name: "initial storage observation", project: "original", changed: true},
				{name: "retry has no extra charge", project: "original"},
				{name: "source cannot move to another project", project: "replacement", wantErr: true},
				{name: "same revision cannot change content", project: "original", mutate: func(o *Observation) { o.Coverage = "partial" }, wantErr: true},
				{name: "revision correction replaces quantity", project: "original", mutate: func(o *Observation) { q := 1.0; o.Quantity, o.Revision = &q, 2 }, changed: true},
				{name: "late earlier revision cannot resurrect old cost", project: "original"},
				{name: "new source for same interval is not additive", project: "original", mutate: func(o *Observation) { o.SourceID = "invoice-alias" }, wantErr: true},
				{name: "overlapping observation is rejected", project: "original", mutate: func(o *Observation) { o.From = o.From.Add(time.Minute) }, wantErr: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					entry := o
					if test.mutate != nil {
						test.mutate(&entry)
					}
					changed, err := recordPostgresTestCost(t, s.Service, organization, test.project, entry, now)
					if (err != nil) != test.wantErr || changed != test.changed {
						t.Fatalf("changed=%t error=%v", changed, err)
					}
				})
			}
			before, err := s.Monthly(t.Context(), organization, "organization", nil, Window{From: from, To: from.AddDate(0, 1, 0)}, now)
			if err != nil {
				t.Fatal(err)
			}
			if *before.Totals[0].KnownMicros != 10_000 || before.Sources[0].Revision != 2 || before.Sources[0].ProjectID != "original" {
				t.Fatalf("correction was counted twice or misattributed: %+v", before)
			}
			if err := s.Service.deps.Repository.(*postgres.Repository).Close(); err != nil {
				t.Fatal(err)
			}
			s.Service = New(Deps{Repository: hubtest.RepositoryOn(owner, s.admin), Details: s.details})
			if changed, err := recordPostgresTestCost(t, s.Service, organization, "original", o, now); err != nil || changed {
				t.Fatalf("replay after restart changed ledger: %t %v", changed, err)
			}
			corrected := o
			q := 1.0
			corrected.Quantity, corrected.Revision = &q, 2
			if changed, err := recordPostgresTestCost(t, s.Service, organization, "original", corrected, now); err != nil || changed {
				t.Fatalf("correction retry after restart changed ledger: %t %v", changed, err)
			}
			after, err := s.Monthly(t.Context(), organization, "organization", nil, before.Period, now)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("restart totals changed: before=%+v after=%+v error=%v", before, after, err)
			}
			voided := corrected
			voided.Voided, voided.Revision = true, 3
			if changed, err := recordPostgresTestCost(t, s.Service, organization, "original", voided, now); err != nil || !changed {
				t.Fatalf("void original period: %t %v", changed, err)
			}
			replacement := corrected
			replacement.To, replacement.ObservedAt, replacement.Revision = to.Add(time.Minute), to.Add(time.Minute), 1
			if changed, err := recordPostgresTestCost(t, s.Service, organization, "original", replacement, now); err != nil || !changed {
				t.Fatalf("replace corrected interval: %t %v", changed, err)
			}
			if changed, err := recordPostgresTestCost(t, s.Service, organization, "original", corrected, now); err != nil || changed {
				t.Fatalf("old period resurrected after void: %t %v", changed, err)
			}
			overlapping := replacement
			overlapping.SourceID, overlapping.Model = "meter-overlapping", "example-model"
			var overlap *billingstore.CostOverlapError
			if _, err := recordPostgresTestCost(t, s.Service, organization, "original", overlapping, now); !errors.As(err, &overlap) || overlap.Overlap.Model != "example-model" || !overlap.Overlap.PeriodStart.Equal(overlapping.From) || !overlap.Overlap.PeriodEnd.Equal(overlapping.To) {
				t.Fatalf("overlapping stored source = %v, want a typed overlap naming its model and period", err)
			}
			final, err := s.Monthly(t.Context(), organization, "organization", nil, before.Period, now)
			if err != nil || len(final.Sources) != 1 || *final.Totals[0].KnownMicros != 10_000 || final.Sources[0].To != replacement.To {
				t.Fatalf("period correction double counted: %+v %v", final, err)
			}
		})
	}
}

func TestSpriteCostValidation(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	for _, test := range []struct {
		name   string
		mutate func(*Observation)
	}{
		{name: "nonfinite quantity", mutate: func(o *Observation) { q := math.Inf(1); o.Quantity = &q }},
		{name: "negative usage", mutate: func(o *Observation) { q := -1.0; o.Quantity = &q }},
		{name: "future observation", mutate: func(o *Observation) { o.ObservedAt = o.To.Add(time.Hour) }},
		{name: "missing freshness", mutate: func(o *Observation) { o.FreshUntil = time.Time{} }},
		{name: "rate effective after usage", mutate: func(o *Observation) { o.RateEffectiveAt = o.To }},
		{name: "invented storage zero", mutate: func(o *Observation) { o.Quantity = nil }},
		{name: "billed quantity is a local estimate", mutate: func(o *Observation) { v := int64(1); o.Basis, o.AmountMicros = "provider_billed", &v }},
		{name: "cost mismatches quantity and rate", mutate: func(o *Observation) { v := int64(1); o.AmountMicros = &v }},
		{name: "wrong storage unit", mutate: func(o *Observation) { o.Unit = "GB" }},
		{name: "empty source", mutate: func(o *Observation) { o.SourceID = "" }},
		{name: "mixed currency", mutate: func(o *Observation) { o.Currency = "usd" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			o := costTestObservation("storage_cold", from, to)
			test.mutate(&o)
			if err := ValidateSpriteObservation(&o, to); err == nil {
				t.Fatal("invalid observation accepted")
			}
		})
	}
}

func TestCostLedgerScopeIsolation(t *testing.T) {
	t.Parallel()
	from := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	s := newCostLedgerTestService(t, to)
	for index, scope := range []struct {
		organization, project, account, CurrencyValue, bucket string
	}{
		{"org", "one", "customer", "USD", SpriteBucket},
		{"org", "two", "other-customer", "USD", SpriteBucket},
		{"other-org", "one", "customer", "USD", SpriteBucket},
		{"org", "one", "euro-customer", "EUR", SpriteBucket},
		{"org", "one", "customer", "USD", "runner_api"},
	} {
		o := costTestObservation("cpu", from, to)
		o.ProviderAccount, o.Currency, o.Bucket = scope.account, scope.CurrencyValue, scope.bucket
		o.SourceID += scope.bucket
		q := float64(index + 1)
		o.Quantity = &q
		if _, err := recordPostgresTestCost(t, s.Service, scope.organization, scope.project, o, to); err != nil {
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
			report, err := s.Monthly(t.Context(), test.organization, "test", test.projects, Window{From: from, To: to}, to)
			if err != nil {
				t.Fatal(err)
			}
			if len(report.Totals) != test.buckets {
				t.Fatalf("mixed bucket/currency totals: %+v", report.Totals)
			}
			for _, total := range report.Totals {
				if total.Bucket == SpriteBucket && total.Currency == "USD" && *total.KnownMicros != test.wantUSD {
					t.Fatalf("scope total: %+v", total)
				}
			}
		})
	}
}

func TestSpriteCostInventory(t *testing.T) {
	t.Parallel()
	admin := hubtest.Database(t)
	repository := hubtest.RepositoryOn(t, admin)
	service := New(Deps{Repository: repository, Details: hubtest.NewDetails()})
	now := time.Now().UTC()
	for _, organization := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Now: now})
		project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{})
		sprite := hubtest.Runner(t, repository, organization, hubtest.RunnerOptions{Organization: true, Hostname: "cost-sprite", Now: now})
		hubtest.Runner(t, repository, organization, hubtest.RunnerOptions{Projects: []tracker.ProjectID{project.ID}, Hostname: "cost-local", Now: now})
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			identity, err := w.RunnerIdentity(t.Context(), sprite.Identity.RunnerID)
			if err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: identity.TokenID, ProjectID: project.ID}); err != nil {
				return err
			}

			for _, projectID := range []tracker.ProjectID{"", project.ID} {
				envelope, err := keys(t).Seal([]byte("example-token"), accessstore.SecretAAD(organization, projectID, "fly_sprites_token"))
				if err != nil {
					return err
				}
				secret := accessstore.Secret{OrganizationID: organization, ProjectID: projectID, Kind: "fly_sprites_token", OrganizationSlug: "example-account", Envelope: envelope, UpdatedAt: now}
				if err := w.StoreAccessSecret(t.Context(), secret, accessstore.SecretAudit{OrganizationID: organization, ProjectID: projectID, Actor: "example", Kind: secret.Kind, Event: "create", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), "UPDATE machines SET sprite_name=hostname WHERE organization_id=$1 AND id=$2", organization, sprite.Identity.MachineID); err != nil {
			t.Fatal(err)
		}
		window, err := Month("", now)
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name     string
			projects []string
			want     []Resource
		}{
			{"organization", nil, []Resource{{Provider: "fly_sprites", ProviderAccount: "example-account", ResourceName: "cost-sprite"}, {ProjectID: string(project.ID), Provider: "fly_sprites", ProviderAccount: "example-account", ResourceName: "cost-sprite"}}},
			{"selected project", []string{string(project.ID)}, []Resource{{ProjectID: string(project.ID), Provider: "fly_sprites", ProviderAccount: "example-account", ResourceName: "cost-sprite"}}},
			{"ungranted project", []string{"prj_other"}, []Resource{}},
		} {
			t.Run(test.name, func(t *testing.T) {
				_, got, _, err := service.MonthlyRecords(t.Context(), string(organization), test.projects, window)
				if err != nil || !reflect.DeepEqual(got, test.want) {
					t.Fatalf("resources=%+v err=%v, want %+v", got, err, test.want)
				}
			})
		}
	}
}

func recordPostgresTestCost(t *testing.T, d *Service, organization, project string, observation Observation, now time.Time) (bool, error) {
	t.Helper()
	if err := Normalize(&observation, now); err != nil {
		return false, err
	}
	raw, err := json.Marshal(observation)
	if err != nil {
		return false, err
	}
	ref, err := d.deps.Details.StoreDetail(t.Context(), organization, raw)
	if err != nil {
		return false, err
	}
	var changed bool
	err = d.deps.Repository.WriteTenant(t.Context(), tracker.OrganizationID(organization), corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		changed, err = w.RecordBillingCost(t.Context(), billingstore.CostObservation{OrganizationID: tracker.OrganizationID(organization), ProjectID: tracker.ProjectID(project), Observation: observation, Reference: billingstore.AuditReferenceFromArchive(ref), ReceivedAt: now})
		return err
	})
	return changed, err
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
			if err := ValidateRunnerUsage(test.entry); err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
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
			cost, CurrencyValue, found := prices.Estimate(test.model, test.input, test.cached, test.output)
			if found != test.wantFound {
				t.Fatalf("estimate(%q) found = %t, want %t", test.model, found, test.wantFound)
			}
			if !found {
				return
			}
			if CurrencyValue != "USD" {
				t.Fatalf("estimate(%q) currency = %q, want USD", test.model, CurrencyValue)
			}
			if diff := cost - test.wantCost; diff > 1e-9 || diff < -1e-9 {
				t.Fatalf("estimate(%q) = %v, want %v", test.model, cost, test.wantCost)
			}
			if saving := prices.CacheSaving(test.model, test.input, test.cached); saving-test.wantSaving > 1e-9 || saving-test.wantSaving < -1e-9 {
				t.Fatalf("cacheSaving(%q, %d) = %v, want %v", test.model, test.cached, saving, test.wantSaving)
			}
		})
	}
	if saving := prices.CacheSaving("gpt-7", 1_000_000, 1_000_000); saving != 0 {
		t.Fatalf("cacheSaving for an unpriced model = %v, want 0", saving)
	}
}

func usageTestPrices() Prices {
	return Prices{Currency: "USD", Prices: map[string]Price{
		"gpt-6-astra":   {Input: 10, CachedInput: 1, Output: 40},
		"claude-opus-5": {Input: 20, CachedInput: 2, Output: 100},
	}}.Normalized()
}
