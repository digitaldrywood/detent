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

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
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
	duringCheckout  func(context.Context, billing.CreditRequest) error
	duringCharge    func(context.Context, billing.CreditRequest) error
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

func (p *hostedCreditProvider) CreditCheckout(ctx context.Context, r billing.CreditRequest) (billing.Session, error) {
	p.mu.Lock()
	p.creditCheckouts = append(p.creditCheckouts, r)
	hook := p.duringCheckout
	if p.checkoutError != nil {
		err := p.checkoutError
		p.mu.Unlock()
		return billing.Session{}, err
	}
	session, ok := p.creditSessions[r.IdempotencyKey]
	if !ok {
		session = billing.Session{ID: "cs_test_credit", URL: "https://checkout.stripe.com/c/pay/test_credit", ExpiresAt: r.ExpiresAt}
		if p.creditSessions == nil {
			p.creditSessions = map[string]billing.Session{}
		}
		p.creditSessions[r.IdempotencyKey] = session
	}
	lost := p.checkoutLoss
	p.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, r); err != nil {
			return billing.Session{}, err
		}
	}
	if lost {
		return billing.Session{}, errors.New("lost response pm_sensitive_secret")
	}
	return session, nil
}

func (p *hostedCreditProvider) CreditEvent(context.Context, billing.Binding, string) (billing.CreditPayment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.payment, nil
}

func (p *hostedCreditProvider) CreditCharge(ctx context.Context, r billing.CreditRequest, _ string) (billing.CreditPayment, error) {
	p.mu.Lock()
	p.chargeKeys = append(p.chargeKeys, r.IdempotencyKey)
	hook, payment, err := p.duringCharge, billing.CreditPayment{Key: r.IdempotencyKey, ID: "pi_auto", PriceID: r.PriceID, USDCents: r.USDCents, Status: p.chargeStatus}, p.chargeError
	p.mu.Unlock()
	if hook != nil {
		if hookErr := hook(ctx, r); hookErr != nil {
			return billing.CreditPayment{}, hookErr
		}
	}
	return payment, err
}

func configureTestCredits(t *testing.T, f *browserHostedFixture, base *hostedBillingProvider, accounts ...billingstore.CreditAccount) *hostedCreditProvider {
	t.Helper()
	p := &hostedCreditProvider{hostedBillingProvider: base, chargeStatus: "succeeded"}
	f.service.config.Hosted.Billing.Provider = p
	f.service.config.Hosted.Billing.CreditPacks = []hostedplans.HostedCreditPack{{PriceID: "price_credit", Label: "AI credit pack", USDCents: 500}}
	if err := hostedplans.ValidateHostedConfig(f.service.config.Hosted); err != nil {
		t.Fatal(err)
	}
	d := f.service.database
	if d.billingAuditStore == nil {
		d.billingAuditStore = &memoryRunDetails{}
	}
	for _, account := range accounts {
		if account.OrganizationID == "" {
			account.OrganizationID = d.hostedOrganization
		}
		if account.Mode == "" {
			account.Mode = billing.ModeTest
		}
		if err := d.billingRepository.WriteTenant(t.Context(), account.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateBillingCreditAccount(t.Context(), account)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := hubapi.FromFeatureError(f.service.database.plansService().ConfigureBilling(t.Context(), hostedplans.PlansInput(f.service.config.Hosted))); err != nil {
		t.Fatal(err)
	}
	return p
}

func postAICreditFixture(t *testing.T, d *database, row billingstore.CreditTransaction) {
	t.Helper()
	if row.OrganizationID == "" {
		row.OrganizationID = d.hostedOrganization
	}
	if row.Mode == "" {
		row.Mode = billing.ModeTest
	}
	if row.Reason != "" {
		ref, err := hubapi.FeatureResult(d.plansService().ArchiveBillingReason(t.Context(), row.Reason))
		if err != nil {
			t.Fatal(err)
		}
		row.ReasonReference, row.Reason = ref, ""
	}
	if err := d.billingRepository.WriteTenant(t.Context(), row.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.PostBillingCredit(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
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

func TestAICreditCheckoutSessionDoesNotRegressWebhookPayment(t *testing.T) {
	t.Parallel()
	service, p, repository := newPostgresAICreditFixture(t, false)
	d := service.database
	p.payment = billing.CreditPayment{ID: "pi_checkout_race", PriceID: "price_credit", USDCents: 500, Status: "succeeded", PaymentMethod: "pm_webhook"}
	p.duringCheckout = func(ctx context.Context, request billing.CreditRequest) error {
		p.payment.Key = request.IdempotencyKey
		if err := hubapi.FromFeatureError(d.plansService().WriteBillingTenant(ctx, func(writer tenantstore.Writer) error {
			_, err := writer.CreateBillingEvent(ctx, billingstore.Event{OrganizationID: d.hostedOrganization, ID: "evt_checkout_race", Type: "checkout.session.completed", ReceivedAt: d.now().UTC()})
			return err
		})); err != nil {
			return err
		}
		return service.billing.creditEvent(ctx, "evt_checkout_race", "checkout.session.completed")
	}
	if _, err := service.checkoutCredits(t.Context(), func(context.Context) (hubapi.APICredential, error) {
		return hubapi.APICredential{Hosted: &auth.HostedIdentity{Subject: "user_browser_owner"}}, nil
	}, "price_credit", "checkout-race"); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), d.hostedOrganization, corestore.Serving, func(reader tenantstore.Reader) error {
		purchase, err := reader.BillingCreditPurchase(t.Context(), p.payment.Key)
		if err != nil {
			return err
		}
		account, err := reader.BillingCreditAccount(t.Context(), billing.ModeTest)
		if err != nil {
			return err
		}
		rows, err := reader.BillingCreditTransactions(t.Context(), billing.ModeTest, 0, 10)
		if err != nil {
			return err
		}
		if purchase.State != "paid" || purchase.PaymentID != "pi_checkout_race" || purchase.Session.ID != "cs_test_credit" || account.BalanceMicros != 5000000 || account.PaymentMethod != "pm_webhook" || account.ThresholdCents != 100 || account.PriceID != "price_credit" || len(rows) != 1 {
			return fmt.Errorf("checkout session update regressed or duplicated the webhook payment: purchase=%+v account=%+v rows=%+v", purchase, account, rows)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func newPostgresAICreditFixture(t *testing.T, automatic bool) (*Service, *hostedCreditProvider, tenantstore.Repository) {
	t.Helper()
	repository := projectRecordPostgres(t)
	f := accountPreferencesPostgresFixture(t, repository)
	plans := hostedplans.PilotPlans()
	f.service.config.Hosted.Plans = &plans
	now := time.Date(2026, 11, 3, 12, 0, 0, 123456000, time.UTC)
	f.service.config.now = func() time.Time { return now }
	d := f.service.database
	d.now = f.service.config.now
	d.aiCreditMode = billing.ModeTest
	d.billingAuditStore = &memoryRunDetails{}
	base := &hostedBillingProvider{snapshot: billing.Snapshot{Status: "free"}}
	p := &hostedCreditProvider{hostedBillingProvider: base, chargeStatus: "succeeded"}
	f.service.config.Hosted.Billing = &hostedplans.HostedBillingConfig{
		AccountID: "acct_fixture", CustomerID: "cus_fixture", Mode: billing.ModeTest, PortalConfigurationID: "bpc_fixture", WebhookSecret: []byte("whsec_fixture_2196_secret"), GraceSeconds: 3600, ReconcileSeconds: 60, Provider: p,
		Prices:      []hostedplans.HostedBillingPrice{{PriceID: "price_fixture", Label: "Extended pilot", Plan: plans.Plans[1].PlanReference}},
		CreditPacks: []hostedplans.HostedCreditPack{{PriceID: "price_credit", Label: "AI credit pack", USDCents: 500}},
	}
	for _, organization := range []tracker.OrganizationID{d.hostedOrganization, "org_credit_other"} {
		seedHostedBillingRecordAccount(t, repository, organization, billingstore.Account{
			OrganizationID: organization, Mode: billing.ModeTest,
			Binding: billing.Binding{OrganizationID: string(organization), AccountID: "acct_fixture", CustomerID: "cus_fixture"}, Status: "free",
		})
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(writer tenantstore.Writer) error {
			if err := writer.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: organization, Mode: billing.ModeTest, AutoEnabled: automatic, ThresholdCents: 100, PriceID: "price_credit", PaymentMethod: "pm_credit"}); err != nil {
				return err
			}
			return writer.CreateCreditPack(t.Context(), billingstore.CreditPack{OrganizationID: organization, Mode: billing.ModeTest, PriceID: "price_credit", USDCents: 500})
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := repository.ReadTenant(context.WithoutCancel(t.Context()), "org_credit_other", corestore.Serving, func(reader tenantstore.Reader) error {
			account, err := reader.BillingCreditAccount(context.WithoutCancel(t.Context()), billing.ModeTest)
			if err == nil && (account.BalanceMicros != 0 || account.AutoEnabled != automatic || account.PaymentMethod != "pm_credit") {
				return fmt.Errorf("other organization credit account changed: %+v", account)
			}
			return err
		}); err != nil {
			t.Error(err)
		}
	})
	done := make(chan struct{})
	close(done)
	f.service.billing = &hostedBillingWorker{service: f.service, cancel: func() {}, done: done}
	return f.service, p, repository
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
			f.service.config.Hosted.Billing = &hostedplans.HostedBillingConfig{}
			d.billingAuditStore = &memoryRunDetails{}
			if err := hubapi.FromFeatureError(d.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
				return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: d.hostedOrganization, Mode: billing.ModeTest})
			})); err != nil {
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
			d.billingAuditStore = &memoryRunDetails{}
			if !test.missing {
				initial, live := test.balance, int64(999000000)
				if test.history {
					initial -= 10000000 + 5000000 + 25000000 - 1000000 - 750000 - 9000000 - 9000000 + 51*2000000 + 50*51/2
					live += 9000000
				}
				if err := hubapi.FromFeatureError(d.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					if err := w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: d.hostedOrganization, Mode: billing.ModeTest, BalanceMicros: initial, AutoEnabled: test.history}); err != nil {
						return err
					}
					return w.CreateBillingCreditAccount(t.Context(), billingstore.CreditAccount{OrganizationID: d.hostedOrganization, Mode: billing.ModeLive, BalanceMicros: live})
				})); err != nil {
					t.Fatal(err)
				}
			}
			if test.history {
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
					postAICreditFixture(t, d, billingstore.CreditTransaction{OrganizationID: tracker.OrganizationID(item.organization), Mode: item.mode, Source: fmt.Sprintf("credit-read-%d", index), AmountMicros: item.amount, Kind: item.kind, Actor: "staff@example.test", Reason: "Pilot credits", RecordedAt: at})
				}
				for index := range 51 {
					postAICreditFixture(t, d, billingstore.CreditTransaction{Source: fmt.Sprintf("earlier-grant-%d", index), AmountMicros: int64(2000000 + index), Kind: "complimentary", Actor: "staff@example.test", Reason: "Earlier grant", RecordedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)})
				}
			}
			owner := f.member(t, "owner", "owner", "")
			if test.failure {
				if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), "REVOKE SELECT ON ai_credit_accounts FROM "+postgres.RuntimeRoleName); err != nil {
					t.Fatal(err)
				}
			}
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
					err := hubapi.FromFeatureError(d.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
						account, err := r.BillingCreditAccount(t.Context(), billing.ModeLive)
						liveBalance = account.BalanceMicros
						return err
					}))
					if err != nil || liveBalance != 999000000 {
						t.Fatalf("live balance=%d err=%v", liveBalance, err)
					}
				} else if result.Spent != 0 || result.Auto || len(result.History) != 0 {
					t.Fatalf("empty credits=%+v", result)
				}

			}
		})
	}
}

func TestHostedUsageUsesSelectedPostgresWindowsAndRefusesSQLiteWrites(t *testing.T) {
	repository := projectRecordPostgres(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	plans := hostedplans.HostedPlansConfig{WindowSeconds: 60, RetentionWindows: 10}
	window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
	for index, organization := range []tracker.OrganizationID{"org_usage_first", "org_usage_second"} {
		want := int64(index + 1)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), CreatedAt: now}); err != nil {
				return err
			}
			_, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: organization, ID: "request_example", WindowStart: window, Metric: "api_mutations", Amount: want}, 10)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	for index, organization := range []tracker.OrganizationID{"org_usage_first", "org_usage_second"} {
		t.Run(string(organization), func(t *testing.T) {
			d := &database{billingRepository: repository, hostedOrganization: organization, now: func() time.Time { return now }}
			d.hostedRuntime.Store(&hostedplans.RuntimeConfig{Plans: plans})
			want := int64(index + 1)
			for range 2 {
				usage, err := hubapi.FeatureResult(d.plansService().Consumption(t.Context(), nil, now, "usage_windows"))
				if err != nil || usage["api_mutations"] != want {
					t.Fatalf("PostgreSQL usage window=%v error=%v, want %d", usage, err, want)
				}
				storage, err := hubapi.FeatureResult(d.plansService().Consumption(t.Context(), nil, now, "collaboration_bytes"))
				if err != nil || len(storage) != 1 || storage["collaboration_bytes"] != 0 {
					t.Fatalf("PostgreSQL collaboration usage=%v error=%v", storage, err)
				}
				if _, err := hubapi.FeatureResult(d.plansService().Consumption(t.Context(), nil, now, "unsupported_metric")); !errors.Is(err, hostedplans.ErrBillingUsageUnavailable) {
					t.Fatalf("unsupported PG usage read error=%v", err)
				}
				if err := hubapi.FromFeatureError(d.plansService().RecordUsage(t.Context(), nil, now, "api_mutations", 1)); !errors.Is(err, hostedplans.ErrBillingUsageUnavailable) {
					t.Fatalf("selected PG usage write error=%v", err)
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
		included     bool
	}{
		{"included above threshold", "succeeded", 2000000, nil, 0, true, 2000000, true},
		{"included below threshold", "succeeded", 0, nil, 0, true, 0, true},
		{"above threshold", "succeeded", 2000000, nil, 0, true, 2000000, false},
		{"at threshold", "succeeded", 1000000, nil, 0, true, 1000000, false},
		{"below threshold", "succeeded", 999999, nil, 1, true, 5999999, false},
		{"one in flight", "processing", 0, nil, 2, true, 0, false},
		{"failed disables", "requires_payment_method", 0, nil, 1, false, 0, false},
		{"authentication disables", "requires_action", 0, nil, 1, false, 0, false},
		{"decline disables", "", 0, billing.ErrPaymentFailed, 1, false, 0, false},
		{"uncertain retains key", "", 0, errors.New("lost response"), 2, true, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, base := newHostedBillingFixture(t)
			p := configureTestCredits(t, f, base, billingstore.CreditAccount{BalanceMicros: test.balance, AutoEnabled: true, ThresholdCents: 100, PriceID: "price_credit", PaymentMethod: "pm_credit"})
			p.chargeStatus, p.chargeError = test.status, test.chargeError
			if test.included {
				d := f.service.database
				plans := hostedplans.CapacityPlans()
				plans.Plans = append(plans.Plans, f.service.config.Hosted.Plans.Plans...)
				if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
					t.Fatal(err)
				}
				now := f.service.config.now()
				setIncludedCreditSubscription(t, d, d.hostedOrganization, now.Add(-time.Hour), now.Add(time.Hour), now.Add(time.Hour), hostedplans.PlanReference{ID: "team", Version: 2})
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
				err := hubapi.FromFeatureError(f.service.database.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					return hubapi.FromFeatureError(f.service.database.creditsService().ApplyPayment(t.Context(), w, billing.CreditPayment{Key: p.chargeKeys[0], ID: "pi_auto", PriceID: "price_credit", USDCents: 500, Status: "requires_payment_method"}))
				}))
				if err != nil {
					t.Fatal(err)
				}
				updated, err := f.service.readAICredits(t.Context())
				if err != nil || !updated.AutoEnabled {
					t.Fatalf("stale failure disabled re-enabled auto-fund: %+v %v", updated, err)
				}
			}
			if test.status == "processing" {
				err := hubapi.FromFeatureError(f.service.database.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					return w.CreateBillingCreditPurchase(t.Context(), billingstore.CreditPurchase{OrganizationID: f.service.database.hostedOrganization, Key: "duplicate_auto", Mode: billing.ModeTest, PriceID: "price_credit", USDCents: 500, Automatic: true, State: "pending", CreatedAt: f.service.config.now().UTC()})
				}))
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
	if err := hubapi.FromFeatureError(f.service.database.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
		account, err := w.BillingCreditAccount(t.Context(), billing.ModeTest)
		if err != nil {
			return err
		}
		account.PaymentMethod = ""
		return w.SetBillingCreditFunding(t.Context(), account)
	})); err != nil {
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

func TestAICreditCSVExport(t *testing.T) {
	t.Parallel()
	f, base := newHostedBillingFixture(t)
	configureTestCredits(t, f, base, billingstore.CreditAccount{BalanceMicros: 4*1234567 - 234567})
	d := f.service.database
	at := time.Date(2026, 10, 10, 12, 0, 0, 123456, time.UTC)
	f.conversation = "credit-export-conversation"
	if _, err := hostedPostgresAdmin(t, f.service).ExecContext(t.Context(), `INSERT INTO conversations(id,organization_id,project_id,owner_principal_id,origin,visibility,status,execution_status,execution_lease_id,execution_settled,created_at,updated_at) VALUES($1,$2,$3,'test_hub_admin','user','private','active','idle','',TRUE,$4,$4)`, f.conversation, d.hostedOrganization, f.project, at); err != nil {
		t.Fatal(err)
	}
	if err := hubapi.FromFeatureError(d.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
		price := billingstore.ConversationPrice{OrganizationID: d.hostedOrganization, ID: at.UnixMicro(), Provider: "openai", Model: "example-model", EffectiveAt: at.Add(-time.Hour)}
		if err := w.CreateConversationPrice(t.Context(), price); err != nil {
			return err
		}
		_, err := w.RecordConversationUsage(t.Context(), billingstore.ConversationUsage{OrganizationID: d.hostedOrganization, ProjectID: tracker.ProjectID(f.project), ConversationID: f.conversation, TurnID: "turn-0", Provider: "openai", Model: "example-model", OccurredAt: at, Input: 100, CachedInput: 20, Output: 50, ReasoningOutput: 10, Outcome: "completed", PriceID: &price.ID, CostUSD: new(999.0), CacheSavingsUSD: new(0.0)})
		return err
	})); err != nil {
		t.Fatal(err)
	}
	for index, kind := range []string{"usage", "purchase", "auto_fund", "complimentary"} {
		source := fmt.Sprintf("%s:turn-%d", kind, index)
		postAICreditFixture(t, d, billingstore.CreditTransaction{Source: source, AmountMicros: -1234567, IncludedAmountMicros: map[string]int64{"usage": 234567}[kind], Kind: kind, RecordedAt: at})
	}
	requireNativeStatus(t, f.billingAPI(t, "owner", http.MethodGet, "/billing/credits/transactions?format=csv&from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z", ""), http.StatusOK)
	requireNativeStatus(t, f.billingAPI(t, "wrong-organization", http.MethodGet, "/billing/credits/transactions?format=csv&from=2026-10-01T00:00:00Z&to=2026-11-01T00:00:00Z", ""), http.StatusForbidden)
	var audits int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), `SELECT count(*) FROM hosted_audit WHERE organization_id=$1 AND event='billing_exported'`, d.hostedOrganization).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 1 {
		t.Fatalf("export audits=%d", audits)
	}
}

func setIncludedCreditSubscription(t *testing.T, d *database, organization tracker.OrganizationID, start, end, access time.Time, ref hostedplans.PlanReference) {
	t.Helper()
	if err := d.billingRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		account, err := w.BillingAccount(t.Context())
		if errors.Is(err, corestore.ErrNotFound) {
			account = billingstore.Account{OrganizationID: organization, Mode: billing.ModeTest, Binding: billing.Binding{OrganizationID: string(organization), AccountID: "acct_" + string(organization), CustomerID: "cus_" + string(organization)}}
		} else if err != nil {
			return err
		}
		account.Status = "active"
		account.Plan = billingstore.PlanReference{ID: ref.ID, Version: ref.Version}
		account.AccessUntil, account.PaidThrough = access, end
		account.Snapshot = billing.Snapshot{SubscriptionID: "sub_" + string(organization), Status: "active", PeriodStart: start, PeriodEnd: end}
		if err := w.SetBillingAccount(t.Context(), account); err != nil {
			return err
		}
		assignment, err := w.BillingAssignment(t.Context())
		if err != nil {
			return err
		}
		assignment.Subscription = &account.Plan
		assignment.ExpiresAt = &access
		return w.SetBillingAssignment(t.Context(), assignment, assignment.Revision)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIncludedAICreditAllowance(t *testing.T) {
	t.Parallel()
	f, base := newHostedBillingFixture(t)
	configureTestCredits(t, f, base)
	d := f.service.database
	plans := hostedplans.CapacityPlans()
	plans.Plans = append(plans.Plans, f.service.config.Hosted.Plans.Plans...)
	if err := hubapi.FromFeatureError(d.plansService().ConfigurePlans(t.Context(), hostedplans.PlansInput(&hostedplans.HostedConfig{Plans: &plans}))); err != nil {
		t.Fatal(err)
	}
	now := f.service.config.now()
	for _, test := range []struct {
		name   string
		ref    hostedplans.PlanReference
		paid   bool
		want   int64
		custom bool
	}{
		{"Starter", hostedplans.PlanReference{ID: "starter", Version: 1}, true, 4900000, false},
		{"Team", hostedplans.PlanReference{ID: "team", Version: 2}, true, 9900000, false},
		{"Growth", hostedplans.PlanReference{ID: "growth", Version: 2}, true, 24900000, false},
		{"Scale", hostedplans.PlanReference{ID: "scale", Version: 2}, true, 0, true},
		{"Free", hostedplans.PlanReference{ID: "free", Version: 1}, true, 0, false},
		{"pilot", hostedplans.PlanReference{ID: "pilot_free", Version: 1}, true, 0, false},
		{"complimentary team", hostedplans.PlanReference{ID: "comp_team", Version: 1}, true, 0, false},
		{"complimentary grant", hostedplans.PlanReference{ID: "team", Version: 2}, false, 0, false},
		{"legacy growth", hostedplans.PlanReference{ID: "growth", Version: 1}, true, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			access := now.AddDate(0, 1, 0)
			if !test.paid {
				access = now
			}
			setIncludedCreditSubscription(t, d, d.hostedOrganization, now, now.AddDate(0, 1, 0), access, test.ref)
			if !test.paid {
				grant := hostedplans.HostedGrant{ID: "included-credit-grant", Plan: test.ref, Scope: []string{"concurrent_work"}, StartsAt: now}
				if err := hubapi.FromFeatureError(d.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
					return w.CreateBillingGrant(t.Context(), billingstore.Grant{OrganizationID: d.hostedOrganization, ID: grant.ID, Plan: billingstore.PlanReference{ID: grant.Plan.ID, Version: grant.Plan.Version}, Scope: grant.Scope, StartsAt: grant.StartsAt})
				})); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := hubapi.FromFeatureError(d.plansService().WriteBillingTenant(t.Context(), func(w tenantstore.Writer) error {
						return w.RevokeBillingGrant(t.Context(), grant.ID, now)
					})); err != nil {
						t.Error(err)
					}
				}()
			}
			var entitlement hostedplans.HostedEntitlement
			err := hubapi.FromFeatureError(d.plansService().ReadBillingTenant(t.Context(), func(r tenantstore.Reader) error {
				var err error
				entitlement, err = hubapi.FeatureResult(d.plansService().EntitlementRecord(t.Context(), r, now))
				return err
			}))
			if err != nil {
				t.Fatal(err)
			}
			got := entitlement.IncludedAICreditMicros
			if test.custom {
				if got != nil {
					t.Fatalf("custom allowance=%v", *got)
				}
			} else if got == nil || *got != test.want {
				t.Fatalf("allowance=%v want=%d", got, test.want)
			}
		})
	}
	view, err := f.service.readBillingView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"free": 0, "starter": 4900000, "team": 9900000, "growth": 24900000}
	if len(view.ComparisonPlans) != 5 {
		t.Fatalf("comparisons=%+v", view.ComparisonPlans)
	}
	for _, plan := range view.ComparisonPlans {
		if plan.ID == "scale" {
			if plan.IncludedAICreditMicros != nil {
				t.Fatal("Scale must be custom")
			}
			continue
		}
		if plan.IncludedAICreditMicros == nil || *plan.IncludedAICreditMicros != want[plan.ID] {
			t.Fatalf("comparison=%+v", plan)
		}
	}
}
