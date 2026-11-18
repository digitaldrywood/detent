package credits

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
)

func (s *Service) ApplyPayment(ctx context.Context, writer billingstore.Writer, payment billing.CreditPayment) error {
	if payment.Key == "" {
		return nil
	}
	purchase, err := writer.BillingCreditPurchase(ctx, payment.Key)
	if errors.Is(err, corestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if purchase.OrganizationID != s.deps.Organization || purchase.Mode != s.deps.Mode || purchase.PriceID != payment.PriceID || purchase.USDCents != payment.USDCents {
		return errors.New("AI credit purchase mismatch")
	}
	if purchase.State == "paid" || purchase.State == "failed" && payment.Status != "succeeded" {
		return nil
	}
	if payment.Status == "succeeded" {
		kind := "purchase"
		if purchase.Automatic {
			kind = "auto_fund"
		}
		if _, err := writer.PostBillingCredit(ctx, billingstore.CreditTransaction{OrganizationID: s.deps.Organization, Mode: purchase.Mode, Source: "purchase:" + purchase.Key, AmountMicros: purchase.USDCents * 10000, Kind: kind, RecordedAt: s.deps.PaymentNow().UTC()}); err != nil {
			return err
		}
		if err := writer.MarkBillingCreditPurchasePaid(ctx, purchase.Key, payment.ID); err != nil {
			return err
		}
		if payment.PaymentMethod != "" {
			account, err := writer.BillingCreditAccount(ctx, purchase.Mode)
			if err != nil {
				return err
			}
			account.PaymentMethod = payment.PaymentMethod
			return writer.SetBillingCreditFunding(ctx, account)
		}
		return nil
	}
	if purchase.Automatic && (payment.Status == "requires_payment_method" || payment.Status == "requires_action" || payment.Status == "canceled") {
		account, err := writer.BillingCreditAccount(ctx, purchase.Mode)
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
		account.AutoEnabled, account.Failure = false, billing.ErrPaymentFailed.Error()
		return writer.SetBillingCreditFunding(ctx, account)
	}
	return nil
}
