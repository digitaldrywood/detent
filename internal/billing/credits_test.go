package billing

import (
	"fmt"
	"net/url"
	"testing"
	"time"
)

func TestStripeCredits(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, kind, status, customer, mode string
		amount                             int64
		wantError, wantPaid                bool
	}{
		{"confirmed checkout", "checkout.session.completed", "paid", "cus_test", "false", 500, false, true},
		{"unpaid checkout", "checkout.session.completed", "unpaid", "cus_test", "false", 500, false, false},
		{"delayed confirmation", "checkout.session.async_payment_succeeded", "paid", "cus_test", "false", 500, false, true},
		{"intent confirmation", "payment_intent.succeeded", "succeeded", "cus_test", "false", 500, false, true},
		{"declined intent", "payment_intent.payment_failed", "requires_payment_method", "cus_test", "false", 500, false, false},
		{"wrong customer", "payment_intent.succeeded", "succeeded", "cus_other", "false", 500, true, false},
		{"wrong mode", "payment_intent.succeeded", "succeeded", "cus_test", "true", 500, true, false},
		{"wrong amount", "payment_intent.succeeded", "succeeded", "cus_test", "false", 499, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, provider := newStripeFixture(t)
			metadata := `"metadata":{"detent_organization_id":"org_test","detent_credit_key":"credit_test","detent_credit_price":"price_credit","detent_credit_cents":"500"}`
			intent := fmt.Sprintf(`{"id":"pi_credit","customer":%q,"livemode":%s,"currency":"usd","amount":%d,"amount_received":%d,"status":"succeeded","payment_method":"pm_credit",%s}`, test.customer, test.mode, test.amount, test.amount, metadata)
			object := fmt.Sprintf(`{"id":"pi_credit","customer":%q,"livemode":%s,"currency":"usd","amount":%d,"amount_received":%d,"status":%q,%s}`, test.customer, test.mode, test.amount, test.amount, test.status, metadata)
			if test.kind != "payment_intent.succeeded" && test.kind != "payment_intent.payment_failed" {
				object = fmt.Sprintf(`{"id":"cs_test_credit","mode":"payment","payment_status":%q,"payment_intent":"pi_credit",%s}`, test.status, metadata)
			}
			f.responses["/v1/events/evt_credit"] = fmt.Sprintf(`{"id":"evt_credit","type":%q,"livemode":%s,"data":{"object":%s}}`, test.kind, test.mode, object)
			f.responses["/v1/payment_intents/pi_credit"] = intent
			payment, err := provider.(CreditProvider).CreditEvent(t.Context(), Binding{AccountID: "acct_test", CustomerID: "cus_test", OrganizationID: "org_test"}, "evt_credit")
			if (err != nil) != test.wantError || (payment.Status == "succeeded") != test.wantPaid {
				t.Fatalf("payment=%+v err=%v", payment, err)
			}
		})
	}
}

func TestStripeCreditPurchaseAndCharge(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, priceType, currency string
		cents                     int64
		automatic, wantError      bool
	}{
		{"checkout", "one_time", "usd", 500, false, false},
		{"saved method charge", "one_time", "usd", 500, true, false},
		{"recurring price rejected", "recurring", "usd", 500, false, true},
		{"other currency rejected", "one_time", "eur", 500, false, true},
		{"price mismatch rejected", "one_time", "usd", 499, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, provider := newStripeFixture(t)
			f.responses["/v1/prices/price_credit"] = fmt.Sprintf(`{"id":"price_credit","livemode":false,"active":true,"type":%q,"billing_scheme":"per_unit","currency":%q,"unit_amount":%d}`, test.priceType, test.currency, test.cents)
			f.responses["/v1/payment_intents"] = `{"id":"pi_credit","customer":"cus_test","livemode":false,"currency":"usd","amount":500,"amount_received":500,"status":"succeeded","payment_method":"pm_credit","metadata":{"detent_organization_id":"org_test","detent_credit_key":"credit_test","detent_credit_price":"price_credit","detent_credit_cents":"500"}}`
			request := CreditRequest{CheckoutRequest: CheckoutRequest{Binding: Binding{AccountID: "acct_test", CustomerID: "cus_test", OrganizationID: "org_test"}, PriceID: "price_credit", IdempotencyKey: "credit_test", ReturnURL: "https://cloud.detent.ai/settings/billing", ExpiresAt: time.Unix(1900000000, 0)}, USDCents: 500}
			var err error
			if test.automatic {
				_, err = provider.(CreditProvider).CreditCharge(t.Context(), request, "pm_credit")
			} else {
				_, err = provider.(CreditProvider).CreditCheckout(t.Context(), request)
			}
			if (err != nil) != test.wantError {
				t.Fatalf("err=%v", err)
			}
			if test.wantError {
				return
			}
			f.responses["/v1/customers/cus_test"] = `{"id":"cus_test","livemode":false,"metadata":{"detent_organization_id":"org_test"},"invoice_settings":{"default_payment_method":"pm_subscription"}}`
			method, err := provider.(CreditProvider).SavedCreditPaymentMethod(t.Context(), fixtureBinding())
			if err != nil || method != "pm_subscription" {
				t.Fatalf("saved method=%s err=%v", method, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			form := f.posts[len(f.posts)-1]
			if f.keys[len(f.keys)-1] != "credit_test" {
				t.Fatal("missing durable key")
			}
			if test.automatic {
				if form.Get("off_session") != "true" || form.Get("confirm") != "true" || form.Get("payment_method") != "pm_credit" || form.Get("amount") != "500" {
					t.Fatalf("charge=%v", form)
				}
			}
			if !test.automatic {
				want := url.Values{
					"mode": {"payment"}, "customer": {"cus_test"}, "client_reference_id": {"org_test"},
					"line_items[0][price]": {"price_credit"}, "line_items[0][quantity]": {"1"},
					"success_url": {"https://cloud.detent.ai/settings/billing?credits=returned"},
					"cancel_url":  {"https://cloud.detent.ai/settings/billing"}, "expires_at": {"1900000000"},
					"payment_intent_data[setup_future_usage]": {"off_session"}, "payment_method_types[0]": {"card"},
					"metadata[detent_organization_id]": {"org_test"}, "metadata[detent_credit_key]": {"credit_test"},
					"metadata[detent_credit_price]": {"price_credit"}, "metadata[detent_credit_cents]": {"500"},
					"payment_intent_data[metadata][detent_organization_id]": {"org_test"},
					"payment_intent_data[metadata][detent_credit_key]":      {"credit_test"},
					"payment_intent_data[metadata][detent_credit_price]":    {"price_credit"},
					"payment_intent_data[metadata][detent_credit_cents]":    {"500"},
				}
				if form.Encode() != want.Encode() {
					t.Fatalf("credit checkout parameters changed: got %v, want %v", form, want)
				}
			}
		})
	}
}
