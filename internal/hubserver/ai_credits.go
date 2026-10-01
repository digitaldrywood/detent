package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/labstack/echo/v4"
)

type aiCreditTransaction struct {
	AmountMicros int64     `json:"amount_micros"`
	Kind         string    `json:"kind"`
	At           time.Time `json:"at"`
}

type aiCreditView struct {
	BalanceMicros  int64                 `json:"balance_micros"`
	AutoEnabled    bool                  `json:"auto_enabled"`
	ThresholdCents int64                 `json:"threshold_cents"`
	PriceID        string                `json:"price_id"`
	Failure        string                `json:"failure"`
	InFlight       bool                  `json:"in_flight"`
	CanAutoFund    bool                  `json:"can_auto_fund"`
	Packs          []HostedCreditPack    `json:"packs"`
	History        []aiCreditTransaction `json:"history"`
}

func (s *Service) readAICredits(ctx context.Context) (*aiCreditView, error) {
	d := s.database
	if d.aiCreditMode == "" {
		return nil, nil
	}
	view := &aiCreditView{Packs: []HostedCreditPack{}, History: []aiCreditTransaction{}}
	var method string
	err := d.db.QueryRowContext(ctx, `SELECT balance_micros,auto_enabled,threshold_cents,price_id,failure,payment_method,
 EXISTS(SELECT 1 FROM ai_credit_purchases p WHERE p.organization_id=a.organization_id AND p.mode=a.mode AND p.automatic=1 AND p.state='pending')
 FROM ai_credit_accounts a WHERE organization_id=? AND mode=?`, d.hostedOrganization, d.aiCreditMode).Scan(&view.BalanceMicros, &view.AutoEnabled, &view.ThresholdCents, &view.PriceID, &view.Failure, &method, &view.InFlight)
	if err != nil {
		return nil, err
	}
	view.CanAutoFund = method != ""
	view.Packs = append(view.Packs, s.config.Hosted.Billing.CreditPacks...)
	rows, err := d.db.QueryContext(ctx, "SELECT amount_micros,kind,recorded_at FROM ai_credit_transactions WHERE organization_id=? AND mode=? ORDER BY id DESC LIMIT 50", d.hostedOrganization, d.aiCreditMode)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item aiCreditTransaction
		var at int64
		if err := rows.Scan(&item.AmountMicros, &item.Kind, &at); err != nil {
			return nil, err
		}
		item.At = time.UnixMicro(at).UTC()
		view.History = append(view.History, item)
	}
	return view, rows.Err()
}

func (s *Service) creditPack(id string) (HostedCreditPack, bool) {
	if s.config.Hosted != nil && s.config.Hosted.Billing != nil && !s.config.Hosted.Billing.CheckoutDisabled {
		for _, pack := range s.config.Hosted.Billing.CreditPacks {
			if pack.PriceID == id {
				return pack, true
			}
		}
	}
	return HostedCreditPack{}, false
}

func (s *Service) hostedCreditCheckout(c echo.Context) error {
	api := hostedBillingAPI(c)
	credential, err := s.hostedBillingOwner(c)
	if err != nil {
		return s.hostedBillingFailure(c, api, http.StatusForbidden, "AI credits require an organization owner")
	}
	var request struct {
		hostedIdempotent
		Price string `json:"price"`
	}
	if api {
		if err := decodeAPIJSON(c, &request); err != nil {
			return invalidAPIRequest(c, err)
		}
		if err := request.validate(true); err != nil {
			return s.nativeAPIError(c, err)
		}
	} else {
		request.Price = c.FormValue("price")
	}
	pack, ok := s.creditPack(request.Price)
	if !ok {
		return s.hostedBillingFailure(c, api, http.StatusBadRequest, "Choose a configured AI credit pack")
	}
	w := s.billing
	if w == nil {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "Billing is unavailable")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	ctx := c.Request().Context()
	binding, err := s.ensureHostedCustomer(ctx, credential.Hosted.Subject)
	if err != nil {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "Billing is temporarily unavailable")
	}
	key := "detent-credit-" + s.config.newLeaseID()
	if api {
		digest := sha256.Sum256([]byte(s.config.Hosted.OrganizationID + "\x00" + s.config.Hosted.Billing.mode() + "\x00" + request.IdempotencyKey))
		key = fmt.Sprintf("detent-credit-%x", digest)
	}
	var raw string
	var cents, at int64
	var price, mode, state string
	err = s.database.db.QueryRowContext(ctx, "SELECT price_id,usd_cents,created_at,session_json,mode,state FROM ai_credit_purchases WHERE purchase_key=? AND organization_id=? AND automatic=0", key, binding.OrganizationID).Scan(&price, &cents, &at, &raw, &mode, &state)
	if errors.Is(err, sql.ErrNoRows) {
		price, cents, at, mode, state = pack.PriceID, pack.USDCents, s.config.now().UnixMicro(), s.config.Hosted.Billing.mode(), "pending"
		_, err = s.database.db.ExecContext(ctx, "INSERT INTO ai_credit_purchases(purchase_key,organization_id,mode,price_id,usd_cents,automatic,created_at) VALUES(?,?,?,?,?,0,?)", key, binding.OrganizationID, mode, price, cents, at)
		raw = "{}"
	}
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if price != pack.PriceID || cents != pack.USDCents || mode != s.config.Hosted.Billing.mode() {
		return s.hostedBillingFailure(c, api, http.StatusConflict, "The purchase key already refers to another credit pack")
	}
	var session billing.Session
	if err := json.Unmarshal([]byte(raw), &session); err != nil {
		return s.nativeAPIError(c, err)
	}
	if session.URL == "" {
		if state != "pending" || s.config.now().Sub(time.UnixMicro(at)) >= 23*time.Hour {
			return s.hostedBillingFailure(c, api, http.StatusConflict, "This purchase needs billing review")
		}
		session, err = s.config.Hosted.Billing.Provider.(billing.CreditProvider).CreditCheckout(ctx, billing.CreditRequest{CheckoutRequest: billing.CheckoutRequest{Binding: binding, PriceID: price, IdempotencyKey: key, ReturnURL: s.hostedBillingReturn(true), ExpiresAt: time.UnixMicro(at).Truncate(time.Second).Add(time.Hour)}, USDCents: cents})
		if err != nil {
			return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "Credit checkout is temporarily unavailable; retry the same purchase")
		}
		encoded, err := json.Marshal(session)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		if _, err := s.database.db.ExecContext(ctx, "UPDATE ai_credit_purchases SET session_json=? WHERE purchase_key=?", string(encoded), key); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	return s.hostedBillingDestination(c, api, session.URL)
}

func (s *Service) hostedCreditAutoFund(c echo.Context) error {
	api := hostedBillingAPI(c)
	if _, err := s.hostedBillingOwner(c); err != nil {
		return s.hostedBillingFailure(c, api, http.StatusForbidden, "Auto-fund requires an organization owner")
	}
	var request struct {
		Enabled   bool   `json:"enabled"`
		Threshold int64  `json:"threshold_cents"`
		Price     string `json:"price"`
	}
	if api {
		if err := decodeAPIJSON(c, &request); err != nil {
			return invalidAPIRequest(c, err)
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
	if s.database.aiCreditMode == "" {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "AI credit purchases are unavailable")
	}
	if request.Enabled {
		pack, ok := s.creditPack(request.Price)
		if !ok || request.Threshold <= 0 || request.Threshold >= pack.USDCents {
			return s.hostedBillingFailure(c, api, http.StatusBadRequest, "Choose a threshold greater than zero and smaller than the credit pack")
		}
	}
	if s.billing == nil {
		return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "Billing is unavailable")
	}
	s.billing.mu.Lock()
	defer s.billing.mu.Unlock()
	if request.Enabled {
		ctx := c.Request().Context()
		var method string
		if err := s.database.db.QueryRowContext(ctx, "SELECT payment_method FROM ai_credit_accounts WHERE organization_id=? AND mode=?", s.database.hostedOrganization, s.database.aiCreditMode).Scan(&method); err != nil {
			return s.nativeAPIError(c, err)
		}
		binding, err := s.database.hostedBillingBinding(ctx, s.config.Hosted.Billing)
		if err != nil {
			return s.hostedBillingFailure(c, api, http.StatusConflict, "Buy credits or save a payment method in the billing portal first")
		}
		current, err := s.config.Hosted.Billing.Provider.(billing.CreditProvider).SavedCreditPaymentMethod(ctx, binding)
		if err != nil && !errors.Is(err, billing.ErrPaymentFailed) {
			return s.hostedBillingFailure(c, api, http.StatusServiceUnavailable, "The saved payment method is temporarily unavailable")
		}
		if current != "" {
			method = current
		}
		if method == "" {
			return s.hostedBillingFailure(c, api, http.StatusConflict, "Buy credits or save a payment method in the billing portal first")
		}
		if _, err := s.database.db.ExecContext(ctx, "UPDATE ai_credit_accounts SET payment_method=? WHERE organization_id=? AND mode=?", method, s.database.hostedOrganization, s.database.aiCreditMode); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	result, err := s.database.db.ExecContext(c.Request().Context(), `UPDATE ai_credit_accounts SET auto_enabled=?,threshold_cents=?,price_id=?,failure='' WHERE organization_id=? AND mode=? AND (?=0 OR payment_method<>'')`, request.Enabled, max(request.Threshold, 0), request.Price, s.database.hostedOrganization, s.database.aiCreditMode, request.Enabled)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return s.hostedBillingFailure(c, api, http.StatusConflict, "Buy credits or save a payment method in the billing portal first")
	}
	if api {
		return c.NoContent(http.StatusNoContent)
	}
	return c.Redirect(http.StatusSeeOther, s.hostedBillingReturn(true))
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

func (d *database) failCreditAutoFund(ctx context.Context, tx *sql.Tx, key string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE ai_credit_purchases SET state='failed' WHERE purchase_key=? AND state='pending'", key); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "UPDATE ai_credit_accounts SET auto_enabled=0,failure=? WHERE organization_id=? AND mode=?", billing.ErrPaymentFailed.Error(), d.hostedOrganization, d.aiCreditMode)
	return err
}

func (w *hostedBillingWorker) creditEvent(ctx context.Context, id, kind string) error {
	s := w.service
	if s.database.aiCreditMode == "" || !(kind == "checkout.session.completed" || kind == "checkout.session.async_payment_succeeded" || kind == "payment_intent.succeeded" || kind == "payment_intent.payment_failed" || kind == "payment_intent.canceled") {
		return nil
	}
	binding, err := s.database.hostedBillingBinding(ctx, s.config.Hosted.Billing)
	if errors.Is(err, errHostedBillingUnbound) {
		return nil
	}
	if err != nil {
		return err
	}
	payment, err := s.config.Hosted.Billing.Provider.(billing.CreditProvider).CreditEvent(ctx, binding, id)
	if err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var processed bool
	if err := tx.QueryRowContext(ctx, "SELECT credit_processed FROM hosted_billing_events WHERE event_id=?", id).Scan(&processed); err != nil {
		return err
	}
	if processed {
		return nil
	}
	if err := s.database.applyCreditPayment(ctx, tx, payment); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE hosted_billing_events SET credit_processed=1 WHERE event_id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (w *hostedBillingWorker) autoFund(ctx context.Context) error {
	s := w.service
	d := s.database
	if d.aiCreditMode == "" {
		return nil
	}
	var key, price, method string
	var cents, at int64
	err := d.db.QueryRowContext(ctx, "SELECT purchase_key,price_id,usd_cents,payment_method,created_at FROM ai_credit_purchases WHERE organization_id=? AND mode=? AND automatic=1 AND state='pending'", d.hostedOrganization, d.aiCreditMode).Scan(&key, &price, &cents, &method, &at)
	if errors.Is(err, sql.ErrNoRows) {
		var enabled bool
		var balance, threshold int64
		if err := d.db.QueryRowContext(ctx, "SELECT auto_enabled,balance_micros,threshold_cents,price_id,payment_method FROM ai_credit_accounts WHERE organization_id=? AND mode=?", d.hostedOrganization, d.aiCreditMode).Scan(&enabled, &balance, &threshold, &price, &method); err != nil {
			return err
		}
		if !enabled || balance >= threshold*10000 {
			return nil
		}
		pack, ok := s.creditPack(price)
		if !ok {
			return nil
		}
		key, cents, at = "detent-autofund-"+s.config.newLeaseID(), pack.USDCents, s.config.now().UnixMicro()
		var inserted sql.Result
		inserted, err = d.db.ExecContext(ctx, `INSERT INTO ai_credit_purchases(purchase_key,organization_id,mode,price_id,usd_cents,automatic,payment_method,created_at)
 SELECT ?,organization_id,mode,?,?,1,payment_method,? FROM ai_credit_accounts WHERE organization_id=? AND mode=? AND auto_enabled=1 AND balance_micros<threshold_cents*10000`, key, price, cents, at, d.hostedOrganization, d.aiCreditMode)
		if err == nil {
			n, e := inserted.RowsAffected()
			if e != nil {
				return e
			}
			if n == 0 {
				return nil
			}
		}
	}
	if err != nil {
		return err
	}
	if s.config.now().Sub(time.UnixMicro(at)) >= 23*time.Hour {
		return errors.New("automatic credit payment needs billing review")
	}
	binding, err := d.hostedBillingBinding(ctx, s.config.Hosted.Billing)
	if err != nil {
		return err
	}
	payment, chargeErr := s.config.Hosted.Billing.Provider.(billing.CreditProvider).CreditCharge(ctx, billing.CreditRequest{CheckoutRequest: billing.CheckoutRequest{Binding: binding, PriceID: price, IdempotencyKey: key}, USDCents: cents}, method)
	if chargeErr != nil && !errors.Is(chargeErr, billing.ErrPaymentFailed) {
		return chargeErr
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if chargeErr != nil {
		err = d.failCreditAutoFund(ctx, tx, key)
	} else {
		err = d.applyCreditPayment(ctx, tx, payment)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
