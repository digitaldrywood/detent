package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/runner"
)

type hostedCreditProvider struct {
	*hostedBillingProvider
	payment         billing.CreditPayment
	chargeStatus    string
	chargeError     error
	creditCheckouts []billing.CreditRequest
	checkoutError   error
	checkoutLoss    bool
	creditSessions  map[string]billing.Session
	chargeKeys      []string
	savedMethod     string
	savedError      error
}

func (p *hostedCreditProvider) SavedCreditPaymentMethod(context.Context, billing.Binding) (string, error) {
	if p.savedError != nil {
		return "", p.savedError
	}
	if p.savedMethod == "" {
		return "", billing.ErrPaymentFailed
	}
	return p.savedMethod, nil
}

func (p *hostedCreditProvider) CreditCheckout(_ context.Context, r billing.CreditRequest) (billing.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creditCheckouts = append(p.creditCheckouts, r)
	if p.checkoutError != nil {
		return billing.Session{}, p.checkoutError
	}
	if session, ok := p.creditSessions[r.IdempotencyKey]; ok {
		return session, nil
	}
	session := billing.Session{ID: "cs_test_credit", URL: "https://checkout.stripe.com/c/pay/test_credit", ExpiresAt: r.ExpiresAt}
	if p.creditSessions == nil {
		p.creditSessions = map[string]billing.Session{}
	}
	p.creditSessions[r.IdempotencyKey] = session
	if p.checkoutLoss {
		return billing.Session{}, errors.New("lost response pm_sensitive_secret")
	}
	return session, nil
}

func (p *hostedCreditProvider) CreditEvent(context.Context, billing.Binding, string) (billing.CreditPayment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.payment, nil
}

func (p *hostedCreditProvider) CreditCharge(_ context.Context, r billing.CreditRequest, _ string) (billing.CreditPayment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chargeKeys = append(p.chargeKeys, r.IdempotencyKey)
	return billing.CreditPayment{Key: r.IdempotencyKey, ID: "pi_auto", PriceID: r.PriceID, USDCents: r.USDCents, Status: p.chargeStatus}, p.chargeError
}

func configureTestCredits(t *testing.T, f *browserHostedFixture, base *hostedBillingProvider) *hostedCreditProvider {
	t.Helper()
	p := &hostedCreditProvider{hostedBillingProvider: base, chargeStatus: "succeeded"}
	f.service.config.Hosted.Billing.Provider = p
	f.service.config.Hosted.Billing.CreditPacks = []HostedCreditPack{{PriceID: "price_credit", Label: "AI credit pack", USDCents: 500}}
	if err := f.service.config.Hosted.validate(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.configureHostedBilling(t.Context(), f.service.config.Hosted); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAICreditPaymentConfirmation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, status, price string
		cents               int64
		wantBalance         int64
		wantError           bool
	}{
		{"paid", "succeeded", "price_credit", 500, 5000000, false},
		{"pending", "processing", "price_credit", 500, 0, false},
		{"unpaid", "requires_payment_method", "price_credit", 500, 0, false},
		{"wrong pack", "succeeded", "price_other", 500, 0, true},
		{"wrong amount", "succeeded", "price_credit", 501, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, base := newHostedBillingFixture(t)
			p := configureTestCredits(t, f, base)
			request := fmt.Sprintf(`{"price":"price_credit","idempotency_key":%q}`, strings.Repeat("k", 128))
			requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPost, "/billing/credits/checkout", request), http.StatusOK)
			requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPost, "/billing/credits/checkout", request), http.StatusOK)
			if len(p.creditCheckouts) != 1 || len(p.creditCheckouts[0].IdempotencyKey) > 255 {
				t.Fatalf("checkout calls=%d", len(p.creditCheckouts))
			}
			before, err := f.service.readAICredits(t.Context())
			if err != nil || before.BalanceMicros != 0 {
				t.Fatalf("credited before webhook: %+v %v", before, err)
			}
			p.payment = billing.CreditPayment{Key: p.creditCheckouts[0].IdempotencyKey, ID: "pi_credit", PriceID: test.price, USDCents: test.cents, Status: test.status, PaymentMethod: "pm_credit"}
			for _, id := range []string{"evt_credit_one", "evt_credit_one", "evt_credit_two"} {
				requireNativeStatus(t, postBillingEvent(t, f, id, "checkout.session.completed", true), http.StatusOK)
				err := f.service.billing.reconcile(t.Context())
				if (err != nil) != test.wantError {
					t.Fatalf("reconcile=%v", err)
				}
			}
			view, err := f.service.readAICredits(t.Context())
			if err != nil || view.BalanceMicros != test.wantBalance {
				t.Fatalf("credits=%+v err=%v", view, err)
			}
			if test.wantBalance > 0 && (len(view.History) != 1 || !view.CanAutoFund) {
				t.Fatalf("history/method=%+v", view)
			}
		})
	}
}

func TestAICreditAutoFund(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, status string
		balance      int64
		chargeError  error
		wantCalls    int
		wantEnabled  bool
		wantBalance  int64
	}{
		{"above threshold", "succeeded", 2000000, nil, 0, true, 2000000},
		{"at threshold", "succeeded", 1000000, nil, 0, true, 1000000},
		{"below threshold", "succeeded", 999999, nil, 1, true, 5999999},
		{"one in flight", "processing", 0, nil, 2, true, 0},
		{"failed disables", "requires_payment_method", 0, nil, 1, false, 0},
		{"authentication disables", "requires_action", 0, nil, 1, false, 0},
		{"decline disables", "", 0, billing.ErrPaymentFailed, 1, false, 0},
		{"uncertain retains key", "", 0, errors.New("lost response"), 2, true, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, base := newHostedBillingFixture(t)
			p := configureTestCredits(t, f, base)
			p.chargeStatus, p.chargeError = test.status, test.chargeError
			_, err := f.service.database.db.ExecContext(t.Context(), "UPDATE ai_credit_accounts SET balance_micros=?,auto_enabled=1,threshold_cents=100,price_id='price_credit',payment_method='pm_credit'", test.balance)
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				err := f.service.billing.autoFund(t.Context())
				if (err != nil) != (test.chargeError != nil && !errors.Is(test.chargeError, billing.ErrPaymentFailed)) {
					t.Fatalf("autoFund=%v", err)
				}
			}
			view, err := f.service.readAICredits(t.Context())
			if err != nil || view.BalanceMicros != test.wantBalance || view.AutoEnabled != test.wantEnabled || len(p.chargeKeys) != test.wantCalls {
				t.Fatalf("view=%+v calls=%v err=%v", view, p.chargeKeys, err)
			}
			if len(p.chargeKeys) == 2 && p.chargeKeys[0] != p.chargeKeys[1] {
				t.Fatalf("multiple charges: %v", p.chargeKeys)
			}
			if !test.wantEnabled && view.Failure == "" {
				t.Fatal("failure not shown to owner")
			}
			if !test.wantEnabled {
				requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPut, "/billing/credits/auto-fund", `{"enabled":true,"threshold_cents":100,"price":"price_credit"}`), http.StatusNoContent)
				tx, err := f.service.database.db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				err = f.service.database.applyCreditPayment(t.Context(), tx, billing.CreditPayment{Key: p.chargeKeys[0], ID: "pi_auto", PriceID: "price_credit", USDCents: 500, Status: "requires_payment_method"})
				if err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				updated, err := f.service.readAICredits(t.Context())
				if err != nil || !updated.AutoEnabled {
					t.Fatalf("stale failure disabled re-enabled auto-fund: %+v %v", updated, err)
				}
			}
			if test.status == "processing" {
				_, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO ai_credit_purchases(purchase_key,organization_id,mode,price_id,usd_cents,automatic,created_at) VALUES('duplicate_auto','org_browser_preview','test','price_credit',500,1,0)")
				if err == nil {
					t.Fatal("second in-flight charge accepted")
				}
			}
		})
	}
}

func TestAICreditOwnerSettings(t *testing.T) {
	t.Parallel()
	f, base := newHostedBillingFixture(t)
	configureTestCredits(t, f, base)
	for _, test := range []struct {
		role, body string
		want       int
	}{
		{"viewer", `{"enabled":true,"threshold_cents":100,"price":"price_credit"}`, http.StatusForbidden},
		{"admin", `{"enabled":true,"threshold_cents":100,"price":"price_credit"}`, http.StatusForbidden},
		{"owner", `{"enabled":true,"threshold_cents":100,"price":"price_credit"}`, http.StatusConflict},
		{"owner", `{"enabled":true,"threshold_cents":500,"price":"price_credit"}`, http.StatusBadRequest},
		{"owner", `{"enabled":false,"threshold_cents":0,"price":""}`, http.StatusNoContent},
	} {
		requireNativeStatus(t, f.billingAPI(t, test.role, http.MethodPut, "/billing/credits/auto-fund", test.body), test.want)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE ai_credit_accounts SET payment_method=''"); err != nil {
		t.Fatal(err)
	}
	f.service.config.Hosted.Billing.Provider.(*hostedCreditProvider).savedMethod = "pm_subscription"
	requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodPut, "/billing/credits/auto-fund", `{"enabled":true,"threshold_cents":100,"price":"price_credit"}`), http.StatusNoContent)
	response := f.billingAPI(t, "owner", http.MethodGet, "/billing", "")
	requireNativeStatus(t, response, http.StatusOK)
	var view hostedBillingView
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil || view.AICredits == nil || !view.AICredits.AutoEnabled {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestAICreditUsageDebit(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		input      int64
		multiplier float64
		wantDebit  int64
	}{
		{"ten cents with markup", 1000000, 1.5, 150000},
		{"sub-cent exact micros", 10000, 1.5, 1500},
		{"fractional micro rounds up", 1, 1.5, 1},
		{"fractional micro above one", 10, 1.5, 2},
		{"exact micro boundary", 20, 1.5, 3},
		{"zero cost", 0, 1.5, 0},
		{"custom markup", 1000000, 2, 200000},
		{"at cost", 1000000, 1, 100000},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newCoordinatorFixture(t, "credit-usage")
			d := f.service.database
			d.hostedOrganization = f.organization
			d.aiCreditMode = billing.ModeTest
			d.aiCreditCostMultiplier = test.multiplier
			f.service.config.Hosted = &HostedConfig{OrganizationID: string(f.organization), Billing: &HostedBillingConfig{}}
			if _, err := d.db.ExecContext(t.Context(), "INSERT INTO ai_credit_accounts(organization_id,mode,balance_micros) VALUES(?,'test',5000000)", f.organization); err != nil {
				t.Fatal(err)
			}
			record := f.seed(t, "Credit usage", nil)
			at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			usage := ConversationUsage{OrganizationID: f.organization, ProjectID: f.project.ID, ConversationID: record.ID, TurnID: "credit-usage-turn", Provider: "openai", Model: "gpt-6-luna", Tokens: runner.AgentTokenCounts{InputTokens: test.input}, Outcome: conversation.DeliveryCompleted, OccurredAt: at}
			for range 2 {
				if err := d.RecordConversationUsage(t.Context(), usage); err != nil {
					t.Fatal(err)
				}
			}
			view, err := f.service.readAICredits(t.Context())
			if err != nil || view.BalanceMicros != 5000000-test.wantDebit || len(view.History) != 1 || view.History[0].AmountMicros != -test.wantDebit || view.History[0].Kind != "usage" {
				t.Fatalf("credits=%+v err=%v", view, err)
			}
			window := usageWindow{From: at, To: at.Add(time.Hour)}
			summary, err := d.chatUsageSummary(t.Context(), f.organization, window, nil)
			wantCost := float64(test.input) * .1 / 1000000
			if err != nil || summary.Turns != 1 || math.Abs(summary.CostUSD-wantCost) > 1e-12 {
				t.Fatalf("raw usage=%+v want cost=%g err=%v", summary, wantCost, err)
			}
			rows, err := f.service.chatUsageRows(t.Context(), window, []string{string(f.project.ID)})
			if err != nil || len(rows) != 1 || math.Abs(rows[0].Cost-wantCost) > 1e-12 {
				t.Fatalf("raw report rows=%+v err=%v", rows, err)
			}
			usage.TurnID = "own-provider-turn"
			usage.Provider = "codex"
			if err := d.RecordConversationUsage(t.Context(), usage); err != nil {
				t.Fatal(err)
			}
			view, err = f.service.readAICredits(t.Context())
			if err != nil || view.BalanceMicros != 5000000-test.wantDebit || len(view.History) != 1 {
				t.Fatalf("own-provider credits=%+v err=%v", view, err)
			}
		})
	}
}
