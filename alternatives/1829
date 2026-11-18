package credits

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

type Funding struct {
	Provider         billing.CreditProvider
	CheckoutDisabled bool
	CreditPacks      []Pack
}

func (s *Service) CreditPack(id string) (Pack, bool) {
	if cfg := s.deps.Funding; cfg != nil && !cfg.CheckoutDisabled {
		for _, pack := range cfg.CreditPacks {
			if pack.PriceID == id {
				return pack, true
			}
		}
	}
	return Pack{}, false
}

func (s *Service) Event(ctx context.Context, id, kind string) error {
	cfg := s.deps.Funding
	if cfg == nil || s.deps.Mode == "" || (kind != "checkout.session.completed" && kind != "checkout.session.async_payment_succeeded" && kind != "payment_intent.succeeded" && kind != "payment_intent.payment_failed" && kind != "payment_intent.canceled") {
		return nil
	}
	binding, err := s.deps.Binding(ctx)
	if errors.Is(err, s.deps.Unbound) {
		return nil
	}
	if err != nil {
		return err
	}
	provider, ok := cfg.Provider, cfg.Provider != nil
	if !ok {
		return errors.New("AI credit purchases are unavailable")
	}
	payment, err := provider.CreditEvent(ctx, binding, id)
	if err != nil {
		return err
	}

	return s.write(ctx, func(writer tenantstore.Writer) error {
		event, err := writer.BillingEvent(ctx, id)
		if err != nil {
			return err
		}
		if event.CreditProcessed {
			return nil
		}
		if err := s.ApplyPayment(ctx, writer, payment); err != nil {
			return err
		}
		return writer.SetBillingEventCreditProcessed(ctx, id)
	})
}

func (s *Service) AutoFund(ctx context.Context) error {
	cfg := s.deps.Funding
	if cfg == nil || s.deps.Mode == "" {
		return nil
	}

	return s.autoFund(ctx, cfg)
}

func (s *Service) autoFund(ctx context.Context, cfg *Funding) error {
	var purchase billingstore.CreditPurchase
	err := s.read(ctx, func(reader tenantstore.Reader) error {
		var err error
		purchase, err = reader.BillingPendingCreditPurchase(ctx, s.deps.Mode, true)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		var account billingstore.CreditAccount
		err = s.read(ctx, func(reader tenantstore.Reader) error {
			var err error
			account, err = reader.BillingCreditAccount(ctx, s.deps.Mode)
			return err
		})
		if err != nil {
			return err
		}
		if !account.AutoEnabled || account.BalanceMicros >= account.ThresholdCents*10000 {
			return nil
		}
		included, err := s.Included(ctx, s.deps.Organization, s.deps.Now())
		if err != nil {
			return err
		}
		if included.Remaining() > 0 {
			return nil
		}
		pack, ok := s.CreditPack(account.PriceID)
		if !ok {
			return nil
		}
		purchase = billingstore.CreditPurchase{OrganizationID: s.deps.Organization, Key: "detent-autofund-" + s.deps.NewID(), Mode: s.deps.Mode, PriceID: pack.PriceID, USDCents: pack.USDCents, Automatic: true, PaymentMethod: account.PaymentMethod, State: "pending", CreatedAt: s.deps.Now().UTC()}
		if err := s.write(ctx, func(writer tenantstore.Writer) error { return writer.CreateBillingCreditPurchase(ctx, purchase) }); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if s.deps.Now().Sub(purchase.CreatedAt) >= 23*time.Hour {
		return errors.New("automatic credit payment needs billing review")
	}
	binding, err := s.deps.Binding(ctx)
	if err != nil {
		return err
	}
	provider, ok := cfg.Provider, cfg.Provider != nil
	if !ok {
		return errors.New("AI credit purchases are unavailable")
	}
	payment, chargeErr := provider.CreditCharge(ctx, billing.CreditRequest{CheckoutRequest: billing.CheckoutRequest{Binding: binding, PriceID: purchase.PriceID, IdempotencyKey: purchase.Key}, USDCents: purchase.USDCents}, purchase.PaymentMethod)
	if chargeErr != nil && !errors.Is(chargeErr, billing.ErrPaymentFailed) {
		return chargeErr
	}
	if chargeErr != nil {
		return s.write(ctx, func(writer tenantstore.Writer) error {
			account, err := writer.BillingCreditAccount(ctx, s.deps.Mode)
			if err != nil {
				return err
			}
			transitioned, err := writer.MarkBillingCreditPurchaseFailed(ctx, purchase.Key)
			if err != nil {
				return err
			}
			if !transitioned {
				return nil
			}
			account.AutoEnabled = false
			account.Failure = billing.ErrPaymentFailed.Error()
			return writer.SetBillingCreditFunding(ctx, account)
		})
	}
	return s.write(ctx, func(writer tenantstore.Writer) error {
		return s.ApplyPayment(ctx, writer, payment)
	})
}
