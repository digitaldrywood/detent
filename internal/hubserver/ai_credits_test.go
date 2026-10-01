package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	chargeKeys      []string
	savedMethod     string
}

func (p *hostedCreditProvider) SavedCreditPaymentMethod(context.Context, billing.Binding) (string, error) {
	if p.savedMethod == "" {
		return "", billing.ErrPaymentFailed
	}
	return p.savedMethod, nil
}

func (p *hostedCreditProvider) CreditCheckout(_ context.Context, r billing.CreditRequest) (billing.Session, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creditCheckouts = append(p.creditCheckouts, r)
	return billing.Session{ID: "cs_test_credit", URL: "https://checkout.stripe.com/c/pay/test_credit", ExpiresAt: r.ExpiresAt}, nil
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
	f := newCoordinatorFixture(t, "credit-usage")
	d := f.service.database
	d.hostedOrganization = f.organization
	d.aiCreditMode = billing.ModeTest
	if _, err := d.db.ExecContext(t.Context(), "INSERT INTO ai_credit_accounts(organization_id,mode,balance_micros) VALUES(?,'test',5000000)", f.organization); err != nil {
		t.Fatal(err)
	}
	record := f.seed(t, "Credit usage", nil)
	usage := ConversationUsage{OrganizationID: f.organization, ProjectID: f.project.ID, ConversationID: record.ID, TurnID: "credit-usage-turn", Provider: "openai", Model: "gpt-6-luna", Tokens: runner.AgentTokenCounts{InputTokens: 1000000}, Outcome: conversation.DeliveryCompleted, OccurredAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	for range 2 {
		if err := d.RecordConversationUsage(t.Context(), usage); err != nil {
			t.Fatal(err)
		}
	}
	var balance, count int64
	if err := d.db.QueryRowContext(t.Context(), "SELECT balance_micros,(SELECT count(*) FROM ai_credit_transactions) FROM ai_credit_accounts").Scan(&balance, &count); err != nil || balance != 4900000 || count != 1 {
		t.Fatalf("balance=%d count=%d err=%v", balance, count, err)
	}
	usage.TurnID = "own-provider-turn"
	usage.Provider = "codex"
	if err := d.RecordConversationUsage(t.Context(), usage); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRowContext(t.Context(), "SELECT balance_micros FROM ai_credit_accounts").Scan(&balance); err != nil || balance != 4900000 {
		t.Fatalf("own-provider balance=%d err=%v", balance, err)
	}
}
