package credits

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

type Transaction struct {
	IncludedAmountMicros int64     `json:"included_amount_micros"`
	AmountMicros         int64     `json:"amount_micros"`
	Kind                 string    `json:"kind"`
	Actor                string    `json:"actor,omitempty"`
	Reason               string    `json:"reason,omitempty"`
	At                   time.Time `json:"at"`
	Turns                int64     `json:"turns"`
}

type View struct {
	Included       *Included     `json:"included,omitempty"`
	BalanceMicros  int64         `json:"balance_micros"`
	AutoEnabled    bool          `json:"auto_enabled"`
	ThresholdCents int64         `json:"threshold_cents"`
	PriceID        string        `json:"price_id"`
	Failure        string        `json:"failure"`
	InFlight       bool          `json:"in_flight"`
	CanAutoFund    bool          `json:"can_auto_fund"`
	Packs          []Pack        `json:"packs"`
	History        []Transaction `json:"history"`
	NextCursor     string        `json:"next_cursor"`
	TotalRows      int64         `json:"total_rows"`
}

func (s *Service) Read(ctx context.Context, pages ...Page) (*View, error) {
	ctx, cancel := context.WithTimeout(ctx, hubapi.ReadTimeout)
	defer cancel()
	view := &View{Packs: []Pack{}, History: []Transaction{}}
	var method string
	err := s.read(ctx, func(reader tenantstore.Reader) error {
		account, err := reader.BillingCreditAccount(ctx, s.deps.Mode)
		if err != nil {
			return err
		}
		view.BalanceMicros = account.BalanceMicros
		view.AutoEnabled = account.AutoEnabled
		view.ThresholdCents = account.ThresholdCents
		view.PriceID = account.PriceID
		view.Failure = account.Failure
		method = account.PaymentMethod
		_, pendingErr := reader.BillingPendingCreditPurchase(ctx, s.deps.Mode, true)
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
	if s.deps.ReadIncluded {
		included, err := s.Included(ctx, s.deps.Organization, s.deps.Now())
		if err != nil {
			return nil, err
		}
		if included.AllowanceMicros > 0 {
			view.Included = &included
		}
	}
	view.CanAutoFund = method != ""
	view.Packs = append(view.Packs, s.deps.Packs...)
	page := Page{}
	if len(pages) > 0 {
		page = pages[0]
	}
	if err := s.History(ctx, view, page); err != nil {
		return nil, err
	}
	return view, nil
}
