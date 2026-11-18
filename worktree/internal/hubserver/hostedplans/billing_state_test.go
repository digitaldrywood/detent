package hostedplans

import (
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
)

func TestHostedBillingLifecycle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	config := &HostedBillingConfig{GraceSeconds: 3600, Prices: []HostedBillingPrice{{PriceID: "price_fixture", Plan: PlanReference{ID: "paid", Version: 1}}}}
	previous := ResolveBilling(config, BillingState{}, activeBillingSnapshot(now), now)
	for _, test := range []struct {
		name, status, invoice, hold string
		cancel                      bool
		at                          time.Duration
		want                        string
		access                      bool
	}{
		{"activation", "active", "paid", "", false, 0, "active", true},
		{"incomplete", "incomplete", "open", "", false, 0, "incomplete", false},
		{"payment failure", "past_due", "open", "", false, time.Hour, "grace", true},
		{"exact grace expiry", "past_due", "open", "", false, 2 * time.Hour, "payment_failed", false},
		{"active unpaid invoice", "active", "open", "", false, time.Hour, "grace", true},
		{"cancellation scheduled", "active", "paid", "", true, 0, "canceling", true},
		{"cancellation boundary", "active", "paid", "", true, time.Hour, "canceling", false},
		{"immediate cancellation", "canceled", "paid", "", false, 0, "canceled", false},
		{"unpaid", "unpaid", "open", "", false, 0, "unpaid", false},
		{"paused", "paused", "paid", "", false, 0, "paused", false},
		{"refund", "active", "paid", "refunded", false, 0, "refunded", false},
		{"dispute", "active", "paid", "disputed", false, 0, "disputed", false},
		{"recovery", "active", "paid", "", false, 0, "active", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			snapshot := activeBillingSnapshot(now)
			snapshot.Status, snapshot.InvoiceStatus, snapshot.PaymentHold, snapshot.CancelAtPeriodEnd = test.status, test.invoice, test.hold, test.cancel
			at := now.Add(test.at)
			state := ResolveBilling(config, previous, snapshot, at)
			if state.Status != test.want || state.AccessUntil.After(at) != test.access {
				t.Fatalf("state=%+v", state)
			}
			if test.status == "past_due" && !state.GraceUntil.Equal(previous.PaidThrough.Add(time.Hour)) {
				t.Fatal("failed payment extended grace")
			}
		})
	}
	for _, test := range []struct {
		name     string
		snapshot billing.Snapshot
		previous BillingState
		want     bool
	}{
		{"unapproved price", billing.Snapshot{SubscriptionID: "sub_fixture", PriceID: "price_other", Status: "active"}, previous, false},
		{"failed initial payment", billing.Snapshot{SubscriptionID: "sub_new", PriceID: "price_fixture", Status: "past_due"}, previous, false},
		{"trial", billing.Snapshot{SubscriptionID: "sub_trial", PriceID: "price_fixture", Status: "trialing", TrialEnd: now.Add(time.Hour), PeriodEnd: now.Add(time.Hour)}, BillingState{}, true},
		{"free", billing.Snapshot{Status: "free"}, previous, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := ResolveBilling(config, test.previous, test.snapshot, now)
			if state.AccessUntil.After(now) != test.want {
				t.Fatalf("state=%+v", state)
			}
		})
	}
}

func TestHostedBillingRenewalAndPlanChange(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	config := &HostedBillingConfig{GraceSeconds: 3600, Prices: []HostedBillingPrice{
		{PriceID: "price_fixture", Plan: PlanReference{ID: "extended", Version: 1}},
		{PriceID: "price_lower", Plan: PlanReference{ID: "limited", Version: 1}},
	}}
	paid := ResolveBilling(config, BillingState{}, activeBillingSnapshot(now), now)
	for _, test := range []struct {
		name, price, invoice string
		end                  time.Time
		wantPlan             string
		wantEnd              time.Time
	}{
		{"renewal", "price_fixture", "paid", now.Add(31 * 24 * time.Hour), "extended", now.Add(31*24*time.Hour + time.Hour)},
		{"three-year approved price", "price_fixture", "paid", now.AddDate(3, 0, 0), "extended", now.AddDate(3, 0, 0).Add(time.Hour)},
		{"paid downgrade", "price_lower", "paid", now.Add(31 * 24 * time.Hour), "limited", now.Add(31*24*time.Hour + time.Hour)},
		{"unpaid plan change", "price_lower", "open", now.Add(31 * 24 * time.Hour), "extended", paid.AccessUntil},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := activeBillingSnapshot(now)
			snapshot.PriceID, snapshot.InvoiceStatus, snapshot.PeriodEnd = test.price, test.invoice, test.end
			snapshot.InvoiceID = "in_renewal"
			state := ResolveBilling(config, paid, snapshot, now)
			if state.Plan.ID != test.wantPlan || !state.AccessUntil.Equal(test.wantEnd) {
				t.Fatalf("state=%+v", state)
			}
			if test.invoice == "open" {
				later := ResolveBilling(config, state, snapshot, now.Add(4*time.Hour))
				if !later.AccessUntil.Equal(state.AccessUntil) {
					t.Fatal("retry restarted grace")
				}
			}
		})
	}
}

func activeBillingSnapshot(now time.Time) billing.Snapshot {
	return billing.Snapshot{SubscriptionID: "sub_fixture", PriceID: "price_fixture", Status: "active", InvoiceID: "in_fixture", InvoiceStatus: "paid", InvoiceCreatedAt: now.Add(-time.Hour), PeriodEnd: now.Add(time.Hour)}
}
