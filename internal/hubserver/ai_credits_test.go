package hubserver

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
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

func TestPlatformAICreditAdjustments(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, amount, reason string
		initial, wantBalance int64
		conflict, rollback   bool
		wantError            bool
	}{
		{name: "complimentary grant", amount: "10.25", reason: "design partner", wantBalance: 10250000},
		{name: "correction", amount: "-2.50", reason: "correction", initial: 10000000, wantBalance: 7500000},
		{name: "micro precision", amount: "0.000001", reason: "rounding correction", wantBalance: 1},
		{name: "conflicting replay", amount: "10", reason: "design partner", wantBalance: 10000000, conflict: true},
		{name: "zero", amount: "0", reason: "invalid", wantError: true},
		{name: "missing reason", amount: "10", reason: "  ", wantError: true},
		{name: "excess precision", amount: "0.0000001", reason: "invalid", wantError: true},
		{name: "overflow", amount: "1", reason: "invalid", initial: math.MaxInt64, wantBalance: math.MaxInt64, wantError: true},
		{name: "underflow", amount: "-1", reason: "invalid", initial: math.MinInt64, wantBalance: math.MinInt64, wantError: true},
		{name: "failed balance write rolls back ledger", amount: "10", reason: "design partner", rollback: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f, base := newHostedBillingFixture(t)
			configureTestCredits(t, f, base)
			d := f.service.database
			now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
			d.now = func() time.Time { return now }
			if _, err := d.db.ExecContext(t.Context(), "UPDATE ai_credit_accounts SET balance_micros=?", test.initial); err != nil {
				t.Fatal(err)
			}
			if test.rollback {
				if _, err := d.db.ExecContext(t.Context(), "CREATE TRIGGER reject_credit_update BEFORE UPDATE ON ai_credit_accounts BEGIN SELECT RAISE(ABORT,'balance write failed'); END"); err != nil {
					t.Fatal(err)
				}
			}
			adjustment := billing.CreditAdjustment{IdempotencyKey: "comp-credit-1", AmountUSD: test.amount, Reason: test.reason}
			for range 2 {
				balance, err := d.adjustAICredits(t.Context(), "staff@example.test", adjustment)
				if (err != nil) != test.wantError || err == nil && balance != test.wantBalance {
					t.Fatalf("balance=%d err=%v", balance, err)
				}
			}
			if test.conflict {
				for _, changed := range []billing.CreditAdjustment{
					{IdempotencyKey: adjustment.IdempotencyKey, AmountUSD: "11", Reason: adjustment.Reason},
					{IdempotencyKey: adjustment.IdempotencyKey, AmountUSD: adjustment.AmountUSD, Reason: "different reason"},
				} {
					_, err := d.adjustAICredits(t.Context(), "staff@example.test", changed)
					var conflict *nativeError
					if !errors.As(err, &conflict) || conflict.status != http.StatusConflict {
						t.Fatalf("conflicting replay=%v", err)
					}
				}
			}
			view, err := f.service.readAICredits(t.Context())
			if err != nil || view.BalanceMicros != test.wantBalance {
				t.Fatalf("credits=%+v err=%v", view, err)
			}
			if test.wantError {
				if len(view.History) != 0 {
					t.Fatalf("rejected adjustment has ledger rows: %+v", view.History)
				}
				return
			}
			if len(view.History) != 1 {
				t.Fatalf("history=%+v", view.History)
			}
			item := view.History[0]
			if item.AmountMicros != test.wantBalance-test.initial || item.Kind != "complimentary" || item.Actor != "staff@example.test" || item.Reason != test.reason || !item.At.Equal(d.now()) {
				t.Fatalf("ledger=%+v", item)
			}
		})
	}
}

func TestPlatformAICreditServiceAuthority(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, kind, bearer string
		identity           bool
		want               int
	}{
		{"organization owner key", cloudassert.KindMachine, "org-owner-key", false, http.StatusNotFound},
		{"non-admin key", cloudassert.KindMachine, "member-key", false, http.StatusNotFound},
		{"browser identity", cloudassert.KindBrowser, "", true, http.StatusNotFound},
		{"anonymous service assertion", cloudassert.KindService, "", false, http.StatusForbidden},
		{"entry-authorized administrator", cloudassert.KindService, "", true, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSharedFixture(t)
			d := f.service.database
			d.aiCreditMode = billing.ModeTest
			f.service.config.Hosted.Billing = &HostedBillingConfig{}
			if _, err := d.db.ExecContext(t.Context(), "INSERT INTO ai_credit_accounts(organization_id,mode) VALUES(?,'test')", d.hostedOrganization); err != nil {
				t.Fatal(err)
			}
			owner := f.member(t, "owner", "owner", "")
			var user *hostedSecurityUser
			if test.identity {
				user = &owner
			}
			for range 2 {
				response := f.serve(t, hostedSharedRequest{user: user, kind: test.kind, bearer: test.bearer, method: http.MethodPost, target: "/internal/v1/platform/ai-credits", body: `{"idempotency_key":"platform-credit","amount_usd":"5","reason":"complimentary pilot"}`})
				requireNativeStatus(t, response, test.want)
			}
			view, err := f.service.readAICredits(t.Context())
			if err != nil || test.want == http.StatusOK && view.BalanceMicros != 5000000 || test.want != http.StatusOK && view.BalanceMicros != 0 {
				t.Fatalf("credits=%+v err=%v", view, err)
			}
		})
	}
}

func TestPlatformAICreditBalanceRead(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, kind, bearer        string
		balance                   int64
		missing, failure, history bool
		want                      int
	}{
		{name: "bounded history and monthly spend", kind: cloudassert.KindService, balance: 10250000, history: true, want: http.StatusOK},
		{name: "positive balance", kind: cloudassert.KindService, balance: 10250000, want: http.StatusOK},
		{name: "zero balance", kind: cloudassert.KindService, want: http.StatusOK},
		{name: "negative balance", kind: cloudassert.KindService, balance: -2500000, want: http.StatusOK},
		{name: "micro precision", kind: cloudassert.KindService, balance: 1, want: http.StatusOK},
		{name: "missing account", kind: cloudassert.KindService, missing: true, want: http.StatusInternalServerError},
		{name: "database failure", kind: cloudassert.KindService, failure: true, want: http.StatusInternalServerError},
		{name: "owner key", kind: cloudassert.KindMachine, bearer: "org-owner-key", want: http.StatusNotFound},
		{name: "member key", kind: cloudassert.KindMachine, bearer: "member-key", want: http.StatusNotFound},
		{name: "browser member", kind: cloudassert.KindBrowser, want: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newHostedSharedFixture(t)
			d := f.service.database
			d.aiCreditMode = billing.ModeTest
			d.now = func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) }
			if !test.missing {
				if _, err := d.db.ExecContext(t.Context(), "INSERT INTO ai_credit_accounts(organization_id,mode,balance_micros) VALUES(?,'test',?),(?,'live',999000000)", d.hostedOrganization, test.balance, d.hostedOrganization); err != nil {
					t.Fatal(err)
				}
			}
			if test.history {
				if _, err := d.db.ExecContext(t.Context(), "UPDATE ai_credit_accounts SET auto_enabled=1 WHERE organization_id=? AND mode='test'", d.hostedOrganization); err != nil {
					t.Fatal(err)
				}
				for index, item := range []struct {
					organization, mode, kind, date string
					amount                         int64
				}{
					{string(d.hostedOrganization), "test", "complimentary", "2026-09-30", 10000000},
					{string(d.hostedOrganization), "test", "purchase", "2026-10-01", 5000000},
					{string(d.hostedOrganization), "test", "auto_fund", "2026-10-02", 25000000},
					{string(d.hostedOrganization), "test", "complimentary", "2026-10-03", -1000000},
					{string(d.hostedOrganization), "test", "usage", "2026-10-01", -750000},
					{string(d.hostedOrganization), "test", "usage", "2026-09-30", -9000000},
					{string(d.hostedOrganization), "test", "usage", "2026-11-01", -9000000},
					{string(d.hostedOrganization), "live", "usage", "2026-10-02", -9000000},
				} {
					at, err := time.Parse("2006-01-02", item.date)
					if err != nil {
						t.Fatal(err)
					}
					if _, err := d.db.ExecContext(t.Context(), "INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,actor,reason,recorded_at) VALUES(?,?,?,?,?,?,?,?)", item.organization, item.mode, fmt.Sprintf("credit-read-%d", index), item.amount, item.kind, "staff@example.test", "Pilot credits", at.UnixMicro()); err != nil {
						t.Fatal(err)
					}
				}
				for index := range 51 {
					if _, err := d.db.ExecContext(t.Context(), "INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,actor,reason,recorded_at) VALUES(?,'test',?,?,'complimentary','staff@example.test','Earlier grant',?)", d.hostedOrganization, fmt.Sprintf("earlier-grant-%d", index), 2000000+index, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).UnixMicro()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if test.failure {
				if _, err := d.db.ExecContext(t.Context(), "DROP TABLE ai_credit_accounts"); err != nil {
					t.Fatal(err)
				}
			}
			owner := f.member(t, "owner", "owner", "")
			response := f.serve(t, hostedSharedRequest{user: &owner, kind: test.kind, bearer: test.bearer, method: http.MethodPost, target: "/internal/v1/platform/ai-credits/balance", body: `{}`})
			requireNativeStatus(t, response, test.want)
			if test.want == http.StatusOK {
				var result struct {
					Balance int64                 `json:"balance_micros"`
					Spent   int64                 `json:"spent_micros"`
					Month   string                `json:"month"`
					Auto    bool                  `json:"auto_enabled"`
					History []aiCreditTransaction `json:"history"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Balance != test.balance || result.Month != "2026-10" || result.History == nil {
					t.Fatalf("credits response=%s err=%v", response.Body.String(), err)
				}
				if test.history {
					if result.Spent != 750000 || !result.Auto || len(result.History) != 50 || result.History[0].AmountMicros != -1000000 || result.History[1].Kind != "auto_fund" || result.History[2].Kind != "purchase" || result.History[3].Kind != "complimentary" || result.History[4].AmountMicros != 2000050 || result.History[49].AmountMicros != 2000005 || result.History[49].Reason != "Earlier grant" || result.History[0].Actor != "staff@example.test" || result.History[0].Reason != "Pilot credits" || result.History[0].At.Format("2006-01-02") != "2026-10-03" {
						t.Fatalf("credits=%+v", result)
					}
					var liveBalance int64
					if err := d.db.QueryRowContext(t.Context(), "SELECT balance_micros FROM ai_credit_accounts WHERE organization_id=? AND mode='live'", d.hostedOrganization).Scan(&liveBalance); err != nil || liveBalance != 999000000 {
						t.Fatalf("live balance=%d err=%v", liveBalance, err)
					}
				} else if result.Spent != 0 || result.Auto || len(result.History) != 0 {
					t.Fatalf("empty credits=%+v", result)
				}

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

func TestAICreditGroupedHistory(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{1, 10, 100} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			f, base := newHostedBillingFixture(t)
			configureTestCredits(t, f, base)
			d := f.service.database
			start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for day := range 35 {
				for turn := range 2 {
					if _, err := d.db.ExecContext(t.Context(), `INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,'test',?,-123,'usage',?)`, d.hostedOrganization, fmt.Sprintf("usage:day-%d-turn-%d", day, turn), start.AddDate(0, 0, day).Add(time.Duration(turn)*time.Hour).UnixMicro()); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, kind := range []string{"purchase", "auto_fund", "complimentary"} {
				for n := range 2 {
					if _, err := d.db.ExecContext(t.Context(), `INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,'test',?,?,?,?)`, d.hostedOrganization, fmt.Sprintf("%s-%d", kind, n), 5000000+n, kind, start.AddDate(0, 0, 34).Add(2*time.Hour).UnixMicro()); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := d.db.ExecContext(t.Context(), `INSERT INTO ai_credit_accounts(organization_id,mode,balance_micros) VALUES(?,'live',0)`, d.hostedOrganization); err != nil {
				t.Fatal(err)
			}
			if _, err := d.db.ExecContext(t.Context(), `INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,'live','other-mode',999,'complimentary',?)`, d.hostedOrganization, start.UnixMicro()); err != nil {
				t.Fatal(err)
			}
			page := creditPageRequest{Limit: limit, From: start.Format(time.RFC3339), To: start.AddDate(0, 2, 0).Format(time.RFC3339)}
			seen := map[string]bool{}
			counts := map[string]int{}
			for index := 0; ; index++ {
				view, err := f.service.readAICredits(t.Context(), page)
				if err != nil {
					t.Fatal(err)
				}
				if len(view.History) == 0 || len(view.History) > limit || view.TotalRows != 41 || view.BalanceMicros != 0 {
					t.Fatalf("page=%+v", view)
				}
				for _, row := range view.History {
					counts[row.Kind]++
					if row.Kind == "usage" {
						day := row.At.Format("2006-01-02")
						if seen[day] || row.Turns != 2 || row.AmountMicros != -246 {
							t.Fatalf("duplicate or ungrouped usage=%+v", row)
						}
						seen[day] = true
					} else {
						key := fmt.Sprintf("%s-%d", row.Kind, row.AmountMicros)
						if row.AmountMicros < 5000000 || row.AmountMicros > 5000001 || row.Turns != 0 || seen[key] {
							t.Fatalf("duplicate or merged credit=%+v", row)
						}
						seen[key] = true
					}
				}
				if view.NextCursor == "" {
					break
				}
				if index == 0 {
					if _, err := d.db.ExecContext(t.Context(), `INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,'test','usage:later',-999,'usage',?)`, d.hostedOrganization, start.AddDate(0, 0, 1).UnixMicro()); err != nil {
						t.Fatal(err)
					}
				}
				page.Cursor = view.NextCursor
				if index > 41 {
					t.Fatal("cursor did not end")
				}
			}
			if counts["usage"] != 35 || counts["purchase"] != 2 || counts["auto_fund"] != 2 || counts["complimentary"] != 2 {
				t.Fatalf("counts=%v", counts)
			}
			for _, query := range []string{"?limit=0", "?limit=101", "?cursor=bad", "?from=bad", "?from=2026-02-01T00:00:00Z&to=2026-01-01T00:00:00Z"} {
				requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodGet, "/billing/credits/transactions"+query, ""), http.StatusUnprocessableEntity)
			}
			response := f.billingAPI(t, "owner", http.MethodGet, "/billing/credits/transactions?limit=1", "")
			requireNativeStatus(t, response, http.StatusOK)
			var first aiCreditView
			if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
				t.Fatal(err)
			}
			response = f.billingAPI(t, "owner", http.MethodGet, "/billing/credits/transactions?limit=1&cursor="+url.QueryEscape(first.NextCursor), "")
			requireNativeStatus(t, response, http.StatusOK)
			var second aiCreditView
			if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
				t.Fatal(err)
			}
			if len(second.History) != 1 || second.NextCursor == first.NextCursor {
				t.Fatalf("second page=%+v", second)
			}
		})
	}
}

func TestAICreditCSVExport(t *testing.T) {
	t.Parallel()
	f, base := newHostedBillingFixture(t)
	configureTestCredits(t, f, base)
	d := f.service.database
	at := time.Date(2026, 10, 10, 12, 0, 0, 123456, time.UTC)
	if _, err := d.db.ExecContext(t.Context(), `INSERT INTO conversation_usage(organization_id,project_id,conversation_id,turn_id,provider,model,occurred_at,input,cached_input,output,reasoning_output,outcome,cost_usd) VALUES(?,?,?,'turn-0','openai','example-model',?,100,20,50,10,'completed',999)`, d.hostedOrganization, "project_a", "conversation_a", at.UnixMicro()); err != nil {
		t.Fatal(err)
	}
	for index, kind := range []string{"usage", "purchase", "auto_fund", "complimentary"} {
		source := fmt.Sprintf("%s:turn-%d", kind, index)
		if _, err := d.db.ExecContext(t.Context(), `INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,'test',?,-1234567,?,?)`, d.hostedOrganization, source, kind, at.UnixMicro()); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		role string
		want int
	}{{"owner", http.StatusOK}, {"viewer", http.StatusForbidden}, {"support-viewer", http.StatusForbidden}, {"wrong-organization", http.StatusForbidden}} {
		t.Run(test.role, func(t *testing.T) {
			response := f.billingAPI(t, test.role, http.MethodGet, "/billing/credits/transactions?format=csv&from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z", "")
			requireNativeStatus(t, response, test.want)
			if test.want != http.StatusOK {
				return
			}
			records, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 5 || strings.Join(records[0], ",") != "recorded_at,kind,turn_id,tokens,charged_amount_usd" {
				t.Fatalf("records=%v", records)
			}
			for _, row := range records[1:] {
				if row[0] != "2026-10-10T12:00:00.000123Z" || row[4] != "-1.234567" {
					t.Fatalf("row=%v", row)
				}
				if row[1] == "usage" && (row[2] != "turn-0" || row[3] != "150") || row[1] != "usage" && (row[2] != "" || row[3] != "") {
					t.Fatalf("turn=%v", row)
				}
			}
			if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/csv") || !strings.Contains(response.Header().Get("Content-Disposition"), "credit-transactions.csv") {
				t.Fatalf("headers=%v", response.Header())
			}
		})
	}
	var audits int
	if err := d.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit WHERE event='billing_exported'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("export audits=%d", audits)
	}
	response := f.billingAPI(t, "owner", http.MethodGet, "/billing/credits/transactions?format=csv&from=2026-11-01T00:00:00Z", "")
	requireNativeStatus(t, response, http.StatusOK)
	records, err := csv.NewReader(strings.NewReader(response.Body.String())).ReadAll()
	if err != nil || len(records) != 1 {
		t.Fatalf("period rows=%v err=%v", records, err)
	}
}
