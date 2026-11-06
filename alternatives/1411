package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

type aiCreditTransaction struct {
	IncludedAmountMicros int64     `json:"included_amount_micros"`
	AmountMicros         int64     `json:"amount_micros"`
	Kind                 string    `json:"kind"`
	Actor                string    `json:"actor,omitempty"`
	Reason               string    `json:"reason,omitempty"`
	At                   time.Time `json:"at"`
	Turns                int64     `json:"turns"`
}

type aiCreditView struct {
	Included       *includedAICredits    `json:"included,omitempty"`
	BalanceMicros  int64                 `json:"balance_micros"`
	AutoEnabled    bool                  `json:"auto_enabled"`
	ThresholdCents int64                 `json:"threshold_cents"`
	PriceID        string                `json:"price_id"`
	Failure        string                `json:"failure"`
	InFlight       bool                  `json:"in_flight"`
	CanAutoFund    bool                  `json:"can_auto_fund"`
	Packs          []HostedCreditPack    `json:"packs"`
	History        []aiCreditTransaction `json:"history"`
	NextCursor     string                `json:"next_cursor"`
	TotalRows      int64                 `json:"total_rows"`
}

func (s *Service) readAICredits(ctx context.Context, pages ...creditPageRequest) (*aiCreditView, error) {
	ctx, cancel := context.WithTimeout(ctx, hubReadTimeout)
	defer cancel()
	d := s.database
	view := &aiCreditView{Packs: []HostedCreditPack{}, History: []aiCreditTransaction{}}
	var method string
	var err error

	err = d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		account, err := reader.BillingCreditAccount(ctx, d.aiCreditMode)
		if err != nil {
			return err
		}
		view.BalanceMicros = account.BalanceMicros
		view.AutoEnabled = account.AutoEnabled
		view.ThresholdCents = account.ThresholdCents
		view.PriceID = account.PriceID
		view.Failure = account.Failure
		method = account.PaymentMethod
		_, pendingErr := reader.BillingPendingCreditPurchase(ctx, d.aiCreditMode, true)
		if pendingErr == nil {
			view.InFlight = true
			return nil
		}
		if !errors.Is(pendingErr, corestore.ErrNotFound) {
			return pendingErr
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	included, err := d.includedAICredits(ctx, nil, d.hostedOrganization, s.config.now())
	if err != nil {
		return nil, err
	}
	if included.AllowanceMicros > 0 {
		view.Included = &included
	}
	view.CanAutoFund = method != ""
	if cfg := s.hostedBillingConfig(); cfg != nil {
		view.Packs = append(view.Packs, cfg.CreditPacks...)
	}
	page := creditPageRequest{}
	if len(pages) > 0 {
		page = pages[0]
	}
	if err := s.readCreditHistory(ctx, view, page); err != nil {
		return nil, err
	}
	return view, nil
}

func (s *Service) creditPack(id string) (HostedCreditPack, bool) {
	if cfg := s.hostedBillingConfig(); cfg != nil && !cfg.CheckoutDisabled {
		for _, pack := range cfg.CreditPacks {
			if pack.PriceID == id {
				return pack, true
			}
		}
	}
	return HostedCreditPack{}, false
}

func (s *Service) hostedCreditCheckout(c echo.Context) error {
	api := hostedBillingAPI(c)
	if _, err := s.hostedBillingOwner(c.Request().Context(), c); err != nil {
		return s.creditCommandFailure(c, api, err)
	}
	var request struct {
		hostedIdempotent
		Price string `json:"price"`
	}
	if api {
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
		if err := request.validate(true); err != nil {
			return s.nativeAPIError(c, err)
		}
	} else {
		request.Price = c.FormValue("price")
	}
	authorize := func(ctx context.Context) (apiCredential, error) { return s.hostedBillingOwner(ctx, c) }
	result, err := s.checkoutCredits(c.Request().Context(), authorize, request.Price, request.IdempotencyKey)
	if err != nil {
		return s.creditCommandFailure(c, api, err)
	}
	return s.hostedBillingDestination(c, api, result.URL)
}

func (s *Service) hostedCreditAutoFund(c echo.Context) error {
	api := hostedBillingAPI(c)
	if _, err := s.hostedBillingOwner(c.Request().Context(), c); err != nil {
		return s.creditCommandFailure(c, api, err)
	}
	var request creditFundingInput
	if api {
		if err := decodeAPIJSON(c, &request); err != nil {
			return s.invalidAPIRequest(c, err)
		}
	} else {
		request.Enabled = c.FormValue("enabled") == "on"
		request.Price = c.FormValue("price")
		threshold, err := strconv.ParseInt(c.FormValue("threshold_cents"), 10, 64)
		if err != nil && request.Enabled {
			return s.hostedBillingFailure(c, api, http.StatusBadRequest, "Enter a threshold in USD cents")
		}
		request.Threshold = threshold
	}
	authorize := func(ctx context.Context) (apiCredential, error) { return s.hostedBillingOwner(ctx, c) }
	if _, err := s.configureCreditFunding(c.Request().Context(), authorize, request, ""); err != nil {
		return s.creditCommandFailure(c, api, err)
	}
	if api {
		return c.NoContent(http.StatusNoContent)
	}
	return c.Redirect(http.StatusSeeOther, s.hostedBillingReturn(true))
}

func (s *Service) creditCommandFailure(c echo.Context, api bool, err error) error {
	s.logStripeFailure(c.Request().Context(), err)
	var failure *nativeError
	switch {
	case errors.Is(err, auth.ErrHostedIdentity):
		return s.hostedBillingFailure(c, api, http.StatusForbidden, "AI credits require an organization owner without support impersonation")
	case errors.Is(err, mutation.ErrConflict), errors.Is(err, mutation.ErrUncertain):
		return s.hostedBillingFailure(c, api, http.StatusConflict, err.Error())
	case errors.As(err, &failure):
		return s.hostedBillingFailure(c, api, failure.status, failure.Message)
	default:
		return s.nativeAPIError(c, err)
	}
}

func (d *database) applyCreditPayment(ctx context.Context, tx *sql.Tx, payment billing.CreditPayment) error {
	if payment.Key == "" {
		return nil
	}
	var price, mode, state, organization string
	var cents int64
	var automatic bool
	err := tx.QueryRowContext(ctx, "SELECT organization_id,mode,price_id,usd_cents,automatic,state FROM ai_credit_purchases WHERE purchase_key=?", payment.Key).Scan(&organization, &mode, &price, &cents, &automatic, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if organization != string(d.hostedOrganization) || mode != d.aiCreditMode || price != payment.PriceID || cents != payment.USDCents {
		return errors.New("AI credit purchase mismatch")
	}
	if state == "paid" || state == "failed" && payment.Status != "succeeded" {
		return nil
	}
	if payment.Status == "succeeded" {
		kind := "purchase"
		if automatic {
			kind = "auto_fund"
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,?,?,?,?,?)", organization, mode, "purchase:"+payment.Key, cents*10000, kind, d.now().UnixMicro()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE ai_credit_accounts SET balance_micros=balance_micros+?,payment_method=CASE WHEN ?<>'' THEN ? ELSE payment_method END WHERE organization_id=? AND mode=?", cents*10000, payment.PaymentMethod, payment.PaymentMethod, organization, mode); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE ai_credit_purchases SET state='paid',payment_id=? WHERE purchase_key=?", payment.ID, payment.Key)
		return err
	}
	if automatic && (payment.Status == "requires_payment_method" || payment.Status == "requires_action" || payment.Status == "canceled") {
		return d.failCreditAutoFund(ctx, tx, payment.Key)
	}
	return nil
}

func (d *database) applyCreditPaymentRecord(ctx context.Context, writer billingstore.Writer, payment billing.CreditPayment) error {
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
	if purchase.OrganizationID != d.hostedOrganization || purchase.Mode != d.aiCreditMode || purchase.PriceID != payment.PriceID || purchase.USDCents != payment.USDCents {
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
		if _, err := writer.PostBillingCredit(ctx, billingstore.CreditTransaction{OrganizationID: d.hostedOrganization, Mode: purchase.Mode, Source: "purchase:" + purchase.Key, AmountMicros: purchase.USDCents * 10000, Kind: kind, RecordedAt: d.now().UTC()}); err != nil {
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

func (d *database) failCreditAutoFund(ctx context.Context, tx *sql.Tx, key string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE ai_credit_purchases SET state='failed' WHERE purchase_key=? AND state='pending'", key); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE ai_credit_accounts SET auto_enabled=0,failure=? WHERE organization_id=? AND mode=?", billing.ErrPaymentFailed.Error(), d.hostedOrganization, d.aiCreditMode)
	return err
}

func (w *hostedBillingWorker) creditEvent(ctx context.Context, id, kind string) error {
	s := w.service
	cfg := s.hostedBillingConfig()
	if cfg == nil || s.database.aiCreditMode == "" || (kind != "checkout.session.completed" && kind != "checkout.session.async_payment_succeeded" && kind != "payment_intent.succeeded" && kind != "payment_intent.payment_failed" && kind != "payment_intent.canceled") {
		return nil
	}
	binding, err := s.database.hostedBillingBinding(ctx, cfg)
	if errors.Is(err, errHostedBillingUnbound) {
		return nil
	}
	if err != nil {
		return err
	}
	provider, ok := cfg.Provider.(billing.CreditProvider)
	if !ok {
		return errors.New("AI credit purchases are unavailable")
	}
	payment, err := provider.CreditEvent(ctx, binding, id)
	if err != nil {
		return err
	}

	return s.database.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		event, err := writer.BillingEvent(ctx, id)
		if err != nil {
			return err
		}
		if event.CreditProcessed {
			return nil
		}
		if err := s.database.applyCreditPaymentRecord(ctx, writer, payment); err != nil {
			return err
		}
		return writer.SetBillingEventCreditProcessed(ctx, id)
	})
}

func (w *hostedBillingWorker) autoFund(ctx context.Context) error {
	s := w.service
	d := s.database
	cfg := s.hostedBillingConfig()
	if cfg == nil || d.aiCreditMode == "" {
		return nil
	}

	return w.autoFundRecord(ctx, cfg)
}

func (w *hostedBillingWorker) autoFundRecord(ctx context.Context, cfg *HostedBillingConfig) error {
	s := w.service
	d := s.database
	var purchase billingstore.CreditPurchase
	err := d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
		var err error
		purchase, err = reader.BillingPendingCreditPurchase(ctx, d.aiCreditMode, true)
		return err
	})
	if errors.Is(err, corestore.ErrNotFound) {
		var account billingstore.CreditAccount
		err = d.readBillingTenant(ctx, func(reader tenantstore.Reader) error {
			var err error
			account, err = reader.BillingCreditAccount(ctx, d.aiCreditMode)
			return err
		})
		if err != nil {
			return err
		}
		if !account.AutoEnabled || account.BalanceMicros >= account.ThresholdCents*10000 {
			return nil
		}
		included, err := d.includedAICreditsRecord(ctx, d.hostedOrganization, s.config.now())
		if err != nil {
			return err
		}
		if included.remaining() > 0 {
			return nil
		}
		pack, ok := s.creditPack(account.PriceID)
		if !ok {
			return nil
		}
		purchase = billingstore.CreditPurchase{OrganizationID: d.hostedOrganization, Key: "detent-autofund-" + s.config.newLeaseID(), Mode: d.aiCreditMode, PriceID: pack.PriceID, USDCents: pack.USDCents, Automatic: true, PaymentMethod: account.PaymentMethod, State: "pending", CreatedAt: s.config.now().UTC()}
		if err := d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error { return writer.CreateBillingCreditPurchase(ctx, purchase) }); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if s.config.now().Sub(purchase.CreatedAt) >= 23*time.Hour {
		return errors.New("automatic credit payment needs billing review")
	}
	binding, err := d.hostedBillingBinding(ctx, cfg)
	if err != nil {
		return err
	}
	provider, ok := cfg.Provider.(billing.CreditProvider)
	if !ok {
		return errors.New("AI credit purchases are unavailable")
	}
	payment, chargeErr := provider.CreditCharge(ctx, billing.CreditRequest{CheckoutRequest: billing.CheckoutRequest{Binding: binding, PriceID: purchase.PriceID, IdempotencyKey: purchase.Key}, USDCents: purchase.USDCents}, purchase.PaymentMethod)
	if chargeErr != nil && !errors.Is(chargeErr, billing.ErrPaymentFailed) {
		return chargeErr
	}
	if chargeErr != nil {
		return d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
			account, err := writer.BillingCreditAccount(ctx, d.aiCreditMode)
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
	return d.writeBillingTenant(ctx, func(writer tenantstore.Writer) error {
		return d.applyCreditPaymentRecord(ctx, writer, payment)
	})
}
