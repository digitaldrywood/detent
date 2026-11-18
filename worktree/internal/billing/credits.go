package billing

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var ErrPaymentFailed = errors.New("automatic payment failed; update the saved payment method")

type CreditRequest struct {
	CheckoutRequest
	USDCents int64
}

type CreditPayment struct {
	Key           string
	ID            string
	PriceID       string
	USDCents      int64
	Status        string
	PaymentMethod string
}

type CreditProvider interface {
	SavedCreditPaymentMethod(context.Context, Binding) (string, error)
	CreditCheckout(context.Context, CreditRequest) (Session, error)
	CreditCharge(context.Context, CreditRequest, string) (CreditPayment, error)
	CreditEvent(context.Context, Binding, string) (CreditPayment, error)
}

func (s *stripeProvider) SavedCreditPaymentMethod(ctx context.Context, binding Binding) (string, error) {
	if err := s.verifyBinding(ctx, binding); err != nil {
		return "", err
	}
	var customer struct {
		InvoiceSettings struct {
			DefaultPaymentMethod string `json:"default_payment_method"`
		} `json:"invoice_settings"`
	}
	if err := s.request(ctx, http.MethodGet, "customers/"+binding.CustomerID, "", nil, &customer); err != nil {
		return "", err
	}
	if !validID(customer.InvoiceSettings.DefaultPaymentMethod, "pm_") {
		return "", ErrPaymentFailed
	}
	return customer.InvoiceSettings.DefaultPaymentMethod, nil
}

func (s *stripeProvider) creditPrice(ctx context.Context, request CreditRequest) error {
	if err := s.verifyBinding(ctx, request.Binding); err != nil {
		return err
	}
	if !validID(request.PriceID, "price_") || request.IdempotencyKey == "" || request.USDCents <= 0 {
		return errors.New("invalid AI credit pack")
	}
	var price struct {
		stripePrice
		Currency   string
		UnitAmount int64 `json:"unit_amount"`
	}
	if err := s.request(ctx, http.MethodGet, "prices/"+request.PriceID, "", nil, &price); err != nil {
		return err
	}
	if price.ID != request.PriceID || !s.mode(price.Livemode) || !price.Active || price.Type != "one_time" || price.BillingScheme != "per_unit" || price.Currency != "usd" || price.UnitAmount != request.USDCents {
		return errors.New("AI credit pack requires a matching active USD one-time price")
	}
	return nil
}

func creditMetadata(form url.Values, prefix string, request CreditRequest) {
	form.Set(prefix+"[detent_organization_id]", request.OrganizationID)
	form.Set(prefix+"[detent_credit_key]", request.IdempotencyKey)
	form.Set(prefix+"[detent_credit_price]", request.PriceID)
	form.Set(prefix+"[detent_credit_cents]", strconv.FormatInt(request.USDCents, 10))
}

func (s *stripeProvider) CreditCheckout(ctx context.Context, request CreditRequest) (Session, error) {
	if err := s.creditPrice(ctx, request); err != nil {
		return Session{}, err
	}
	form := url.Values{
		"mode": {"payment"}, "customer": {request.CustomerID}, "client_reference_id": {request.OrganizationID},
		"line_items[0][price]": {request.PriceID}, "line_items[0][quantity]": {"1"},
		"success_url": {request.ReturnURL + "?credits=returned"}, "cancel_url": {request.ReturnURL},
		"expires_at": {strconv.FormatInt(request.ExpiresAt.Unix(), 10)},
		"payment_intent_data[setup_future_usage]": {"off_session"},
		"payment_method_types[0]":                 {"card"},
	}
	creditMetadata(form, "metadata", request)
	creditMetadata(form, "payment_intent_data[metadata]", request)
	var response struct {
		ID, URL, Customer string
		Livemode          *bool
		ExpiresAt         int64 `json:"expires_at"`
	}
	if err := s.request(ctx, http.MethodPost, "checkout/sessions", request.IdempotencyKey, form, &response); err != nil {
		return Session{}, err
	}
	if !validID(response.ID, s.sessionPrefix()) || !s.mode(response.Livemode) || response.Customer != request.CustomerID || !sessionURL(response.URL, "checkout.stripe.com") || response.ExpiresAt != request.ExpiresAt.Unix() {
		return Session{}, errors.New("invalid AI credit checkout response")
	}
	return Session{ID: response.ID, URL: response.URL, ExpiresAt: time.Unix(response.ExpiresAt, 0).UTC()}, nil
}

type stripeCreditIntent struct {
	ID, Customer, Currency, Status string
	Livemode                       *bool
	Amount                         int64
	AmountReceived                 int64  `json:"amount_received"`
	PaymentMethod                  string `json:"payment_method"`
	Metadata                       map[string]string
}

func (s *stripeProvider) creditPayment(binding Binding, intent stripeCreditIntent) (CreditPayment, error) {
	if !validID(intent.ID, "pi_") || !s.mode(intent.Livemode) || intent.Customer != binding.CustomerID || intent.Metadata["detent_organization_id"] != binding.OrganizationID {
		return CreditPayment{}, errors.New("AI credit payment binding mismatch")
	}
	cents, err := strconv.ParseInt(intent.Metadata["detent_credit_cents"], 10, 64)
	if err != nil || cents <= 0 || intent.Currency != "usd" || intent.Amount != cents || intent.Status == "succeeded" && intent.AmountReceived != cents {
		return CreditPayment{}, errors.New("AI credit payment amount mismatch")
	}
	return CreditPayment{Key: intent.Metadata["detent_credit_key"], ID: intent.ID, PriceID: intent.Metadata["detent_credit_price"], USDCents: cents, Status: intent.Status, PaymentMethod: intent.PaymentMethod}, nil
}

func (s *stripeProvider) CreditCharge(ctx context.Context, request CreditRequest, method string) (CreditPayment, error) {
	if err := s.creditPrice(ctx, request); err != nil {
		return CreditPayment{}, err
	}
	if !validID(method, "pm_") {
		return CreditPayment{}, ErrPaymentFailed
	}
	form := url.Values{"customer": {request.CustomerID}, "amount": {strconv.FormatInt(request.USDCents, 10)}, "currency": {"usd"}, "payment_method": {method}, "off_session": {"true"}, "confirm": {"true"}, "payment_method_types[0]": {"card"}}
	creditMetadata(form, "metadata", request)
	var intent stripeCreditIntent
	if err := s.request(ctx, http.MethodPost, "payment_intents", request.IdempotencyKey, form, &intent); err != nil {
		return CreditPayment{}, err
	}
	payment, err := s.creditPayment(request.Binding, intent)
	if err != nil {
		return CreditPayment{}, err
	}
	if payment.Key != request.IdempotencyKey || payment.PriceID != request.PriceID || payment.USDCents != request.USDCents {
		return CreditPayment{}, errors.New("automatic AI credit payment does not match the purchase")
	}
	return payment, nil
}

func (s *stripeProvider) CreditEvent(ctx context.Context, binding Binding, id string) (CreditPayment, error) {
	if err := s.verifyBinding(ctx, binding); err != nil {
		return CreditPayment{}, err
	}
	if !validID(id, "evt_") {
		return CreditPayment{}, errors.New("invalid AI credit event")
	}
	var event struct {
		ID, Type string
		Livemode *bool
		Data     struct {
			Object struct {
				stripeCreditIntent
				Mode          string
				PaymentStatus string `json:"payment_status"`
				PaymentIntent string `json:"payment_intent"`
			}
		}
	}
	if err := s.request(ctx, http.MethodGet, "events/"+id, "", nil, &event); err != nil {
		return CreditPayment{}, err
	}
	if event.ID != id || !s.mode(event.Livemode) {
		return CreditPayment{}, errors.New("AI credit event mode mismatch")
	}
	object := event.Data.Object
	if object.Metadata["detent_credit_key"] == "" {
		return CreditPayment{}, nil
	}
	if event.Type == "checkout.session.completed" || event.Type == "checkout.session.async_payment_succeeded" {
		if object.Mode != "payment" || object.PaymentStatus != "paid" {
			return CreditPayment{}, nil
		}
		if !validID(object.PaymentIntent, "pi_") {
			return CreditPayment{}, errors.New("invalid AI credit payment intent")
		}
		var intent stripeCreditIntent
		if err := s.request(ctx, http.MethodGet, "payment_intents/"+object.PaymentIntent, "", nil, &intent); err != nil {
			return CreditPayment{}, err
		}
		if intent.Status != "succeeded" {
			return CreditPayment{}, nil
		}
		return s.creditPayment(binding, intent)
	}
	if event.Type == "payment_intent.succeeded" || event.Type == "payment_intent.payment_failed" || event.Type == "payment_intent.canceled" {
		return s.creditPayment(binding, object.stripeCreditIntent)
	}
	return CreditPayment{}, nil
}
