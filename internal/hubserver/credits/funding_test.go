package credits

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func TestAICreditAutoFundFailureDoesNotRegressWebhookPayment(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	d, p := fundingFixture(t, repository)
	p.chargeError = billing.ErrPaymentFailed
	p.duringCharge = func(ctx context.Context, request billing.CreditRequest) error {
		p.payment = billing.CreditPayment{Key: request.IdempotencyKey, ID: "pi_autofund_race", PriceID: request.PriceID, USDCents: request.USDCents, Status: "succeeded"}
		if err := d.write(ctx, func(writer tenantstore.Writer) error {
			_, err := writer.CreateBillingEvent(ctx, billingstore.Event{OrganizationID: d.deps.Organization, ID: "evt_autofund_race", Type: "payment_intent.succeeded", ReceivedAt: d.deps.Now().UTC()})
			return err
		}); err != nil {
			return err
		}
		return d.Event(ctx, "evt_autofund_race", "payment_intent.succeeded")
	}
	if err := d.AutoFund(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), d.deps.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
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
		if purchase.State != "paid" || purchase.PaymentID != "pi_autofund_race" || !account.AutoEnabled || account.Failure != "" || account.BalanceMicros != 5000000 || account.ThresholdCents != 100 || account.PriceID != "price_credit" || account.PaymentMethod != "pm_credit" || len(rows) != 1 {
			return errors.New("failed charge response regressed or duplicated the webhook payment")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

type creditProvider struct {
	payment      billing.CreditPayment
	chargeError  error
	duringCharge func(context.Context, billing.CreditRequest) error
}

func (p *creditProvider) SavedCreditPaymentMethod(context.Context, billing.Binding) (string, error) {
	return "pm_credit", nil
}
func (p *creditProvider) CreditCheckout(context.Context, billing.CreditRequest) (billing.Session, error) {
	return billing.Session{}, errors.New("unexpected checkout")
}
func (p *creditProvider) CreditCharge(ctx context.Context, r billing.CreditRequest, _ string) (billing.CreditPayment, error) {
	if p.duringCharge != nil {
		if err := p.duringCharge(ctx, r); err != nil {
			return billing.CreditPayment{}, err
		}
	}
	return billing.CreditPayment{}, p.chargeError
}
func (p *creditProvider) CreditEvent(context.Context, billing.Binding, string) (billing.CreditPayment, error) {
	return p.payment, nil
}

func fundingFixture(t *testing.T, repository tenantstore.Repository) (*Service, *creditProvider) {
	t.Helper()
	d := creditFixture(t, repository, 0)
	now := time.Date(2026, 11, 3, 12, 0, 0, 123456000, time.UTC)
	d.deps.Now = func() time.Time { return now }
	p := &creditProvider{}
	d.deps.Funding = &Funding{Provider: p, CreditPacks: []Pack{{PriceID: "price_credit", Label: "AI credit pack", USDCents: 500}}}
	d.deps.NewID = func() string { return hubapi.NewID("purchase") }
	d.deps.Unbound = errors.New("unbound")
	d.deps.Binding = func(context.Context) (billing.Binding, error) {
		return billing.Binding{OrganizationID: string(d.deps.Organization), AccountID: "acct_example", CustomerID: "cus_example"}, nil
	}
	if err := d.write(t.Context(), func(w tenantstore.Writer) error {
		account, err := w.BillingCreditAccount(t.Context(), billing.ModeTest)
		if err != nil {
			return err
		}
		account.AutoEnabled = true
		account.ThresholdCents = 100
		account.PriceID = "price_credit"
		account.PaymentMethod = "pm_credit"
		if err := w.SetBillingCreditFunding(t.Context(), account); err != nil {
			return err
		}
		return w.CreateCreditPack(t.Context(), billingstore.CreditPack{OrganizationID: d.deps.Organization, Mode: billing.ModeTest, PriceID: "price_credit", USDCents: 500})
	}); err != nil {
		t.Fatal(err)
	}
	return d, p
}
