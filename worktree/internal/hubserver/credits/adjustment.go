package credits

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func (s *Service) Adjust(ctx context.Context, actor string, adjustment billing.CreditAdjustment) (int64, error) {
	amount, err := adjustment.AmountMicros()
	if err != nil {
		return 0, hubapi.Invalid(err.Error())
	}
	adjustment.Reason = strings.TrimSpace(adjustment.Reason)
	if !s.deps.SafeID(adjustment.IdempotencyKey) || adjustment.Reason == "" || len(adjustment.Reason) > 500 || actor == "" || len(actor) > 254 {
		return 0, hubapi.PublicInvalid("A command ID, actor and reason of at most 500 characters are required")
	}
	if s.deps.Mode == "" {
		return 0, hubapi.PublicInvalid("AI credits are not configured for this organization")
	}

	reference, err := s.deps.ArchiveReason(ctx, adjustment.Reason)
	if err != nil {
		return 0, err
	}
	var balance int64
	err = s.write(ctx, func(writer tenantstore.Writer) error {
		account, err := writer.BillingCreditAccount(ctx, s.deps.Mode)
		if err != nil {
			return err
		}
		balance = account.BalanceMicros
		source := "adjustment:" + adjustment.IdempotencyKey
		previous, err := writer.BillingCreditTransactionBySource(ctx, s.deps.Mode, source)
		if err == nil {
			if previous.AmountMicros != amount || previous.Actor != actor || previous.ReasonReference.SHA256 != reference.SHA256 {
				return &hubapi.Error{Status: http.StatusConflict, Code: "idempotency_conflict", Message: "That idempotency key was used for a different credit adjustment", Public: true}
			}
			return nil
		}
		if !errors.Is(err, corestore.ErrNotFound) {
			return err
		}
		if amount > 0 && balance > math.MaxInt64-amount || amount < 0 && balance < math.MinInt64-amount {
			return hubapi.PublicInvalid("The adjusted balance is out of range")
		}
		_, err = writer.PostBillingCredit(ctx, billingstore.CreditTransaction{OrganizationID: s.deps.Organization, Mode: s.deps.Mode, Source: source, AmountMicros: amount, Kind: "complimentary", Actor: actor, ReasonReference: reference, RecordedAt: s.deps.Now().UTC()})
		if err == nil {
			balance += amount
		}
		return err
	})
	return balance, err
}
